package config

import (
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
)

// Clippet Auto ON THE BOX (§20 erratum 333, DEPLOY §30.10; run 14 lane
// `autobox`). The instance itself holds a Discord gateway connection for an
// application its operator made, and turns the videos posted in the servers
// bound to an account here into Clippet links — Prompt (one reply, one button)
// or Auto (no press). It is a DIFFERENT thing from §30's bot: that one answers
// interactions over HTTP and holds no socket; this one is the only part of the
// binary that reads a channel's messages, and it is OFF unless
// DISCORD_AUTO_MODE says otherwise.
//
// Four variables:
//
//	DISCORD_AUTO_MODE     off (default) | prompt | auto
//	DISCORD_AUTO_TOKEN    the bot token of the operator's own application (SECRET)
//	DISCORD_AUTO_SECONDS  Prompt's reply lifetime, 5-60, default 10
//	DISCORD_AUTO_NOTICE   on (default) | off — the line under each link
//
// The rules, each a boot error with the usual one-line reason (§4):
//
//   - With the mode unset, the other three are orphans: a token pasted without
//     a mode is a feature the operator believes is on.
//   - `off` written out is a deliberate off: the token may stay in the file and
//     nothing reads it — no socket is opened, no client is built.
//   - prompt|auto needs the token, in the bot token's shape, and needs §30's bot
//     configured (DISCORD_APP_ID + DISCORD_PUBLIC_KEY + DISCORD_BOT_TOKEN, own
//     or `managed`): the servers it acts in are the ones `admin bot bind` bound,
//     and the attachment is fetched by that bot's fetcher (the CDN allowlist,
//     the size cap). The token is never `managed`: the official application's
//     token is not the operator's to hold.
const (
	EnvDiscordAutoMode    = "DISCORD_AUTO_MODE"
	EnvDiscordAutoToken   = "DISCORD_AUTO_TOKEN"
	EnvDiscordAutoSeconds = "DISCORD_AUTO_SECONDS"
	EnvDiscordAutoNotice  = "DISCORD_AUTO_NOTICE"
)

// The three modes (the owner's design, 2026-10-11).
const (
	AutoModeOff    = "off"
	AutoModePrompt = "prompt"
	AutoModeAuto   = "auto"
)

// The Prompt reply's lifetime bounds and default, in seconds.
const (
	DefaultAutoSeconds = 10
	MinAutoSeconds     = 5
	MaxAutoSeconds     = 60
)

// DiscordAuto is the validated Clippet Auto configuration. The token is a
// SECRET, reachable only through Token(); String/LogValue never render it.
type DiscordAuto struct {
	mode    string
	token   string // SECRET
	seconds int
	notice  bool
}

// On reports whether the box opens a gateway socket at all: prompt or auto.
// False is the default, and then nothing of the feature runs.
func (a DiscordAuto) On() bool { return a.mode == AutoModePrompt || a.mode == AutoModeAuto }

// Mode is off, prompt or auto ("off" for the zero value).
func (a DiscordAuto) Mode() string {
	if a.mode == "" {
		return AutoModeOff
	}
	return a.mode
}

// Token is the application's bot token — the secret. Only internal/notify may
// put it on the wire.
func (a DiscordAuto) Token() string { return a.token }

// Seconds is Prompt's reply lifetime (DefaultAutoSeconds for the zero value).
func (a DiscordAuto) Seconds() int {
	if a.seconds == 0 {
		return DefaultAutoSeconds
	}
	return a.seconds
}

// Notice reports whether the line goes under each link.
func (a DiscordAuto) Notice() bool { return a.notice }

// String is the redacted rendering.
func (a DiscordAuto) String() string {
	if !a.On() {
		return "off"
	}
	n := "on"
	if !a.notice {
		n = "off"
	}
	return a.mode + ", " + strconv.Itoa(a.Seconds()) + " s, notice " + n + ", token (redacted)"
}

// LogValue makes slog print the redacted form.
func (a DiscordAuto) LogValue() slog.Value { return slog.StringValue(a.String()) }

// discordAuto reads the four variables as ONE value. bot is the already-read
// §30 bot, which prompt|auto requires.
func (l *loader) discordAuto(bot DiscordBot) DiscordAuto {
	modeRaw := l.raw(EnvDiscordAutoMode)
	token := l.raw(EnvDiscordAutoToken)
	secsSet := l.raw(EnvDiscordAutoSeconds) != ""
	noticeSet := l.raw(EnvDiscordAutoNotice) != ""
	if modeRaw == "" {
		for _, o := range []struct {
			env string
			set bool
		}{{EnvDiscordAutoToken, token != ""}, {EnvDiscordAutoSeconds, secsSet}, {EnvDiscordAutoNotice, noticeSet}} {
			if o.set {
				l.fail(o.env, "means nothing with %s unset: set %s=prompt or auto to turn Clippet Auto on, %s=off to keep it off deliberately, or unset this (DEPLOY.md §30.10)",
					EnvDiscordAutoMode, EnvDiscordAutoMode, EnvDiscordAutoMode)
			}
		}
		return DiscordAuto{}
	}
	mode := l.oneOf(EnvDiscordAutoMode, AutoModeOff, AutoModeOff, AutoModePrompt, AutoModeAuto)
	a := DiscordAuto{
		mode:    mode,
		seconds: l.intRange(EnvDiscordAutoSeconds, DefaultAutoSeconds, MinAutoSeconds, MaxAutoSeconds),
		notice:  l.oneOf(EnvDiscordAutoNotice, "on", "on", "off") == "on",
	}
	if mode == AutoModeOff {
		// A deliberate off: the token may stay, nothing reads it.
		return DiscordAuto{mode: AutoModeOff, seconds: a.seconds, notice: a.notice}
	}
	ok := true
	switch {
	case token == "":
		l.fail(EnvDiscordAutoToken, "is required while %s=%s: the bot token of the Discord application Clippet Auto runs as (DEPLOY.md §30.10)", EnvDiscordAutoMode, mode)
		ok = false
	case token == ManagedToken:
		l.fail(EnvDiscordAutoToken, "must be your own application's bot token, not %q: the official Clippet bot's token is not held by an instance (DEPLOY.md §30.10)", ManagedToken)
		ok = false
	case !isBotToken(token):
		l.fail(EnvDiscordAutoToken, "must be the bot token as the portal prints it: %d-%d characters of A-Z, a-z, 0-9, '_', '.' and '-' (the value is a secret and is not echoed here)",
			minBotTokenChars, maxBotTokenChars)
		ok = false
	}
	if !bot.Configured() {
		l.fail(EnvDiscordAutoMode, "%s needs the Discord bot configured (%s, %s and %s — your own application or %s=%s): Clippet Auto acts only in servers bound with `clippet admin bot bind`, and fetches through that bot (DEPLOY.md §30.10)",
			mode, EnvDiscordAppID, EnvDiscordPublicKey, EnvDiscordBotToken, EnvDiscordBotToken, ManagedToken)
		ok = false
	}
	if !ok {
		return DiscordAuto{}
	}
	a.token = token
	return a
}

// The gateway host rule (§15): the socket goes to Discord's gateway and
// nowhere else. Discord names the URL itself — `GET /gateway/bot` answers
// `gateway.discord.gg`, and READY's `resume_gateway_url` a regional
// `gateway-<region>.discord.gg` — so the rule is the scheme, the shape of the
// host, and nothing a third party can steer: no userinfo, no port, ASCII only.
const discordGatewayHost = "gateway.discord.gg"

var gatewayRegionalHost = regexp.MustCompile(`^gateway-[a-z0-9-]{1,48}\.discord\.gg$`)

// DiscordGatewayHost is the gateway's main host, for DEPLOY and the doctor.
func DiscordGatewayHost() string { return discordGatewayHost }

// ValidGatewayURL is the egress gate on a gateway URL: `wss`, no userinfo, no
// port, no path beyond "/", and the host `gateway.discord.gg` or a regional
// `gateway-….discord.gg`. Any other answer is refused before a dial.
func ValidGatewayURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return nil, ErrGatewayHost
	}
	if u.Path != "" && u.Path != "/" {
		return nil, ErrGatewayHost
	}
	for i := 0; i < len(u.Host); i++ {
		if u.Host[i] >= 0x80 {
			return nil, ErrGatewayHost
		}
	}
	h := asciiLower(u.Host)
	if h != discordGatewayHost && !gatewayRegionalHost.MatchString(h) {
		return nil, ErrGatewayHost
	}
	return u, nil
}

// ErrGatewayHost carries no value: the URL came off the network.
var ErrGatewayHost = errorString("the gateway URL is not on Discord's gateway host")

type errorString string

func (e errorString) Error() string { return string(e) }
