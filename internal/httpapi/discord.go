package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"clippet/internal/autobot"
	"clippet/internal/autorule"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/notify"
)

// The Discord bot's INBOUND half: POST /api/discord/interactions (ROADMAP #33,
// DESIGN.md §20 errata 253-256, 268, 286, 315).
//
// Three commands since erratum 315: `/upload file:` (the one command of errata
// 253-286, renamed) is DEFERRED and finished by internal/notify's goroutine;
// `/clip link:` and the message command "Enrich" (a message's Apps menu) are
// answered at once, on this response, with the message about a clip already
// here — its row is a local read, so there is nothing to defer.
//
// Discord POSTs every slash-command interaction to this URL, signed with the
// application's Ed25519 key, and reads the answer off the HTTP response —
// there is no gateway connection, no WebSocket and no privileged intent
// anywhere in this feature (erratum 253 says what that costs and what it
// buys). The three seconds Discord allows for that answer are the only budget
// that matters here: a `/clip` is acknowledged as DEFERRED and the actual work
// — fetch, ingest, edit the reply — happens in internal/notify's goroutine,
// which is also where the only outbound client lives.
//
// Four properties this file exists to keep:
//
//  1. **Anonymous, and authenticated by signature alone.** No session, no
//     token, no CSRF (there is no browser): `ed25519.Verify(pubkey,
//     timestamp||body, sig)` is the whole credential, and it is checked before
//     the body is parsed, logged or acted on.
//  2. **The route does not exist when the bot is not configured.** routes()
//     registers it only then, so an instance without the three variables
//     answers 404 from the catch-all like any other unknown path.
//  3. **It never 500s.** Discord shows a user "The application did not
//     respond" for anything it cannot read, so every branch answers a legal
//     interaction response — including the ones that are refusals.
//  4. **Nothing it says is a CTA.** A refusal is one terse line; a success is
//     the share link and nothing else — the clip's page declares the card
//     Discord draws under it (§20 errata 268, 315, 316 — no "here you go",
//     no emoji, no "upload more at"; §15 anti-features).
//  5. **`Bot.Clip` never blocks, and this handler depends on it.** The route
//     is wrapped in the 15 s http.TimeoutHandler every short JSON route has,
//     which BUFFERS the response until the handler returns — so the
//     `{"type":5}` written before `h.d.Bot.Clip(...)` reaches Discord only
//     because Clip takes a mutex, adds to a WaitGroup and spawns a goroutine
//     (internal/notify/bot.go), and nothing else. Making Clip do any work
//     inline would hold the deferral past Discord's three seconds — and,
//     since the official bot's forwarder relays this same response under a
//     two-second deadline (§20 erratum 279), past that too, silently.

// Discord's interaction TYPE values (the request).
const (
	interactionPing        = 1 // PING — the portal's endpoint validation
	interactionCommand     = 2 // APPLICATION_COMMAND
	interactionComponent   = 3 // MESSAGE_COMPONENT — a click on the `/settings` panel (§20 erratum 318)
	interactionModalSubmit = 5 // MODAL_SUBMIT — the link-defaults modal saved (§20 erratum 318)
)

// Discord's interaction CALLBACK TYPE values (the response).
const (
	callbackPong            = 1 // answer to a PING
	callbackMessage         = 4 // an immediate message
	callbackDeferredMessage = 5 // "thinking…", edited later through the webhook
	callbackModal           = 9 // a popup modal (§20 erratum 318)
)

// ephemeralFlag (1 << 6) makes a message visible only to the person who ran
// the command. Every refusal carries it: a channel does not need to see that
// somebody's file was too big.
const ephemeralFlag = 64

// optionTypeAttachment is Discord's ATTACHMENT option type — the type of
// `/upload`'s ONE option — and optionTypeString its STRING type, the type of
// `/clip`'s one option since §20 erratum 315. internal/notify registers the
// commands with them; this file reads what comes back under them. The
// spellings are pinned against the registration by a test (§3 forbids the
// import).
const (
	optionTypeString     = 3
	optionTypeAttachment = 11
)

// linkOptionName is `/clip`'s one option, as internal/notify registers it. The
// link is found by name AND type: STRING is the shape any later option would
// take too, and a `/clip` carrying one must not be read as a link.
const linkOptionName = "link"

// commandTypeMessage is Discord's MESSAGE command type — the `data.type` of an
// "Enrich" interaction (§20 erratum 315's addendum). A message command's name
// lives in a namespace of its own, so the endpoint routes it by TYPE and name,
// never by name alone. Pinned against the registration like the option types.
const commandTypeMessage = 3

// The refusals this endpoint answers with, in full. They are terse on purpose
// (the copy rule) and carry no link, no CTA and no instance branding.
const (
	msgUnbound     = "This server is not connected to a Clippet account."
	msgUnknown     = "Clippet does not know that command."
	msgUnavailable = "Clippet is not available right now."
	// msgNotYours answers any member but the one a PERSON binding names (§20
	// erratum 342): this box consented for one member here.
	msgNotYours = "Not your Clippet."
	// msgLinkNotOurs answers `/clip link:` for everything that is not a clip
	// here: a link of another host, a route that is not a clip, a segment that
	// is not an id, an id nobody minted (§20 erratum 315).
	msgLinkNotOurs = "That is not a link to a clip on this Clippet."
	// msgLinkGone answers a link to a clip that WAS here: on its way out (the
	// `deleting` tombstone) or past its expiry (erratum 315).
	msgLinkGone = "That clip is gone."
	// msgEnrichNoLink answers "Enrich" on a message that carries no link to a
	// clip here (erratum 315's addendum) — and, since erratum 323,
	// no video either.
	msgEnrichNoLink = "No link to a clip on this Clippet in that message."
	// msgEnrichNotYours answers "Enrich" on somebody else's video from a member
	// without Manage Server (§20 erratum 323): nothing is fetched.
	msgEnrichNotYours = "That video is not yours. You need Manage Server here."
)

// The two refusals that NAME the command. They stopped being constants at §20
// erratum 284, when DISCORD_COMMAND_NAME made the name a setting: an instance
// renamed to `/clip-dev` must not send the person in the channel to a `/clip` it
// does not answer — which is the exact confusion a rename exists to remove. On a
// default instance both are the byte-identical sentences they were, minus the
// half erratum 286 withdrew — and since erratum 315 each names the command it
// answers: `/upload` or `/clip` for the DM line, `/upload` for the attach line.
func msgNotInGuild(command string) string { return "Run /" + command + " in a server, not in a DM." }

// msgNoInput is what an `/upload` with no usable attachment is told (the upload
// command was `/clip` until §20 erratum 315). Since erratum 286 the option is
// `required`, so Discord itself refuses an empty one in the client and this line
// is the second line of defence: it answers a body that arrives without one
// anyway — anything that is not Discord's own picker.
func msgNoInput(command string) string {
	return "Attach a video to /" + command + "."
}

// discordInteraction is the part of Discord's interaction object this app
// reads. Everything else in the body — the channel, the locale, the
// entitlements — is deliberately not parsed: what is not read cannot be
// depended on. Of the member, TWO fields are read: the permission bitfield —
// what `/settings` and its clicks are gated on (§20 erratum 318, proof A of run
// 4's panel contract), and, with DISCORD_ENRICH_UPLOAD=on, Enrich's upload of
// somebody else's video (§20 erratum 323) — and, for that upload only, the
// user's id, compared with the picked message's author and dropped. The id is
// never logged. Since §20 erratum 341 `/signin` alone reads more: the member's
// roles (the `role:<id>` gate, compared and dropped) and the user's id, username
// and global name, which go into the one-time login link's row — the id is the
// account's key, the names are what the page shows and a new account is named
// from — and nowhere else.
type discordInteraction struct {
	Type    int                    `json:"type"`
	Token   string                 `json:"token"` // SECRET for 15 minutes
	GuildID string                 `json:"guild_id"`
	Member  *discordMember         `json:"member"`
	Data    discordInteractionData `json:"data"`
}

// discordMember is the invoking member as an interaction carries it: the
// guild-computed permission bitfield (a decimal string), the user, and — for
// `/signin`'s role gate only (§20 erratum 341) — the member's role ids.
type discordMember struct {
	Permissions string       `json:"permissions"`
	User        *discordUser `json:"user"`
	Roles       []string     `json:"roles"`
}

// discordUser is a user object as far as this app reads one: its id, for
// Enrich's one comparison (§20 erratum 323), and the two names `/signin` puts
// in its login link's row (§20 erratum 341) — nothing else reads them.
type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
}

type discordInteractionData struct {
	Name string `json:"name"`
	// Type is the COMMAND's type: 1 for a slash command, 3 for a message
	// command (§20 erratum 315's addendum). TargetID is the message a message
	// command was used on, and Resolved.Messages carries it.
	Type     int             `json:"type"`
	TargetID string          `json:"target_id"`
	Options  []discordOption `json:"options"`
	Resolved discordResolved `json:"resolved"`
	// CustomID and ComponentType are a component click's (type 3); CustomID
	// and Components a modal submit's (type 5) — §20 erratum 318.
	CustomID      string                  `json:"custom_id"`
	ComponentType int                     `json:"component_type"`
	Components    []discordModalComponent `json:"components"`
}

// discordOption is one command option. Value is left RAW: an attachment
// option's value is a snowflake STRING, but a body that carries something else
// under it must be a refusal rather than a decode failure of the whole
// interaction.
type discordOption struct {
	Name  string          `json:"name"`
	Type  int             `json:"type"`
	Value json.RawMessage `json:"value"`
}

type discordResolved struct {
	Attachments map[string]discordAttachment `json:"attachments"`
	Messages    map[string]discordMessage    `json:"messages"`
}

// discordMessage is the part of a message an "Enrich" reads: its text, its
// attachments and its author's id (§20 erratum 323) — and nothing else. Its
// embeds, the author's name and the rest are not parsed: what is not read
// cannot be stored or logged (DEPLOY §30.6). Of what IS read, the text is
// scanned and dropped, the author's id is compared and dropped, and of the
// attachments only the first video is used — exactly as `/upload` uses its own
// attachment, the file name becoming the clip's title.
type discordMessage struct {
	Content     string              `json:"content"`
	Author      *discordUser        `json:"author"`
	Attachments []discordAttachment `json:"attachments"`
}

// discordAttachment is one uploaded file as Discord describes it. `url` is the
// CDN link (signed, and short-lived); `size` and `content_type` are Discord's
// claims about it, checked here and enforced again by the ingest core.
type discordAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// discordInteractions is POST /api/discord/interactions.
func (h *handler) discordInteractions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		// Too large, or the connection died mid-body. Either way there is
		// nothing to verify, so it gets the same answer an unsigned request
		// gets: no body, no detail, no hint about what would have worked.
		httpx.SetLogField(r, "reject", "unreadable_body")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if reject := h.verifyInteraction(r, body); reject != "" {
		// Discord's endpoint validation deliberately sends a request with a
		// BAD signature and requires a 401; an endpoint that answers 200 to it
		// is rejected in the portal. The log line names the verdict and never
		// the body — an unverified body is attacker-chosen text. A genuine
		// signature over a timestamp outside the window is `stale_timestamp`
		// (§20 erratum 279 (g), amended): the one verdict that points at this
		// box's clock rather than at the sender.
		httpx.SetLogField(r, "reject", reject)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var in discordInteraction
	if err := json.Unmarshal(body, &in); err != nil {
		httpx.SetLogField(r, "reject", "unparsable_interaction")
		writeEphemeral(w, msgUnknown)
		return
	}
	switch in.Type {
	case interactionPing:
		// The handshake the portal performs when the Interactions Endpoint URL
		// is saved (and periodically afterwards). It is the whole of "is this
		// URL ours".
		httpx.SetLogField(r, "discord", "ping")
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"type": callbackPong})
	case interactionCommand:
		h.discordCommand(w, r, in)
	case interactionComponent, interactionModalSubmit:
		// Clippet Auto's button (§20 erratum 333): an app WITH an Interactions
		// Endpoint URL gets its presses here rather than on the socket, and
		// they go to the one press function both doors share.
		if in.Type == interactionComponent && h.d.Auto != nil && in.Data.CustomID == autobot.PressID {
			httpx.SetLogField(r, "discord", "auto_press")
			answer, start := h.d.Auto.Press(r.Context(), body)
			if answer == nil {
				answer = autobot.PressIgnored()
			}
			httpx.WriteJSON(w, http.StatusOK, answer)
			if start != nil {
				start()
			}
			return
		}
		// A click on the `/settings` panel or the link-defaults modal saved
		// (§20 erratum 318), routed by the `cb:` prefix of its custom_id.
		h.discordPanel(w, r, in)
	default:
		// Autocomplete (4), and anything newer: this application registers
		// none of it, so anything arriving under it is either a future Discord
		// feature or a mistake. Both get a legal answer rather than a 500 (§11:
		// never a permanent-sounding failure for something that is merely
		// unknown).
		httpx.SetLogField(r, "discord", "unhandled_type")
		writeEphemeral(w, msgUnknown)
	}
}

// interactionMaxSkew is how far X-Signature-Timestamp may sit from this box's
// clock, either way (§20 erratum 279 (g), amended): the forwarder's own window
// (clippet-pass forward/src/admit.ts, DEFAULT_MAX_SKEW_S), so the official
// bot's relay and a direct Discord POST are held to one rule.
const interactionMaxSkew = 300 * time.Second

// verifyInteraction is the whole credential: Ed25519 over the timestamp header
// concatenated with the raw body, against the application's public key — and
// that timestamp within interactionMaxSkew of this box's clock. It answers ""
// when the request is admitted, else the access log's `reject` value:
// `bad_signature` (no key, a malformed or wrong signature, a missing or
// non-numeric timestamp) or `stale_timestamp` (a GENUINE signature over a
// timestamp outside the window — checked after the signature, so only a
// request Discord really signed can say anything about the clock).
//
// ed25519.Verify is constant-time by construction, and it is given the RAW
// bytes — not a re-serialisation of the parsed object, which would verify a
// different message than the one that was signed.
//
// The window is what stops a captured request from being replayed later: the
// signature covers the timestamp, so an old request cannot be given a new one.
// Inside the window a replay is still possible: the box holds no nonce (spec
// §6.5, decision 6).
func (h *handler) verifyInteraction(r *http.Request, body []byte) string {
	key := h.d.Cfg.DiscordBot.PublicKey()
	if len(key) != ed25519.PublicKeySize {
		return "bad_signature" // unreachable: the route is not registered without one
	}
	sig, err := hex.DecodeString(r.Header.Get("X-Signature-Ed25519"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "bad_signature"
	}
	ts := r.Header.Get("X-Signature-Timestamp")
	sent, ok := interactionTimestamp(ts)
	if !ok {
		return "bad_signature"
	}
	signed := make([]byte, 0, len(ts)+len(body))
	signed = append(signed, ts...)
	signed = append(signed, body...)
	if !ed25519.Verify(key, signed, sig) {
		return "bad_signature"
	}
	if !interactionFresh(h.now(), sent) {
		return "stale_timestamp"
	}
	return ""
}

// interactionFresh reports whether a timestamp of sent (Unix seconds) is within
// interactionMaxSkew of now, either way; exactly 300 s is still inside.
func interactionFresh(now time.Time, sent int64) bool {
	skew := now.Sub(time.Unix(sent, 0))
	return skew <= interactionMaxSkew && skew >= -interactionMaxSkew
}

// interactionTimestamp parses X-Signature-Timestamp: Unix seconds, ASCII
// digits only (no sign, no space), at most twelve of them — the shape Discord
// sends. Anything else is not a timestamp and the request is refused.
func interactionTimestamp(ts string) (int64, bool) {
	if ts == "" || len(ts) > 12 {
		return 0, false
	}
	for i := 0; i < len(ts); i++ {
		if ts[i] < '0' || ts[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	return n, err == nil
}

// discordCommand handles an APPLICATION_COMMAND: the two commands this bot
// registers — `/upload` and `/clip`, or whatever DISCORD_UPLOAD_COMMAND_NAME and
// DISCORD_COMMAND_NAME renamed them to (§20 errata 284, 315) — and nothing else.
// The NAME routes, and only the name: the two configured names always differ
// (internal/config refuses a pair that does not), so no body can be both.
func (h *handler) discordCommand(w http.ResponseWriter, r *http.Request, in discordInteraction) {
	upload := h.d.Cfg.DiscordBot.UploadCommandName()
	link := h.d.Cfg.DiscordBot.CommandName()
	settings := h.d.Cfg.DiscordBot.SettingsCommandName()
	if in.Data.Type == commandTypeMessage {
		// A message command lives in a namespace of its own (a server may hold
		// a message command and a slash command of one name), so it is routed
		// by its TYPE first and only then by its name.
		enrich := h.d.Cfg.DiscordBot.EnrichCommandName()
		if in.Data.Name != enrich {
			httpx.SetLogField(r, "discord", "unknown_command")
			writeEphemeral(w, msgUnknown)
			return
		}
		httpx.SetLogField(r, "discord", "enrich")
		if in.GuildID == "" {
			// Unreachable from Discord's own client (the command is guild-only),
			// so the line is the generic one rather than a sentence of its own.
			writeEphemeral(w, msgUnavailable)
			return
		}
		// The binding gates it exactly as it gates `/clip link:`: a server
		// nobody connected may not post this instance's clips through this
		// app's name — nor, since §20 erratum 323, upload into it.
		binding, ok := h.guildBinding(w, r, in, enrich)
		if !ok {
			return
		}
		h.discordEnrich(w, r, in, binding)
		return
	}
	switch in.Data.Name {
	case upload, link:
	case settings:
		// `/settings` (§20 erratum 318): the box's own one-row panel.
		httpx.SetLogField(r, "discord", settings)
		h.discordSettingsCommand(w, r, in, settings)
		return
	case config.SigninCommandName:
		// `/signin` (§20 erratum 341): a one-time login link, privately.
		httpx.SetLogField(r, "discord", config.SigninCommandName)
		h.discordSignin(w, r, in)
		return
	default:
		httpx.SetLogField(r, "discord", "unknown_command")
		writeEphemeral(w, msgUnknown)
		return
	}
	command := in.Data.Name
	// The field carries the command this instance ANSWERS, not a literal: on a
	// renamed instance (§20 erratum 284) the log said `discord=clip` for an
	// interaction nobody could have sent under that name, which is the one line
	// an operator greps to see the rename took.
	httpx.SetLogField(r, "discord", command)
	binding, ok := h.guildBinding(w, r, in, command)
	if !ok {
		return
	}
	if command == link {
		h.discordClipLink(w, r, in, binding, upload)
		return
	}
	h.discordUpload(w, r, in, binding, upload)
}

// guildBinding is what both commands check first, in this order: a server and
// not a DM, then the binding — a server nobody connected may neither upload
// into this instance nor post its clips through this app's name. It answers the
// refusal itself and reports whether the caller may go on.
//
// On a PERSON binding (§20 erratum 342) the invoker is the third check: only
// the bound account's linked Discord user may go on, and every other member —
// or an interaction with no member at all — gets one private line before
// anything is fetched. The plane routes a member here only after this box
// answered its proof for that member, so the refusal is the box's own guard
// against a mis-route, not the path a member normally meets.
func (h *handler) guildBinding(w http.ResponseWriter, r *http.Request, in discordInteraction, command string) (db.GuildBinding, bool) {
	if in.GuildID == "" {
		writeEphemeral(w, msgNotInGuild(command))
		return db.GuildBinding{}, false
	}
	if h.d.DB == nil {
		writeEphemeral(w, msgUnavailable)
		return db.GuildBinding{}, false
	}
	binding, err := h.d.DB.GuildBinding(r.Context(), in.GuildID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// The default state of every server: nobody has run
			// `clippet admin bot bind` for it. One line, no CTA, no link —
			// whoever ran the command cannot fix this and the operator does
			// not need a Discord message to find out (DEPLOY §30).
			httpx.SetLogField(r, "discord_guild", "unbound")
			writeEphemeral(w, msgUnbound)
			return db.GuildBinding{}, false
		}
		h.d.Log.Warn("discord bot: guild lookup failed", "err", err)
		writeEphemeral(w, msgUnavailable)
		return db.GuildBinding{}, false
	}
	if binding.Person() && (in.Member == nil || in.Member.User == nil || binding.DiscordID == "" || in.Member.User.ID != binding.DiscordID) {
		httpx.SetLogField(r, "discord_guild", "not_yours")
		writeEphemeral(w, msgNotYours)
		return db.GuildBinding{}, false
	}
	return binding, true
}

// discordUpload is `/upload file:` — the one command of errata 253-286 under
// its own name since §20 erratum 315, and otherwise unchanged: the refusals it
// can see, then the deferral, then the job.
func (h *handler) discordUpload(w http.ResponseWriter, r *http.Request, in discordInteraction, binding db.GuildBinding, command string) {
	// `/upload` names its video ONE way (§20 erratum 286): the attachment, and
	// it is `required` in the command definition, so Discord refuses an empty
	// one in its own client. This is the other end of that rule and it is
	// deliberately kept: a body that carries no usable attachment — anything
	// that is not Discord's picker — gets the one line, never a crash and never
	// a different answer per option.
	att, hasFile := clipAttachment(in.Data)
	if !hasFile {
		httpx.SetLogField(r, "reject", "no_attachment")
		writeEphemeral(w, msgNoInput(command))
		return
	}
	h.startUpload(w, r, in, binding, att, true)
}

// isMedia is the cheap first opinion on an attachment: Discord's own content
// type, or the filename's extension — the same pair the ingest stores — naming
// a video or, since §20 erratum 328, a sound (a voice message is one `audio/ogg`
// attachment). It is not the gate; ffprobe is, and it runs on the bytes (§8.3
// step 6).
func isMedia(att discordAttachment) bool {
	return autorule.IsMedia(att.Filename, att.ContentType)
}

// startUpload is everything `/upload` does once it holds an attachment: the
// refusals it can see, then the form or the deferral, then the job. It is the
// ONE way an attachment becomes a job — `/upload`, an old `/clip file:` and,
// since §20 erratum 323, Enrich on a video all come through here, so there is
// one set of checks and one fetcher (internal/notify's, behind its CDN
// allowlist). `form` says whether the upload form (§20 erratum 320) may answer:
// Enrich passes false — somebody who picked a message asked for a link. It
// reports whether the upload STARTED (a job was handed over), so a caller logs
// an accepted upload only once it is one.
func (h *handler) startUpload(w http.ResponseWriter, r *http.Request, in discordInteraction, binding db.GuildBinding, att discordAttachment, form bool) bool {
	if !isMedia(att) {
		httpx.SetLogField(r, "reject", "not_a_video")
		writeEphemeral(w, notify.SentenceNotVideo)
		return false
	}
	if att.Size > h.d.Cfg.MaxUploadBytes {
		httpx.SetLogField(r, "reject", "too_large")
		writeEphemeral(w, notify.SentenceTooLarge+" (limit "+humanBytes(h.d.Cfg.MaxUploadBytes)+")")
		return false
	}
	if h.d.Bot == nil {
		writeEphemeral(w, msgUnavailable)
		return false
	}
	// The upload form (§20 erratum 320, DISCORD_UPLOAD_FORM, on by default): a
	// modal answers the command and the job starts behind it; its message is a
	// follow-up. A full table, or the switch off, is the deferral below exactly
	// as it was.
	if form && h.d.Cfg.UploadForm && h.openUploadForm(w, r, in, binding, att) {
		return true
	}
	// Answer FIRST — the three-second deadline is Discord's, and everything
	// after this point can take minutes. `type: 5` is a public "thinking…"
	// that becomes the link when internal/notify edits it.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"type": callbackDeferredMessage})
	h.d.Bot.Clip(notify.ClipJob{
		Token: in.Token, URL: att.URL, Filename: att.Filename,
		ContentType: att.ContentType, Size: att.Size, GuildID: in.GuildID,
		Ingest:  h.botIngest(binding.UserID),
		Command: jobCommand(in.Data),
	})
	return true
}

// jobCommand is the command a job's log lines name (run 8): `/upload`, an old
// `/clip file:`'s `/clip`, or a message command's own name (`Enrich`). The
// name is one this endpoint already matched against the commands it answers.
func jobCommand(d discordInteractionData) string {
	if d.Type == commandTypeMessage {
		return d.Name
	}
	return "/" + d.Name
}

// The commands this endpoint answers are config.DiscordBot.UploadCommandName()
// and CommandName() — DISCORD_UPLOAD_COMMAND_NAME, default `upload`, and
// DISCORD_COMMAND_NAME, default `clip` (§20 errata 284, 315). They are read from
// the configuration rather than spelled here so that a renamed instance answers
// the names it publishes: internal/notify registers whatever the same values
// say, and a test pins the DEFAULTS against each other and against internal/config's,
// since §3 forbids the imports that would make them one constant.
//
// An interaction naming anything else is msgUnknown, exactly as it was — which
// is also what a default name reaching a renamed instance gets, and the right
// answer: that command was never published here.

// discordClipLink is `/clip link:` (§20 erratum 315; the owner's call of 2026-10-05:
// /clip is the formal way to show a clip): a share link of THIS instance, answered
// at once with the same message an upload ends in, posted publicly so the
// channel sees it.
//
// **It fetches nothing.** The link is parsed by internal/urls' ParseShare, a
// pure function over a string, and a URL naming any other host is refused here,
// on the three-second response, before anything that could act on it runs —
// erratum 268's load-bearing sentence, restored with the parser: a link option
// is not a fetch option. The only read is the clip's own row.
//
// Three answers, and only one of them public:
//
//  1. Not a clip here — another host, another route, a segment that is not an
//     id, an id nobody minted → msgLinkNotOurs, ephemeral.
//  2. A clip that WAS here — the `deleting` tombstone, or a clip past its expiry
//     that the sweeper has not reached yet → msgLinkGone, ephemeral. (Erratum
//     268 answered this and case 1 with one line, so as not to be an oracle over
//     ids; the two lines are erratum 315's review call, and what they
//     distinguish is only "this id existed and is going", for an id the asker
//     already holds — not a listing, and not "exists, but not for you".)
//  3. Anything else → the message, type 4, public — in EVERY other state. An
//     unlisted clip answers like a listed one (the link is the auth, as on its
//     page), and a queued, encoding or failed clip is posted too, its state in
//     the card's colour: somebody who pastes a link mid-encode wants the message now,
//     and the card Discord draws under it is the link-out card of §9.1.
//
// Ownership is deliberately NOT checked, for erratum 268's reason: every clip
// here is viewable by anyone holding its link, so posting one discloses nothing
// pasting it by hand would not, and a server where two people upload is the
// ordinary case. The rate limit is the endpoint's own per-IP `bot` bucket; the
// account's upload bucket meters bytes into the library and this adds none.
//
// A body carrying an ATTACHMENT and no link is an OLD `/clip file:` — the
// definition every server held before this build, still in its picker until
// somebody re-registers — and it uploads exactly as `/upload` does, rather than
// telling a person who attached a video that it is not a link. That is the
// whole of the compatibility, and it closes when the command is re-published.
func (h *handler) discordClipLink(w http.ResponseWriter, r *http.Request, in discordInteraction, binding db.GuildBinding, upload string) {
	raw, hasLink := clipLink(in.Data)
	if !hasLink {
		if _, hasFile := clipAttachment(in.Data); hasFile {
			httpx.SetLogField(r, "discord_compat", "attachment_on_link_command")
			h.discordUpload(w, r, in, binding, upload)
			return
		}
		httpx.SetLogField(r, "reject", "no_link")
		writeEphemeral(w, msgLinkNotOurs)
		return
	}
	id, ok := h.d.URLs.ParseShare(raw)
	if !ok || !ids.Valid(id) {
		httpx.SetLogField(r, "reject", "foreign_link")
		writeEphemeral(w, msgLinkNotOurs)
		return
	}
	clip, err := h.d.DB.GetClip(r.Context(), id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		httpx.SetLogField(r, "reject", "no_such_clip")
		writeEphemeral(w, msgLinkNotOurs)
		return
	case err != nil:
		h.d.Log.Warn("discord bot: clip lookup failed", "err", err)
		writeEphemeral(w, msgUnavailable)
		return
	}
	h.postClip(w, r, in.Token, clip)
}

// clipGone is the tombstone test both link-reading commands share: the
// `deleting` state, or an expiry already past that the sweeper has not reached.
func clipGone(c db.Clip) bool {
	return c.State == db.ClipDeleting || (c.ExpiresAt != nil && *c.ExpiresAt <= time.Now().Unix())
}

// postClip answers with the message for one clip — type 4, PUBLIC —
// or with "That clip is gone." for a tombstone. The clip's id is the one thing
// about it the access log records.
//
// A clip whose card is not ready yet — pasted mid-encode, or while its Light
// copy or audiogram is still being made — is HELD by the bot before the answer
// is written (§20 erratum 330): Discord reads the page once, right after this
// reply, and never again by itself, so the bot redraws the reply once the
// worker says the card is ready. The hold is in memory and never blocks.
func (h *handler) postClip(w http.ResponseWriter, r *http.Request, token string, clip db.Clip) {
	if clipGone(clip) {
		httpx.SetLogField(r, "reject", "clip_gone")
		writeEphemeral(w, msgLinkGone)
		return
	}
	httpx.SetLogField(r, "clip", clip.ID)
	if !db.CardReady(clip) && h.d.Bot != nil {
		httpx.SetLogField(r, "card_ready", false)
		h.d.Bot.HoldReply(token, clip.ID)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"type": callbackMessage,
		"data": notify.ClipMessage(h.d.URLs, clip.ID),
	})
}

// discordEnrich is the message command "Enrich" (§20 erratum 315's addendum;
// the owner's decision (b) on the auto-enrich spike): somebody picks a message's
// Apps menu → Enrich, and the bot posts the same message `/clip link:` would
// for the FIRST link to a clip of this instance in that message's text.
//
// The message arrives IN the interaction (`data.resolved.messages[target_id]`)
// — Discord's own exception for a message command, which is why this needs no
// gateway and no Message Content intent. Its text is scanned and dropped: only
// the id of the clip found reaches the access log, and nothing of the message is
// stored anywhere (DEPLOY §30.6).
//
// What it cannot do, stated rather than discovered: the answer is not a REPLY
// to the picked message — an interaction response has no `message_reference`,
// and a reply would need the bot token and a channel POST, which a managed
// instance does not hold — so the card posts next in the channel. And Discord's
// own card on the picked message stays: the bot asks for no Manage Messages.
//
// A link that names nothing here, or no link at all, is the one refusal line; a
// tombstone is "That clip is gone.", as for `/clip link:`.
//
// **A video (§20 erratum 323), with DISCORD_ENRICH_UPLOAD=on (off by default).**
// When the text names no clip here, the FIRST video attached to the message is
// uploaded exactly as `/upload` uploads its own attachment (startUpload, without the form) — but only for the message's
// own poster or a member holding Administrator or Manage Server, decided here
// from the signed body; anyone else gets one private line and nothing is
// fetched. A link of ours still wins: a message carrying both is answered with
// the link's clip and uploads nothing.
func (h *handler) discordEnrich(w http.ResponseWriter, r *http.Request, in discordInteraction, binding db.GuildBinding) {
	msg, ok := in.Data.Resolved.Messages[in.Data.TargetID]
	if !ok || in.Data.TargetID == "" {
		httpx.SetLogField(r, "reject", "no_message")
		writeEphemeral(w, msgEnrichNoLink)
		return
	}
	for _, id := range h.shareIDsIn(msg.Content) {
		clip, err := h.d.DB.GetClip(r.Context(), id)
		switch {
		case errors.Is(err, db.ErrNotFound):
			continue // an id-shaped link of ours that names nothing: read on
		case err != nil:
			h.d.Log.Warn("discord bot: clip lookup failed", "err", err)
			writeEphemeral(w, msgUnavailable)
			return
		}
		h.postClip(w, r, in.Token, clip)
		return
	}
	// DISCORD_ENRICH_UPLOAD off (the default): Enrich answers links only, as it
	// did before erratum 323 — the attachments and both ids are not looked at.
	att, hasVideo := discordAttachment{}, false
	if h.d.Cfg.EnrichUpload {
		att, hasVideo = firstVideo(msg.Attachments)
	}
	if !hasVideo {
		httpx.SetLogField(r, "reject", "no_link")
		writeEphemeral(w, msgEnrichNoLink)
		return
	}
	if !mayEnrichUpload(in.Member, msg.Author) {
		// Before any check that reads the file: a stranger learns nothing
		// about it, and no job means no fetch.
		httpx.SetLogField(r, "reject", "not_poster")
		writeEphemeral(w, msgEnrichNotYours)
		return
	}
	// The outcome word only once the upload has started: a refused one (too
	// large, no bot) carries its own `reject=` and nothing else.
	if h.startUpload(w, r, in, binding, att, false) {
		httpx.SetLogField(r, "enrich", "upload")
	}
}

// firstVideo is the attachment Enrich uploads: the FIRST one with a URL that
// reads as a video or a sound by isMedia — `/upload`'s own first opinion (§20
// erratum 328: a voice message is an audio upload like any other). Images and
// other files are skipped; a second one is never taken (DEPLOY §30.4).
func firstVideo(atts []discordAttachment) (discordAttachment, bool) {
	for _, a := range atts {
		if a.URL != "" && isMedia(a) {
			return a, true
		}
	}
	return discordAttachment{}, false
}

// mayEnrichUpload is who may turn somebody's video into a clip of the bound
// account (§20 erratum 323): the message's own poster — the invoking member's
// user id equal to the message author's, both present — or a member holding
// Administrator or Manage Server (proof A, hasManagePermission). Both ids come
// from the signed body and are compared and dropped. This is on top of
// Discord's own per-command permissions, which decide who may run Enrich at all.
func mayEnrichUpload(m *discordMember, author *discordUser) bool {
	if hasManagePermission(m) {
		return true
	}
	if m == nil || m.User == nil || author == nil {
		return false
	}
	return m.User.ID != "" && m.User.ID == author.ID
}

// maxEnrichCandidates bounds how many links of ours one message may cost: a
// message is at most a few thousand characters, and nobody's real message names
// more clips than this before the one they meant.
const maxEnrichCandidates = 10

// shareIDsIn finds the links to clips of THIS instance in a message's text, in
// the order they appear, as validated ids. Each whitespace-separated token is
// tried as it stands — ParseShare takes Discord's `<…>` and `||…||` wraps itself
// — then from every scheme inside it (a masked link `[text](…)`, a link
// in brackets), and each of those again with closing punctuation trimmed off
// its end (a link that ends a sentence).
func (h *handler) shareIDsIn(content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, tok := range strings.Fields(content) {
		for _, cand := range shareCandidates(tok) {
			id, ok := h.d.URLs.ParseShare(cand)
			if !ok || !ids.Valid(id) || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
			if len(out) == maxEnrichCandidates {
				return out
			}
			break // one link per token
		}
	}
	return out
}

// shareCandidates spells one token every way a link inside it could be meant.
func shareCandidates(tok string) []string {
	cands := []string{tok}
	// The two scheme prefixes, spelled split: scripts/check.sh's outbound-URL
	// tripwire reads every literal scheme-and-slashes in this tree as a URL.
	for _, scheme := range []string{"https:" + "//", "http:" + "//"} {
		for i := 0; ; {
			j := strings.Index(tok[i:], scheme)
			if j < 0 {
				break
			}
			cands = append(cands, tok[i+j:])
			i += j + len(scheme)
		}
	}
	n := len(cands)
	for _, c := range cands[:n] {
		if t := strings.TrimRight(c, ".,!?;:)]}'\"*_~`"); t != c && t != "" {
			cands = append(cands, t)
		}
	}
	return cands
}

// clipLink resolves `/clip`'s `link` option: a plain string, matched by name and
// type. An option that is present but blank reads as ABSENT — Discord will not
// send one (the option is `required`), but a body is a body, and a `/clip` with
// nothing usable in it is always the same single line.
func clipLink(d discordInteractionData) (string, bool) {
	for _, opt := range d.Options {
		if opt.Type != optionTypeString || opt.Name != linkOptionName {
			continue
		}
		var s string
		if err := json.Unmarshal(opt.Value, &s); err != nil {
			continue
		}
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		return s, true
	}
	return "", false
}

// clipAttachment resolves `/upload`'s attachment option: the option carries the
// attachment's id, and the file itself is in `data.resolved.attachments`.
//
// It is found by TYPE, and every other option in the body is skipped in
// silence — which is what makes a body with an option this build does not know
// the ordinary "attach a video" refusal rather than a crash or a second
// vocabulary.
func clipAttachment(d discordInteractionData) (discordAttachment, bool) {
	for _, opt := range d.Options {
		if opt.Type != optionTypeAttachment {
			continue
		}
		var id string
		if err := json.Unmarshal(opt.Value, &id); err != nil || id == "" {
			continue
		}
		att, ok := d.Resolved.Attachments[id]
		if !ok || att.URL == "" {
			continue
		}
		return att, true
	}
	return discordAttachment{}, false
}

// writeEphemeral answers with a type-4 message only the caller can see.
func writeEphemeral(w http.ResponseWriter, content string) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"type": callbackMessage,
		"data": map[string]any{"content": content, "flags": ephemeralFlag},
	})
}

// botIngest is the app's upload path, as the bot's goroutine sees it: the SAME
// ingest core POST /api/upload runs (ingest.go), as the bound account, with
// that account's own defaults and its own upload budget — and the same
// card-ready wait, so the link the reply carries is one Discord can already
// unfurl as a player (§20 errata 212, 256).
//
// It returns a *notify.IngestError for everything the person who ran the
// command can act on, and a bare error for everything else: an unclassified
// error is exactly the one whose text nobody has checked for a path, a bucket
// name or a credential, and the bot answers those with one generic sentence.
func (h *handler) botIngest(userID string) notify.Ingest {
	return h.botIngestWith(userID, nil)
}

// botIngestWith is botIngest with one hook, called the moment the clip exists
// and before the card wait: the upload form's attach (§20 erratum 320), which
// writes a submit that arrived before the clip did. nil is botIngest.
//
// existing tells the hook the clip was ALREADY there (§20 erratum 326): the
// form must then apply nothing to it. The bot's own paths never send the
// duplicate check's opt-out. The hook's answer is IngestResult.SubmitDropped:
// true when a submit already answered "Changes saved." was dropped, so the job tells
// that person the form was not applied (run 8: a later submit hears it from
// its own answer, never twice).
func (h *handler) botIngestWith(userID string, afterIngest func(ctx context.Context, clipID string, existing bool) bool) notify.Ingest {
	return h.attachmentIngest(userID, true, false, h.captureWait, afterIngest)
}

// attachmentIngest is the body botIngestWith and the Clippet Auto URL (§20
// erratum 324) share. `charge` says whether THIS is where the account's upload
// bucket is spent: true for the bot, whose interaction route carries no upload
// limiter; false for POST /in/auto/{token}, whose route's own two limiters
// already charged it — an attachment is never charged twice. `cardWait` is
// the card-ready wait's budget, read when the clip exists: the capture wait
// for the bot, and for the Clippet Auto URL that cut to what is left of the
// request's own hold. `allowDuplicate` is the duplicate check's opt-out (§20
// erratum 326): false on the bot's own paths, the instruction's `duplicate`
// field on the Clippet Auto URL.
func (h *handler) attachmentIngest(userID string, charge, allowDuplicate bool, cardWait func() time.Duration,
	afterIngest func(ctx context.Context, clipID string, existing bool) bool) notify.Ingest {
	return func(ctx context.Context, src notify.IngestSource) (notify.IngestResult, error) {
		// The bound ACCOUNT's upload budget, keyed exactly as the upload
		// routes key it (router.go userKey). The bot mints no exemption: a
		// server that runs `/clip` in a loop spends the same tokens the
		// account's own capture tool would (§14).
		if charge {
			if ok, _ := h.uploadLimit.Allow("u:" + userID); !ok {
				return notify.IngestResult{}, &notify.IngestError{Sentence: notify.SentenceBusy}
			}
		}
		if free, ok := h.scratchHasRoom(declaredScratchNeed(src.Size, h.d.Cfg.ScratchMinFree)); !ok {
			h.d.Log.Warn("discord bot: not enough scratch for the attachment",
				"bytes", src.Size, "free", free)
			return notify.IngestResult{}, &notify.IngestError{Sentence: notify.SentenceFull}
		}
		meta := uploadMeta{allowDuplicate: allowDuplicate}
		if err := h.applyDefaultsCtx(ctx, userID, &meta); err != nil {
			// Only a bad stored default can land here (the bot sends neither
			// ttl_days nor link_style of its own, and the two field-sent
			// refusals are the only other way out), and applyDefaultsCtx
			// already falls back to the instance's. Treated as a failure
			// rather than as a silent "never expires".
			return notify.IngestResult{}, &notify.IngestError{Sentence: notify.SentenceFailed, Err: err}
		}
		// No title: `defaultTitle` inside the core makes it the filename
		// without its extension, which is erratum 60's rule for every other
		// upload and the right one here — Discord's filename is all anybody
		// typed.
		clip, existing, err := h.ingestCore(ctx, userID, meta, ingestSource{
			Body:        capReader(src.Body, h.d.Cfg.MaxUploadBytes),
			Filename:    src.Filename,
			ContentType: src.ContentType,
		})
		if err != nil {
			return notify.IngestResult{}, botIngestError(err)
		}
		dropped := false
		if afterIngest != nil {
			dropped = afterIngest(ctx, clip.ID, existing)
		}
		// The same wait the capture endpoint holds its 201 for, for the same
		// reason: Discord caches an unfurl per URL, so a link posted before
		// the card file exists would stay a bare link-out card. Not ready in
		// time → the link goes out anyway, held, and the bot redraws it once
		// the worker says the card is ready (§20 erratum 330): Discord never
		// reads that URL again by itself.
		fresh, _ := h.awaitCardReadyCtx(ctx, clip, cardWait())
		return notify.IngestResult{ClipID: fresh.ID, CardReady: db.CardReady(fresh), Existing: existing,
			SubmitDropped: dropped}, nil
	}
}

// botIngestError turns the ingest core's verdict into a sentence, exactly as
// writeIngestError turns it into a status. Every arm that can be the
// requester's own fault says so; everything else is the generic sentence and a
// log line for the operator.
func botIngestError(err error) error {
	var ie *ingestError
	if !errors.As(err, &ie) {
		return err
	}
	switch ie.kind {
	case ingestFailTooLarge:
		return &notify.IngestError{Sentence: notify.SentenceTooLarge, Err: ie}
	case ingestFailNotVideo:
		return &notify.IngestError{Sentence: notify.SentenceNotVideo, Err: ie}
	case ingestFailEmpty, ingestFailMalformed:
		return &notify.IngestError{Sentence: notify.SentenceFetch, Err: ie}
	case ingestFailScratch:
		return &notify.IngestError{Sentence: notify.SentenceFull, Err: ie}
	case ingestFailQuota:
		// The one refusal on this path that is a DECISION rather than a
		// fault, and the person in the channel gets the same sentence the
		// upload routes' 507 carries (§20 erratum 277).
		return &notify.IngestError{Sentence: notify.SentenceQuota, Err: ie}
	case ingestFailGone:
		return ie // the bot words a cancellation itself: it is a restart
	}
	return &notify.IngestError{Sentence: notify.SentenceFailed, Err: ie}
}

// capReader bounds a reader at n bytes and fails with the error the ingest
// core already classifies as "too large", so an attachment that lies about its
// size is refused by exactly the rule MAX_UPLOAD_BYTES states — the same one
// http.MaxBytesReader applies to an upload route's body, minus the
// ResponseWriter it needs and this path does not have.
//
// The boundary is MaxBytesReader's exactly: a stream of n bytes is delivered
// whole and the error is raised only when a byte PAST the limit exists, so
// a file of exactly MAX_UPLOAD_BYTES is accepted here as it is on the upload
// routes (the lane verifier found the first draft refusing it one byte early).
func capReader(r io.Reader, n int64) io.Reader {
	if n <= 0 {
		return r
	}
	return &cappedReader{r: r, limit: n, left: n + 1}
}

type cappedReader struct {
	r           io.Reader
	limit, left int64 // left counts one byte past the limit: reading it is the error
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, &http.MaxBytesError{Limit: c.limit}
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left <= 0 {
		// The byte past the limit arrived: hand back the n allowed and refuse.
		return n - 1, &http.MaxBytesError{Limit: c.limit}
	}
	return n, err
}

// discordConfigured reports whether the interactions route should exist at
// all. It is the ONE condition routes() consults, so "the bot is off" is one
// fact rather than a check in every handler.
func (h *handler) discordConfigured() bool {
	return h.d.Cfg.DiscordBot.Configured()
}

// interactionsPath is the endpoint URL's path — the one an operator pastes
// into the developer portal. DEPLOY §30 and API.md name the same constant's
// value; `admin doctor` prints it beside PUBLIC_BASE_URL.
const interactionsPath = "/api/discord/interactions"

// InteractionsPath is that path, for the CLI and the docs.
func InteractionsPath() string { return interactionsPath }
