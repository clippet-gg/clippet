package config

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The Discord bot's three variables (ROADMAP #33, DESIGN.md §20 errata
// 253-255). They travel together: all three unset is the OFF state — no route,
// no client, no command registration, nothing anywhere — and any one of them
// set without the others is fatal at config.Load, for the reason erratum 63
// gives for the webhook. A half-configured integration that boots is an
// integration that is silently dead, and silence is the worst way for this
// feature to fail: the operator has already pasted an endpoint URL into
// Discord's portal by the time they find out.
const (
	// EnvDiscordAppID is the application's snowflake. NOT a secret: it is in
	// every interaction Discord sends and in the follow-up URL.
	EnvDiscordAppID = "DISCORD_APP_ID"
	// EnvDiscordPublicKey is the application's Ed25519 verify key, 64 hex
	// characters. NOT a secret either — it verifies signatures and cannot
	// make them, which is the whole point of asking for a public key rather
	// than for a shared one.
	EnvDiscordPublicKey = "DISCORD_PUBLIC_KEY"
	// EnvDiscordBotToken is the bot token. It IS a secret, and it is used for
	// exactly one thing: registering the `/clip` command for a bound guild.
	// Nothing else in the binary sends it, and it is redacted everywhere a
	// value can be printed (erratum 84's discipline, applied to a second
	// secret). The one value that is NOT a secret is ManagedToken below.
	EnvDiscordBotToken = "DISCORD_BOT_TOKEN"
	// EnvDiscordCommandName is the slash command's NAME, default "clip"
	// (§20 erratum 284). It is a FOURTH variable and deliberately not part of
	// the three-variable rule: it has a default, so it is never required — but
	// it is meaningless without the bot, and setting it with the bot off is the
	// same kind of typo the rule above exists for, so it is a boot error too.
	EnvDiscordCommandName = "DISCORD_COMMAND_NAME"
	// EnvDiscordUploadCommandName is the UPLOAD command's name, default
	// "upload" (§20 erratum 315). Since that erratum the bot publishes TWO
	// commands: `/upload file:` takes a video, and `/clip link:` posts a clip
	// already here as a card. DISCORD_COMMAND_NAME kept naming `/clip` — the
	// LINK command — and this is its twin for the other one: same default rule,
	// same shape rule, same orphan rule, same managed rule, and the two must
	// differ, because one Discord server holds one command per name.
	EnvDiscordUploadCommandName = "DISCORD_UPLOAD_COMMAND_NAME"
	// EnvDiscordEnrichCommandName is the MESSAGE command's name, default
	// "Enrich" (§20 erratum 315's addendum): the right-click Apps → Enrich on a
	// message that carries a link to a clip here. A message command's name
	// follows Discord's rule for type 3, not the slash-command pattern: mixed
	// case and spaces are allowed ("Enrich (dev)"), so only its LENGTH is
	// checked — plus this loader's own whitespace-around rule, the orphan rule
	// and the managed rule. It need not differ from the two chat names: Discord
	// keeps message commands in a namespace of their own.
	EnvDiscordEnrichCommandName = "DISCORD_ENRICH_COMMAND_NAME"
	// EnvDiscordSettingsCommandName is the SETTINGS command's name, default
	// "settings" (§20 erratum 318; run 4's panel contract §1): `/settings`
	// answers an ephemeral panel whose one row opens the account's link defaults
	// in a modal. A slash command like `/upload` and `/clip`, so it is held to
	// the same shape, orphan and managed rules, and it must differ from both of
	// them — one Discord server holds one chat command per name.
	EnvDiscordSettingsCommandName = "DISCORD_SETTINGS_COMMAND_NAME"
	// EnvDiscordUploadForm is the upload form's switch, `on` (default) or `off`
	// (§20 erratum 320). It lives on Config rather than on DiscordBot because it
	// is a behaviour of the endpoint, not part of the application's identity.
	EnvDiscordUploadForm = "DISCORD_UPLOAD_FORM"
	// EnvDiscordEnrichUpload is Enrich's upload switch, `off` (default) or `on`
	// (§20 erratum 323): on Config beside the form's, for the same reason.
	EnvDiscordEnrichUpload = "DISCORD_ENRICH_UPLOAD"
)

// DefaultCommandName is the slash command every instance publishes unless its
// operator says otherwise, and the ONE spelling internal/httpapi answers and
// internal/notify registers by default (a test in internal/httpapi pins all
// three against each other, since §3 forbids the import that would make them
// one constant).
//
// It is also the name a MANAGED instance is held to: the official application
// publishes exactly one global command, so an instance answering that
// application's forwarder must answer the same word (erratum 284).
const DefaultCommandName = "clip"

// DefaultUploadCommandName is the upload command's default name (§20 erratum
// 315) — the twin of DefaultCommandName, pinned the same way against
// internal/notify's spelling and the endpoint's by a test in internal/httpapi,
// and held on a MANAGED instance for the same reason: the official application
// publishes `/upload` and `/clip` globally, under exactly these two words.
const DefaultUploadCommandName = "upload"

// DefaultEnrichCommandName is the message command's default name (§20 erratum
// 315's addendum), pinned like the other two by a test in internal/httpapi.
const DefaultEnrichCommandName = "Enrich"

// DefaultSettingsCommandName is the settings command's default name (§20
// erratum 318), pinned like the other three by a test in internal/httpapi, and
// held on a MANAGED instance for their reason: the official application
// publishes `/settings` globally under exactly this word.
const DefaultSettingsCommandName = "settings"

// SigninCommandName is `/signin` (§20 erratum 341): the command that mints a
// one-time login link for a member of a bound server. It has no variable of
// its own — the official application publishes it under exactly this word and
// an own application under the same one — so it is a constant, and the three
// configurable chat names may not take it (one server holds one command per
// name). internal/notify registers its own spelling; a test in internal/httpapi
// pins the two.
const SigninCommandName = "signin"

// maxCommandNameChars is Discord's limit on a chat-input command name.
const maxCommandNameChars = 32

// ManagedToken is the sentinel an instance sets as its DISCORD_BOT_TOKEN when
// it answers the OFFICIAL Clippet bot instead of an application of its own
// (§20 erratum 279, DEPLOY §30.8). The official bot's `/clip` reaches this
// instance through a forwarder that relays Discord's own signed interaction,
// and the whole reply — the deferral, the fetch, the follow-up edit — needs
// no bot token at all: the follow-up is authenticated by the interaction
// token in the body. What a token was needed for, publishing the command,
// happens ONCE, globally, on the application's side. So the third variable
// is set to this word, which passes the "all three or none" rule honestly
// instead of with a token-shaped lie, is never sent anywhere, and turns
// `admin bot register` into a refusal rather than a 401 from Discord.
const ManagedToken = "managed"

// Snowflake and token bounds. Both are deliberately generous: they exist to
// catch a value pasted into the wrong variable (a URL, a token, an empty
// string), not to second-guess Discord's own id or token format, which has
// changed twice already.
const (
	minSnowflakeDigits = 15
	maxSnowflakeDigits = 25
	minBotTokenChars   = 20
	maxBotTokenChars   = 200
)

// DiscordBot is the validated Discord application this instance answers
// interactions for.
//
// **The token is a SECRET** and is reachable only through Token(). The
// String()/LogValue() pair renders the harmless half — the application id and
// the public key's first bytes — so a Config printed with %v, handed to slog
// or walked by `admin doctor` can never leak it.
type DiscordBot struct {
	appID     string
	publicKey ed25519.PublicKey
	pubHex    string
	token     string // SECRET
	command   string // the LINK command's name, without the slash (errata 284, 315)
	upload    string // the UPLOAD command's name, without the slash (erratum 315)
	enrich    string // the MESSAGE command's name, as the Apps menu shows it (erratum 315)
	settings  string // the SETTINGS command's name, without the slash (erratum 318)
}

// Configured reports whether the bot exists at all. False is the default and
// the OFF state: POST /api/discord/interactions is not registered, no client
// is built and no command is ever registered.
func (b DiscordBot) Configured() bool { return b.appID != "" }

// AppID is the application snowflake (not secret).
func (b DiscordBot) AppID() string { return b.appID }

// PublicKey is the Ed25519 key every interaction signature is verified
// against (not secret: it cannot sign).
func (b DiscordBot) PublicKey() ed25519.PublicKey { return b.publicKey }

// PublicKeyHex is the key as the developer portal shows it (not secret).
func (b DiscordBot) PublicKeyHex() string { return b.pubHex }

// CommandName is the slash command this instance answers and registers, without
// the leading slash (DISCORD_COMMAND_NAME, default DefaultCommandName; §20
// erratum 284). It is "" only on the OFF state, where there is no command at
// all — the endpoint is not routed and nothing is ever registered — so no
// caller has to decide what an unconfigured bot's command would be called.
//
// Since §20 erratum 315 this is the LINK command, `/clip link:`, which posts a
// clip already on this instance as a card; the upload command has its own name,
// UploadCommandName.
func (b DiscordBot) CommandName() string { return b.command }

// UploadCommandName is the command that takes a video, `/upload file:`
// (DISCORD_UPLOAD_COMMAND_NAME, default DefaultUploadCommandName; §20 erratum
// 315). "" only on the OFF state, exactly as CommandName.
func (b DiscordBot) UploadCommandName() string { return b.upload }

// EnrichCommandName is the message command's name — the entry a person picks
// from a message's Apps menu (DISCORD_ENRICH_COMMAND_NAME, default
// DefaultEnrichCommandName; §20 erratum 315's addendum). "" only on the OFF
// state.
func (b DiscordBot) EnrichCommandName() string { return b.enrich }

// SettingsCommandName is the command that answers the link-defaults panel,
// `/settings` (DISCORD_SETTINGS_COMMAND_NAME, default DefaultSettingsCommandName;
// §20 erratum 318). "" only on the OFF state.
func (b DiscordBot) SettingsCommandName() string { return b.settings }

// Token is the bot token — the secret. Only internal/notify may put it on the
// wire; nothing may put it in a log line, an error or a DB row. When Managed()
// it is the sentinel word and nothing may put it on the wire either.
func (b DiscordBot) Token() string { return b.token }

// Managed reports whether this instance answers the official bot rather than
// its own application: DISCORD_BOT_TOKEN is ManagedToken, no command is ever
// registered from here, and the interactions route works exactly as it does
// for an own application (the credential is Discord's signature either way).
func (b DiscordBot) Managed() bool { return b.Configured() && b.token == ManagedToken }

// String is the redacted rendering: never the token — and, for the managed
// case, the honest word, which is not a secret.
func (b DiscordBot) String() string {
	if !b.Configured() {
		return "(unset)"
	}
	head := b.pubHex
	if len(head) > 8 {
		head = head[:8]
	}
	if b.Managed() {
		return "app " + b.appID + ", public key " + head + "… , token managed (the official bot)"
	}
	return "app " + b.appID + ", public key " + head + "… , token (redacted)"
}

// LogValue makes slog print the redacted form even if the whole value is
// logged by accident.
func (b DiscordBot) LogValue() slog.Value { return slog.StringValue(b.String()) }

// discordAPIHost is the ONE host the bot talks to: the interaction follow-up
// (`/webhooks/{app}/{token}/messages/@original`) and the command registration
// (`/applications/{app}/guilds/{guild}/commands`) both live there. It is the
// same host DISCORD_WEBHOOK_URL is already allowed to name — that is not a
// coincidence and is pinned by a test — so the §15 destination list gains no
// new NAME here, only two new paths.
//
// Stored as a bare host name, never as a URL literal: §15 / §20 erratum 27
// forbid an `http(s)://` literal anywhere in the binary, and the point of the
// allowlist is that the no-call-home guarantee stays greppable.
const discordAPIHost = "discord.com"

// discordCDNHosts is the closed set of hosts an attachment may be FETCHED
// from. These two are the only ones Discord puts in an interaction's
// `resolved.attachments[].url`: `cdn.discordapp.com` serves the file and
// `media.discordapp.net` serves its media proxy's copy. They are the ONLY new
// egress the bot adds (§15, §20 erratum 254).
//
// The set is closed for erratum 63's second reason: the URL is chosen by
// whoever ran the command, not by the operator, so "fetch what the request
// names" would turn a slash command into a server-side request forger with
// this binary's network position — inside the compose network, where
// `minio:9000` and `host.docker.internal` resolve. That is precisely the
// egress ROADMAP's "upload via URL" was refused for.
var discordCDNHosts = map[string]bool{
	"cdn.discordapp.com":   true,
	"media.discordapp.net": true,
}

// DiscordAPIHost is the host of every call the bot makes.
func DiscordAPIHost() string { return discordAPIHost }

// DiscordCDNHosts lists the attachment hosts, sorted — for DEPLOY, for
// `admin doctor` and for the tests that pin the allowlist.
func DiscordCDNHosts() []string { return sortedKeys(discordCDNHosts) }

// ErrAttachmentHost is what ValidAttachmentURL returns for anything that is
// not an https URL on an allowlisted CDN host. It carries no value: the URL
// comes from a Discord interaction and ends up in a log line.
var ErrAttachmentHost = errors.New("the attachment is not on an allowed Discord CDN host")

// ValidAttachmentURL is the egress gate on an attachment: an absolute https
// URL, no userinfo, and a host in discordCDNHosts exactly (no port, no
// subdomain, no lookalike). It is the one function that decides where the
// bot's fetch may point, and it lives here — beside the webhook's allowlist —
// so §15's "S3, and Discord" claim has exactly one home.
func ValidAttachmentURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, ErrAttachmentHost
	}
	if u.Scheme != "https" || u.User != nil {
		return nil, ErrAttachmentHost
	}
	// ASCII only, BEFORE the fold: strings.ToLower is Unicode, and U+0130 (a
	// dotted capital I, raw or percent-encoded) lowercases to an ASCII "i", so
	// a lookalike host would pass the map lookup and then be dialled in its
	// IDNA form — a different registrable domain. The fold below is for ASCII
	// case alone.
	for i := 0; i < len(u.Host); i++ {
		if u.Host[i] >= 0x80 {
			return nil, ErrAttachmentHost
		}
	}
	if !discordCDNHosts[strings.ToLower(u.Host)] {
		return nil, ErrAttachmentHost
	}
	return u, nil
}

// asciiLower folds ASCII letters only (§20 erratum 327). Every host this
// package compares or shows goes through it rather than strings.ToLower, whose
// Unicode fold can turn a non-ASCII host into an ASCII one (U+0130 → "i").
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// discordBot reads and validates the three variables as ONE value.
//
// Nothing it says echoes the token: a rejection names the rule that failed,
// exactly as parseDiscordURL's do, because error strings reach stderr and
// `docker compose logs app`.
func (l *loader) discordBot() DiscordBot {
	appID := l.raw(EnvDiscordAppID)
	pubHex := l.raw(EnvDiscordPublicKey)
	token := l.raw(EnvDiscordBotToken)
	// The command name is read BOTH ways. The trimmed value answers "is it set
	// at all", which is what the orphan rule below is about and is how every
	// other variable in this loader is read. The UNTRIMMED one goes to
	// commandName, because `l.raw` trims and `DISCORD_COMMAND_NAME=clip `
	// therefore reached it as `clip` and was accepted — while what Discord
	// would be asked to register is whatever the operator actually typed. A
	// name with content and stray whitespace is refused by name rather than
	// silently decided (§4); a whitespace-ONLY value is still simply unset.
	commandRaw := l.getenv(EnvDiscordCommandName)
	uploadRaw := l.getenv(EnvDiscordUploadCommandName)
	enrichRaw := l.getenv(EnvDiscordEnrichCommandName)
	settingsRaw := l.getenv(EnvDiscordSettingsCommandName)
	set := 0
	for _, v := range []string{appID, pubHex, token} {
		if v != "" {
			set++
		}
	}
	if set == 0 {
		// An orphan of the same family as one of the three alone: the operator
		// renamed a command on an instance that publishes none and answers none,
		// and nothing anywhere would have said so (§4, and the reason the three
		// travel together). Each of the two names is its own orphan (§20 erratum
		// 315), so each is named when it is the one set.
		for _, n := range []struct{ env, raw string }{
			{EnvDiscordUploadCommandName, uploadRaw}, {EnvDiscordCommandName, commandRaw},
			{EnvDiscordEnrichCommandName, enrichRaw}, {EnvDiscordSettingsCommandName, settingsRaw},
		} {
			if strings.TrimSpace(n.raw) != "" {
				l.fail(n.env, "names a slash command and means nothing with the bot off: set %s, %s and %s too, or unset it (DEPLOY.md §30.2)",
					EnvDiscordAppID, EnvDiscordPublicKey, EnvDiscordBotToken)
			}
		}
		return DiscordBot{} // the default: the bot does not exist
	}
	if set < 3 {
		// One variable of a three-variable feature is not a partial
		// configuration, it is a typo — and a bot that boots without its
		// token registers no command, while one without its public key
		// refuses every interaction as unsigned. Both look like Discord's
		// fault from the outside. Fail at boot instead (§4).
		l.fail(EnvDiscordAppID, "the Discord bot needs all three of %s, %s and %s, or none of them (set: %s); unset the ones you did set to turn the bot off deliberately",
			EnvDiscordAppID, EnvDiscordPublicKey, EnvDiscordBotToken, l.botNamesSet(appID, pubHex, token))
		return DiscordBot{}
	}
	bot := DiscordBot{appID: appID, pubHex: strings.ToLower(pubHex), token: token}
	// The NAME is resolved first, so a bad name is reported BESIDE a bad
	// application id or public key instead of behind it: the loader collects
	// every fault and prints them together (§4), and until this the two
	// refusals below returned before commandName had run, so an operator who
	// mistyped both fixed one, restarted, and met the other.
	managed := token == ManagedToken
	var linkOK, uploadOK, settingsOK bool
	bot.command, linkOK = l.commandName(EnvDiscordCommandName, commandRaw, managed, DefaultCommandName)
	bot.upload, uploadOK = l.commandName(EnvDiscordUploadCommandName, uploadRaw, managed, DefaultUploadCommandName)
	bot.enrich = l.messageCommandName(enrichRaw, managed)
	bot.settings, settingsOK = l.commandName(EnvDiscordSettingsCommandName, settingsRaw, managed, DefaultSettingsCommandName)
	// The must-differ rule over the THREE chat names (§20 erratum 318 extends
	// erratum 315's pair): `/settings` must not collide with either of the
	// other two, each pair checked only when both of its names resolved
	// cleanly. The message command stays outside it — Discord keeps message
	// commands in a namespace of their own (erratum 315's addendum).
	if settingsOK && linkOK && bot.settings == bot.command {
		l.fail(EnvDiscordSettingsCommandName, "must differ from %s: both name /%s, and one Discord server holds one command per name, so the settings command and the link command would replace each other (DEPLOY.md §30.2)",
			EnvDiscordCommandName, bot.settings)
	}
	if settingsOK && uploadOK && bot.settings == bot.upload {
		l.fail(EnvDiscordSettingsCommandName, "must differ from %s: both name /%s, and one Discord server holds one command per name, so the settings command and the upload command would replace each other (DEPLOY.md §30.2)",
			EnvDiscordUploadCommandName, bot.settings)
	}
	// `/signin` is a fixed name (§20 erratum 341): a chat name renamed onto it
	// would replace it in every server this instance registers.
	for _, n := range []struct {
		env, name string
		ok        bool
	}{{EnvDiscordCommandName, bot.command, linkOK}, {EnvDiscordUploadCommandName, bot.upload, uploadOK}, {EnvDiscordSettingsCommandName, bot.settings, settingsOK}} {
		if n.ok && n.name == SigninCommandName {
			l.fail(n.env, "must not be %q: /%s is the sign-in command's fixed name, and one Discord server holds one command per name (DEPLOY.md §30.2)",
				SigninCommandName, SigninCommandName)
		}
	}
	if linkOK && uploadOK && bot.command == bot.upload {
		// Two commands under ONE name (§20 erratum 315). One Discord server holds
		// one command per name, so the registration would carry the same name
		// twice — a PUT Discord refuses whole — and the endpoint could not tell
		// which of the two an interaction meant. Checked only when both names
		// resolved cleanly: a refused name falls back to its default, and that
		// fallback must not be reported as a collision nobody typed.
		l.fail(EnvDiscordUploadCommandName, "must differ from %s: both name /%s, and one Discord server holds one command per name, so the upload command and the link command would replace each other (DEPLOY.md §30.2)",
			EnvDiscordCommandName, bot.upload)
	}
	if !isSnowflake(appID) {
		l.fail(EnvDiscordAppID, "must be the application's numeric id: %d-%d digits, no spaces (Developer Portal → General Information → APPLICATION ID)",
			minSnowflakeDigits, maxSnowflakeDigits)
		return DiscordBot{}
	}
	key, err := hex.DecodeString(bot.pubHex)
	if err != nil || len(key) != ed25519.PublicKeySize {
		l.fail(EnvDiscordPublicKey, "must be the application's PUBLIC KEY: exactly %d hex characters (Developer Portal → General Information → PUBLIC KEY)",
			ed25519.PublicKeySize*2)
		return DiscordBot{}
	}
	bot.publicKey = ed25519.PublicKey(key)
	if token == ManagedToken {
		// The official-bot sentinel (§20 erratum 279): accepted BEFORE the
		// charset rule, which stays exactly as strict for everything else.
		return bot
	}
	if !isBotToken(token) {
		// The value is a live credential: name the rule, never the value.
		l.fail(EnvDiscordBotToken, "must be the bot token as the portal prints it: %d-%d characters of A-Z, a-z, 0-9, '_', '.' and '-' (the value is a secret and is not echoed here)",
			minBotTokenChars, maxBotTokenChars)
		return DiscordBot{}
	}
	return bot
}

// commandName resolves one of the two command names for a bot that IS
// configured — DISCORD_COMMAND_NAME (§20 erratum 284) or, since erratum 315,
// DISCORD_UPLOAD_COMMAND_NAME, each with its own default: the default when it is
// unset, the value when Discord would accept it, and a boot error otherwise. The
// bool says whether the name resolved cleanly (unset counts), so the caller's
// must-differ rule never compares a fallback.
//
// It takes the UNTRIMMED value, and that is the third rule. **Whitespace**: a
// name with content and a space around it is refused rather than trimmed into
// something the operator did not write — `DISCORD_COMMAND_NAME=clip ` used to
// be accepted as `clip`, which is fine until the day the space was the typo and
// the name was not. A whitespace-ONLY value is still "unset", as it is
// everywhere else in this loader. Then **Discord's**: a chat-input
// command name is 1-32 characters of letters, digits, '-' and '_', and it must
// carry no uppercase — Discord rejects the registration, which is a rejection
// an operator would meet minutes later, from the wrong side, as "the command
// never appeared". **Ours**: a MANAGED instance (erratum 279) must answer the
// official application's one global command, because the forwarder relays that
// command and no other. A renamed managed instance would publish nothing, be
// reached by nothing and refuse the one interaction that did arrive with
// "Clippet does not know that command" — silence with extra steps, which is
// what §4 says to turn into a boot error. (Since erratum 315 the official
// application publishes TWO commands, `/upload` and `/clip`, and each name is held
// to its own default.)
//
// A rejection falls back to the default rather than to "", so nothing
// downstream ever has to cope with a nameless command: config.Load is going to
// return the error anyway and the process will not start.
func (l *loader) commandName(env, raw string, managed bool, def string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" {
		// Unset, or whitespace only — which reads as unset for every variable
		// this loader takes, and turning that one into a refusal would be a new
		// rule rather than this one.
		return def, true
	}
	if name != raw {
		l.fail(env, "must carry no leading or trailing whitespace: Discord registers the name as written, and %q is not %q — remove the space (DEPLOY.md §30.2)",
			raw, name)
		return def, false
	}
	if !validCommandName(name) {
		l.fail(env, "must be a Discord slash-command name: 1-%d characters of lowercase letters, digits, '-' and '_' — no spaces, no uppercase and no leading slash (Discord rejects the registration otherwise), got %q",
			maxCommandNameChars, name)
		return def, false
	}
	if managed && name != def {
		if env == EnvDiscordSettingsCommandName {
			// The official application publishes `/settings` globally too, and
			// its forwarder answers it itself (§20 erratum 318): a renamed one
			// here would name a command nobody can run.
			l.fail(env, "must be %q, or unset, while %s=%s: the official Clippet bot publishes /%s globally under that name (DEPLOY.md §30.8); got %q",
				def, EnvDiscordBotToken, ManagedToken, DefaultSettingsCommandName, name)
			return def, false
		}
		l.fail(env, "must be %q, or unset, while %s=%s: the official Clippet bot publishes /%s and /%s globally, and those are the only commands its forwarder ever relays, so an instance answering it under another name would answer nothing (DEPLOY.md §30.8); got %q",
			def, EnvDiscordBotToken, ManagedToken, DefaultUploadCommandName, DefaultCommandName, name)
		return def, false
	}
	return name, true
}

// messageCommandName resolves DISCORD_ENRICH_COMMAND_NAME (§20 erratum 315's
// addendum). Discord's rule for a MESSAGE command's name is not the slash
// pattern: it "may be mixed case and can include spaces", 1-32 characters — so
// the length is the only shape rule. The two rules of ours stand as they do for
// the chat names: whitespace AROUND the name is refused rather than trimmed,
// and a managed instance answers the official application's own word.
func (l *loader) messageCommandName(raw string, managed bool) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return DefaultEnrichCommandName
	}
	if name != raw {
		l.fail(EnvDiscordEnrichCommandName, "must carry no leading or trailing whitespace: Discord registers the name as written, and %q is not %q — remove the space (DEPLOY.md §30.2)",
			raw, name)
		return DefaultEnrichCommandName
	}
	if n := utf8.RuneCountInString(name); n > maxCommandNameChars {
		l.fail(EnvDiscordEnrichCommandName, "must be 1-%d characters (Discord's limit for a message command's name, which may carry uppercase and spaces), got %d",
			maxCommandNameChars, n)
		return DefaultEnrichCommandName
	}
	if managed && name != DefaultEnrichCommandName {
		l.fail(EnvDiscordEnrichCommandName, "must be %q, or unset, while %s=%s: the official Clippet bot publishes its message command under that name, and those are the only commands its forwarder ever relays (DEPLOY.md §30.8); got %q",
			DefaultEnrichCommandName, EnvDiscordBotToken, ManagedToken, name)
		return DefaultEnrichCommandName
	}
	return name
}

// validCommandName applies Discord's own rule for a CHAT_INPUT command name:
// 1-32 characters, each a letter, a digit, '-' or '_', and no uppercase
// anywhere. "No uppercase" is spelled as "the rune is its own lowercase" rather
// than as unicode.IsUpper, so a titlecase rune — which is neither upper nor
// lower — is refused too.
func validCommandName(s string) bool {
	n := 0
	for _, r := range s {
		n++
		if n > maxCommandNameChars {
			return false
		}
		switch {
		case r == '-', r == '_':
		case unicode.IsLetter(r), unicode.IsNumber(r):
			if r != unicode.ToLower(r) {
				return false
			}
		default:
			return false
		}
	}
	return n > 0
}

// botNamesSet lists which of the three were set, for the "all or none"
// failure. It names VARIABLES, never values.
func (l *loader) botNamesSet(appID, pubHex, token string) string {
	var names []string
	for _, p := range []struct {
		name, value string
	}{{EnvDiscordAppID, appID}, {EnvDiscordPublicKey, pubHex}, {EnvDiscordBotToken, token}} {
		if p.value != "" {
			names = append(names, p.name)
		}
	}
	return strings.Join(names, ", ")
}

// isSnowflake reports whether s is a plausible Discord id: digits only, within
// the length a 64-bit snowflake can have.
func isSnowflake(s string) bool {
	if len(s) < minSnowflakeDigits || len(s) > maxSnowflakeDigits {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isBotToken applies the charset every Discord token has used (three
// dot-separated URL-safe base64 runs). The charset is not decoration: it is
// what lets internal/notify scrub the token out of an echoed error by a plain
// string match, exactly as the webhook's does (§20 erratum 63).
func isBotToken(s string) bool {
	if len(s) < minBotTokenChars || len(s) > maxBotTokenChars {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}
