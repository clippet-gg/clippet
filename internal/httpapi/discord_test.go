package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/notify"
	"clippet/internal/transcode"
)

// The interactions endpoint (ROADMAP #33, §20 errata 253-256, 268, 286). Every
// test here drives the REAL route table, so "the route does not exist when the
// bot is off" is proven the same way every other route is.

// recorderBot is httpapi.DiscordBot with nothing behind it: the endpoint's job
// is to hand over a well-formed ClipJob, and internal/notify's own tests prove
// what happens to it afterwards.
type recorderBot struct {
	mu      sync.Mutex
	jobs    []notify.ClipJob
	fetches []notify.Attachment
	redraws []string
	holds   []string // "token clip" per HoldReply (§20 erratum 330)
}

// HoldReply records a type-4 reply held for a late card (§20 erratum 330); the
// redraw itself is internal/notify's, proven there.
func (b *recorderBot) HoldReply(token, clipID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.holds = append(b.holds, token+" "+clipID)
}

func (b *recorderBot) held() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.holds...)
}

// Redraw records the clip a late form submit asked to redraw (§20 erratum
// 325); the edit itself is internal/notify's, proven there.
func (b *recorderBot) Redraw(clipID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.redraws = append(b.redraws, clipID)
}

func (b *recorderBot) redrawn() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.redraws...)
}

func (b *recorderBot) Clip(job notify.ClipJob) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.jobs = append(b.jobs, job)
}

// Fetch records a Clippet Auto job (§20 erratum 324) and ends it at once with
// an unclassified error; the real fetcher is exercised through a real
// notify.Bot in autofetch_test.go.
func (b *recorderBot) Fetch(att notify.Attachment, _ notify.Ingest) (<-chan notify.FetchOutcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fetches = append(b.fetches, att)
	out := make(chan notify.FetchOutcome, 1)
	out <- notify.FetchOutcome{Err: errors.New("recorder: nothing fetched")}
	return out, nil
}

func (b *recorderBot) fetchCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.fetches)
}

func (b *recorderBot) last() (notify.ClipJob, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.jobs) == 0 {
		return notify.ClipJob{}, false
	}
	return b.jobs[len(b.jobs)-1], true
}

func (b *recorderBot) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.jobs)
}

// botEnv is an env whose bot is configured, plus the private key the test
// signs with. The key is minted per test: nothing here is a fixture.
type botEnv struct {
	*env
	priv ed25519.PrivateKey
	bot  *recorderBot
}

func newBotEnv(t *testing.T, mutate ...func(*Deps)) *botEnv {
	t.Helper()
	return newBotEnvNamed(t, "", mutate...)
}

// newBotEnvNamed is newBotEnv with DISCORD_COMMAND_NAME set (§20 erratum 284).
// "" is the shipped shape — no variable, the default name.
func newBotEnvNamed(t *testing.T, command string, mutate ...func(*Deps)) *botEnv {
	t.Helper()
	return newBotEnvWith(t, map[string]string{config.EnvDiscordCommandName: command}, mutate...)
}

// newBotEnvWith is newBotEnv with any of the bot's variables set over the
// defaults — the two command names (§20 erratum 315), the managed sentinel.
func newBotEnvWith(t *testing.T, vars map[string]string, mutate ...func(*Deps)) *botEnv {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorderBot{}
	all := append([]func(*Deps){func(d *Deps) {
		d.Cfg.DiscordBot = testBotConfigWith(t, pub, vars)
		d.Bot = rec
	}}, mutate...)
	return &botEnv{env: newEnv(t, all...), priv: priv, bot: rec}
}

// testBotConfig builds a real config.DiscordBot by going through config.Load,
// so the tests below cannot pass on a value the loader would refuse. `command`
// is DISCORD_COMMAND_NAME and "" leaves it unset, which is the shipped shape.
func testBotConfig(t *testing.T, pub ed25519.PublicKey, command string) config.DiscordBot {
	t.Helper()
	return testBotConfigWith(t, pub, map[string]string{config.EnvDiscordCommandName: command})
}

// testBotConfigWith is testBotConfig with any of the bot's variables laid over
// the shipped shape.
func testBotConfigWith(t *testing.T, pub ed25519.PublicKey, vars map[string]string) config.DiscordBot {
	t.Helper()
	env := map[string]string{
		"PUBLIC_BASE_URL":          testOrigin,
		"S3_ENDPOINT":              "http://minio:9000",
		"S3_BUCKET":                "b",
		"S3_ACCESS_KEY":            "a",
		"S3_SECRET_KEY":            "s",
		config.EnvDiscordAppID:     "123456789012345678",
		config.EnvDiscordPublicKey: hex.EncodeToString(pub),
		config.EnvDiscordBotToken:  "MTIzNDU2Nzg5MDEyMzQ1Njc4.GhIjKl.mNoPqRsTuVwXyZ0123456789abcdef",
	}
	for k, v := range vars {
		env[k] = v
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg.DiscordBot
}

// renameCommand rewrites the command NAME in an interaction body built by the
// helpers below — the one thing DISCORD_COMMAND_NAME changes about what Discord
// sends (§20 erratum 284).
func renameCommand(t *testing.T, body, name string) string {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	data, ok := v["data"].(map[string]any)
	if !ok {
		t.Fatalf("the body carries no data object: %s", body)
	}
	data["name"] = name
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// post signs the body the way Discord does — ed25519 over timestamp||body —
// and sends it to the endpoint.
func (b *botEnv) post(body string, opts ...reqOpt) resp {
	b.t.Helper()
	ts := strconv.FormatInt(b.now(), 10) // signed with the clock: the box admits ±300 s (§20 erratum 279 (g))
	sig := ed25519.Sign(b.priv, []byte(ts+body))
	opts = append([]reqOpt{
		withHeader("X-Signature-Ed25519", hex.EncodeToString(sig)),
		withHeader("X-Signature-Timestamp", ts),
		withJSON(),
	}, opts...)
	return b.do(http.MethodPost, interactionsPath, strings.NewReader(body), opts...)
}

// postLogged is post through the access log, for the ONE annotation an operator
// greps to see which command an interaction was (§20 erratum 284, amended).
func (b *botEnv) postLogged(body string, opts ...reqOpt) (resp, string) {
	b.t.Helper()
	ts := strconv.FormatInt(b.now(), 10) // signed with the clock: the box admits ±300 s (§20 erratum 279 (g))
	sig := ed25519.Sign(b.priv, []byte(ts+body))
	opts = append([]reqOpt{
		withHeader("X-Signature-Ed25519", hex.EncodeToString(sig)),
		withHeader("X-Signature-Timestamp", ts),
		withJSON(),
	}, opts...)
	return b.logged(http.MethodPost, interactionsPath, strings.NewReader(body), opts...)
}

// clipBody is an `/upload` interaction with one attachment (the upload command
// was `/clip` until §20 erratum 315; the helper keeps its name).
func clipBody(guildID, filename, contentType, url string, size int64) string {
	body, _ := json.Marshal(map[string]any{
		"type": interactionCommand, "token": "int-token-abc", "guild_id": guildID,
		"data": map[string]any{
			"name":    config.DefaultUploadCommandName,
			"options": []any{map[string]any{"name": "file", "type": optionTypeAttachment, "value": "999"}},
			"resolved": map[string]any{"attachments": map[string]any{
				"999": map[string]any{"id": "999", "filename": filename, "url": url,
					"size": size, "content_type": contentType},
			}},
		},
	})
	return string(body)
}

// legacyOptionTypeString is Discord's STRING type, spelled apart from the
// endpoint's own optionTypeString on purpose: these two bodies are an UPLOAD
// command carrying a string option it never had — §20 erratum 286's withdrawn
// `link`, which since erratum 315 lives on its own command — and what is proven
// is that the upload command reads it as no attachment, not as a link.
const legacyOptionTypeString = 3

// legacyLinkBody is an `/upload` carrying that option and nothing else, and
// legacyBothBody one carrying it BESIDE a real attachment.
func legacyLinkBody(guildID, link string) string {
	body, _ := json.Marshal(map[string]any{
		"type": interactionCommand, "token": "int-token-abc", "guild_id": guildID,
		"data": map[string]any{
			"name": config.DefaultUploadCommandName,
			"options": []any{map[string]any{
				"name": "link", "type": legacyOptionTypeString, "value": link,
			}},
		},
	})
	return string(body)
}

func legacyBothBody(guildID, link string) string {
	body, _ := json.Marshal(map[string]any{
		"type": interactionCommand, "token": "int-token-abc", "guild_id": guildID,
		"data": map[string]any{
			"name": config.DefaultUploadCommandName,
			"options": []any{
				map[string]any{"name": "file", "type": optionTypeAttachment, "value": "999"},
				map[string]any{"name": "link", "type": legacyOptionTypeString, "value": link},
			},
			"resolved": map[string]any{"attachments": map[string]any{
				"999": map[string]any{"id": "999", "filename": "clip.mp4",
					"url": "https://cdn.discordapp.com/x/clip.mp4", "size": 10, "content_type": "video/mp4"},
			}},
		},
	})
	return string(body)
}

func TestInteractionsRouteAbsentWhenTheBotIsOff(t *testing.T) {
	e := newEnv(t) // no DISCORD_* at all: the default
	r := e.do(http.MethodPost, interactionsPath, strings.NewReader(`{"type":1}`), withJSON())
	if r.code != http.StatusNotFound || r.errCode() != "not_found" {
		t.Fatalf("with the bot off the endpoint answered %d %s, want 404 not_found", r.code, r.errCode())
	}
}

func TestInteractionsBadSignatureIs401WithNoBody(t *testing.T) {
	b := newBotEnv(t)
	body := `{"type":1}`
	// Discord's endpoint validation sends a well-formed but WRONG signature
	// and requires a 401; an endpoint that answers 200 is refused in the
	// portal.
	r := b.do(http.MethodPost, interactionsPath, strings.NewReader(body),
		withHeader("X-Signature-Ed25519", hex.EncodeToString(make([]byte, ed25519.SignatureSize))),
		withHeader("X-Signature-Timestamp", strconv.FormatInt(b.now(), 10)), withJSON())
	if r.code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d, want 401", r.code)
	}
	if len(bytes.TrimSpace(r.body)) != 0 {
		t.Fatalf("the 401 carried a body: %q", r.body)
	}
	for _, missing := range []string{"", "not-hex", hex.EncodeToString([]byte("short"))} {
		r := b.do(http.MethodPost, interactionsPath, strings.NewReader(body),
			withHeader("X-Signature-Ed25519", missing),
			withHeader("X-Signature-Timestamp", strconv.FormatInt(b.now(), 10)), withJSON())
		if r.code != http.StatusUnauthorized {
			t.Fatalf("signature %q = %d, want 401", missing, r.code)
		}
	}
	// A valid signature over a DIFFERENT timestamp must not verify: the
	// timestamp is part of the signed message, which is what stops one
	// signature from being replayed over another body.
	now := b.now()
	sig := ed25519.Sign(b.priv, []byte(strconv.FormatInt(now, 10)+body))
	r = b.do(http.MethodPost, interactionsPath, strings.NewReader(body),
		withHeader("X-Signature-Ed25519", hex.EncodeToString(sig)),
		withHeader("X-Signature-Timestamp", strconv.FormatInt(now+1, 10)), withJSON())
	if r.code != http.StatusUnauthorized {
		t.Fatalf("signature over another timestamp = %d, want 401", r.code)
	}
}

// postAt is post with the timestamp given, signed over exactly that string,
// through the access log.
func (b *botEnv) postAt(ts, body string) (resp, string) {
	b.t.Helper()
	sig := ed25519.Sign(b.priv, []byte(ts+body))
	return b.logged(http.MethodPost, interactionsPath, strings.NewReader(body),
		withHeader("X-Signature-Ed25519", hex.EncodeToString(sig)),
		withHeader("X-Signature-Timestamp", ts), withJSON())
}

// TestInteractionsTimestampWindow: a genuinely signed interaction is admitted
// only when its X-Signature-Timestamp is within ±300 s of the box's clock —
// the forwarder's window (§20 erratum 279 (g), amended). Outside it, or with a
// missing or non-numeric timestamp, the answer is the bad signature's: 401,
// no body. A genuine signature over a stale time logs `stale_timestamp`, the
// one verdict that points at this box's clock.
func TestInteractionsTimestampWindow(t *testing.T) {
	b := newBotEnv(t)
	body := `{"type":1}`
	// The harness clock (e.now(), never backwards) against the handler's wall
	// clock: the two can differ by a step of the container's clock, which is
	// why the inside case is 240 s and not 299.
	at := func(d time.Duration) string { return strconv.FormatInt(b.now()+int64(d/time.Second), 10) }
	if r, _ := b.postAt(at(0), body); r.code != http.StatusOK {
		t.Fatalf("a fresh signed PING = %d, want 200", r.code)
	}
	if r, _ := b.postAt(at(-240*time.Second), body); r.code != http.StatusOK {
		t.Fatalf("a PING signed 240 s ago = %d, want 200 (inside the window)", r.code)
	}
	for _, c := range []struct {
		name, ts, reject string
	}{
		{"ten minutes old", at(-600 * time.Second), "stale_timestamp"},
		{"ten minutes in the future", at(600 * time.Second), "stale_timestamp"},
		{"missing", "", "bad_signature"},
		{"not a number", "17575x0000", "bad_signature"},
		{"with a sign", "-" + at(0), "bad_signature"},
		{"spaced", " " + at(0), "bad_signature"},
	} {
		r, log := b.postAt(c.ts, body)
		if r.code != http.StatusUnauthorized {
			t.Fatalf("timestamp %s (%q) = %d, want 401", c.name, c.ts, r.code)
		}
		if len(bytes.TrimSpace(r.body)) != 0 {
			t.Fatalf("timestamp %s: the 401 carried a body: %q", c.name, r.body)
		}
		if !strings.Contains(log, "reject="+c.reject) {
			t.Fatalf("timestamp %s: the log does not say reject=%s:\n%s", c.name, c.reject, log)
		}
		if strings.Contains(log, `"type"`) {
			t.Fatalf("timestamp %s: the log carries the body:\n%s", c.name, log)
		}
	}
}

// TestInteractionFreshIsPlusOrMinus300Seconds: the window's edges, on a fixed
// clock (the handler's own is the wall's, which a test cannot hold still):
// 300 s either way is inside, 301 s either way is out.
func TestInteractionFreshIsPlusOrMinus300Seconds(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, c := range []struct {
		age  int64
		want bool
	}{{0, true}, {300, true}, {-300, true}, {301, false}, {-301, false}, {86400, false}} {
		if got := interactionFresh(now, now.Unix()-c.age); got != c.want {
			t.Errorf("a timestamp %d s old: fresh = %v, want %v", c.age, got, c.want)
		}
	}
}

func TestInteractionsPingAnswersPong(t *testing.T) {
	b := newBotEnv(t)
	r := b.post(`{"type":1}`)
	if r.code != http.StatusOK {
		t.Fatalf("PING = %d, want 200: %s", r.code, r.body)
	}
	if got, _ := r.json["type"].(float64); int(got) != callbackPong {
		t.Fatalf("PING answered type %v, want %d (PONG)", r.json["type"], callbackPong)
	}
}

func TestInteractionsUnboundGuildIsRefusedEphemerally(t *testing.T) {
	b := newBotEnv(t)
	r := b.post(clipBody("111111111111111111", "clip.mp4", "video/mp4", "https://cdn.discordapp.com/x/clip.mp4", 10))
	if r.code != http.StatusOK {
		t.Fatalf("unbound /clip = %d, want 200: %s", r.code, r.body)
	}
	assertEphemeral(t, r, msgUnbound)
	if b.bot.count() != 0 {
		t.Fatal("an unbound guild handed work to the bot")
	}
}

func TestInteractionsBoundGuildDefersAndHandsOverTheJob(t *testing.T) {
	b := newBotEnv(t)
	u, _ := b.newUser("guildowner")
	const guild = "222222222222222222"
	if _, err := b.db.BindGuild(context.Background(), guild, u.ID, b.now()); err != nil {
		t.Fatal(err)
	}
	const cdn = "https://cdn.discordapp.com/attachments/1/2/clip.mp4"
	r := b.post(clipBody(guild, "clip.mp4", "video/mp4", cdn, 4096))
	if r.code != http.StatusOK {
		t.Fatalf("bound /clip = %d, want 200: %s", r.code, r.body)
	}
	if got, _ := r.json["type"].(float64); int(got) != callbackDeferredMessage {
		t.Fatalf("bound /clip answered type %v, want %d (deferred)", r.json["type"], callbackDeferredMessage)
	}
	if _, ok := r.json["data"]; ok {
		t.Fatalf("the deferred answer carried data (it must be public and empty): %s", r.body)
	}
	job, ok := b.bot.last()
	if !ok {
		t.Fatal("no job was handed to the bot")
	}
	if job.URL != cdn || job.Filename != "clip.mp4" || job.Size != 4096 || job.GuildID != guild {
		t.Fatalf("the job does not carry the attachment: %+v", job)
	}
	if job.Token != "int-token-abc" {
		t.Fatalf("the job does not carry the interaction token: %q", job.Token)
	}
	if job.Ingest == nil {
		t.Fatal("the job carries no ingest")
	}
	if job.Command != "/upload" {
		t.Fatalf("the job's log lines would name %q, want /upload", job.Command)
	}
}

func TestInteractionsRefusesWhatItCanSeeIsWrong(t *testing.T) {
	b := newBotEnv(t)
	u, _ := b.newUser("guildowner2")
	const guild = "333333333333333333"
	if _, err := b.db.BindGuild(context.Background(), guild, u.ID, b.now()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, body, want string
	}{
		{"not a video", clipBody(guild, "notes.txt", "text/plain", "https://cdn.discordapp.com/x/notes.txt", 10), notify.SentenceNotVideo},
		{"too large", clipBody(guild, "big.mp4", "video/mp4", "https://cdn.discordapp.com/x/big.mp4", 1<<30), notify.SentenceTooLarge},
		{"no option at all", `{"type":2,"token":"t","guild_id":"` + guild + `","data":{"name":"` + config.DefaultUploadCommandName + `"}}`, msgNoInput(config.DefaultUploadCommandName)},
		// A string option on the UPLOAD command (§20 erratum 286's withdrawn
		// `link`, which since erratum 315 lives on its own command): an option
		// this command does not know is not an attachment, so it reads as "no
		// attachment" and gets the one line — whatever it carries.
		{"a link option on the upload command", legacyLinkBody(guild, testOrigin+"/AbcDefGhiJkl"), msgNoInput(config.DefaultUploadCommandName)},
		{"…naming another host", legacyLinkBody(guild, "https://clips.elsewhere.test/AbcDefGhiJkl"), msgNoInput(config.DefaultUploadCommandName)},
		{"…blank", legacyLinkBody(guild, "   "), msgNoInput(config.DefaultUploadCommandName)},
		{"another command", `{"type":2,"token":"t","guild_id":"` + guild + `","data":{"name":"whatever"}}`, msgUnknown},
		{"a DM", clipBody("", "clip.mp4", "video/mp4", "https://cdn.discordapp.com/x/clip.mp4", 10), msgNotInGuild(config.DefaultUploadCommandName)},
		{"an unknown interaction type", `{"type":9}`, msgUnknown},
		{"a body that is not an interaction", `[]`, msgUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := b.post(c.body)
			if r.code != http.StatusOK {
				t.Fatalf("%s = %d, want 200: %s", c.name, r.code, r.body)
			}
			assertEphemeral(t, r, c.want)
		})
	}
	if b.bot.count() != 0 {
		t.Fatalf("%d job(s) were handed over for interactions that should have been refused", b.bot.count())
	}
}

// assertEphemeral checks a type-4 response that only its caller can see, and
// that it carries the expected line and nothing else — no embed, no component,
// no CTA (§15 anti-features).
func assertEphemeral(t *testing.T, r resp, want string) {
	t.Helper()
	if got, _ := r.json["type"].(float64); int(got) != callbackMessage {
		t.Fatalf("type %v, want %d (message): %s", r.json["type"], callbackMessage, r.body)
	}
	data, ok := r.json["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data object: %s", r.body)
	}
	if content, _ := data["content"].(string); !strings.HasPrefix(content, want) {
		t.Fatalf("content = %q, want it to start with %q", content, want)
	}
	if flags, _ := data["flags"].(float64); int(flags) != ephemeralFlag {
		t.Fatalf("flags = %v, want %d (ephemeral)", data["flags"], ephemeralFlag)
	}
	for _, forbidden := range []string{"embeds", "components", "attachments"} {
		if _, bad := data[forbidden]; bad {
			t.Fatalf("the refusal carries %q: %s", forbidden, r.body)
		}
	}
}

// boundIngest binds a guild, runs a `/clip` through the REAL endpoint and
// hands back the ingest callback the endpoint built — so every test below
// exercises the path an interaction actually takes, callback included.
func (b *botEnv) boundIngest(t *testing.T, name, guild string) (db.User, notify.Ingest) {
	t.Helper()
	u, _ := b.newUser(name)
	if _, err := b.db.BindGuild(context.Background(), guild, u.ID, b.now()); err != nil {
		t.Fatal(err)
	}
	r := b.post(clipBody(guild, "clip.mp4", "video/mp4", "https://cdn.discordapp.com/x/clip.mp4", 16))
	if got, _ := r.json["type"].(float64); int(got) != callbackDeferredMessage {
		t.Fatalf("bound /clip answered %s", r.body)
	}
	job, ok := b.bot.last()
	if !ok || job.Ingest == nil {
		t.Fatal("the endpoint handed over no ingest")
	}
	return u, job.Ingest
}

// TestBotIngestUploadsAsTheBoundAccount drives the callback the endpoint hands
// over: it must create a clip owned by the BOUND account, through the same
// ingest core an upload route uses.
func TestBotIngestUploadsAsTheBoundAccount(t *testing.T) {
	b := newBotEnv(t)
	u, ingest := b.boundIngest(t, "botowner", "444444444444444444")
	res, err := ingest(context.Background(), notify.IngestSource{
		Body: strings.NewReader("fake video bytes"), Filename: "Match Point.mp4",
		ContentType: "video/mp4", Size: 16,
	})
	if err != nil {
		t.Fatalf("botIngest: %v", err)
	}
	clip := b.getClip(res.ClipID)
	if clip.OwnerID != u.ID {
		t.Fatalf("the clip is owned by %s, want the bound account %s", clip.OwnerID, u.ID)
	}
	// Erratum 60: the title is the filename without its extension, exactly as
	// every other upload route's fallback renders it.
	if clip.Title != "Match Point" {
		t.Fatalf("title = %q, want the filename without its extension", clip.Title)
	}
	if clip.State != db.ClipQueued {
		t.Fatalf("state = %q, want queued (the worker has not run)", clip.State)
	}
	if b.worker.woken.Load() == 0 {
		t.Fatal("the worker was not woken")
	}
	if res.CardReady {
		t.Fatal("a queued clip cannot be card-ready")
	}
}

// TestBotIngestAppliesTheAccountDefaults: the bot sends no metadata at all, so
// the account's own TTL and visibility are what a `/clip` clip gets (ROADMAP
// #22's defaults, reached through applyDefaultsCtx).
func TestBotIngestAppliesTheAccountDefaults(t *testing.T) {
	b := newBotEnv(t)
	u, ingest := b.boundIngest(t, "defaulter", "555555555555555555")
	days := 7
	if err := b.db.SetUserDefaults(context.Background(), u.ID, db.UserDefaults{
		TTLDays: &days, Unlisted: true, LinkStyle: db.LinkStyleMemorable,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := ingest(context.Background(), notify.IngestSource{
		Body: strings.NewReader("bytes"), Filename: "x.mp4", ContentType: "video/mp4", Size: 5,
	})
	if err != nil {
		t.Fatalf("botIngest: %v", err)
	}
	clip := b.getClip(res.ClipID)
	if !clip.Unlisted {
		t.Fatal("the account defaults to unlisted and the clip is listed")
	}
	if clip.ExpiresAt == nil {
		t.Fatal("the account defaults to a 7-day TTL and the clip never expires")
	}
	// The id grammar is an account default like the other two (§20 errata 250,
	// 256), and the WORD COUNT follows the visibility this upload resolved to —
	// so a bot upload into an account that defaults to unlisted draws four
	// words, exactly as a web upload would. That is only true because the id is
	// minted inside the core AFTER applyDefaultsCtx has settled both, which is
	// the one thing the two lanes could have merged wrong.
	if ids.Kind(clip.ID) != ids.KindMemorable || wordsIn(clip.ID) != ids.MemorableMaxWords {
		t.Fatalf("the bot minted %q (%s, %d words), want a four-word memorable id",
			clip.ID, ids.Kind(clip.ID), wordsIn(clip.ID))
	}
}

// TestBotIngestRefusalsAreSentences pins the mapping every failure the ingest
// core can produce goes through on its way to a Discord message: the requester
// gets one plain line and nothing else leaks.
func TestBotIngestRefusalsAreSentences(t *testing.T) {
	b := newBotEnv(t)
	_, ingest := b.boundIngest(t, "refused", "666666666666666666")
	b.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
		return nil, &transcode.ErrPermanent{Err: errors.New("moov atom not found")}
	}
	_, err := ingest(context.Background(), notify.IngestSource{
		Body: strings.NewReader("not a video"), Filename: "x.mp4", ContentType: "video/mp4", Size: 11,
	})
	var ie *notify.IngestError
	if !errors.As(err, &ie) || ie.Sentence != notify.SentenceNotVideo {
		t.Fatalf("a file ffprobe rejects = %v, want the not-a-video sentence", err)
	}
	// The operator detail is still there for the log, and the SENTENCE is what
	// the bot shows: internal/notify only reads Sentence.
	if !strings.Contains(ie.Error(), "moov atom") {
		t.Fatalf("the cause was dropped: %v", ie)
	}
}

// TestBotIngestSpendsTheAccountsUploadBudget proves the §14 hook: the bot is
// not an exemption from the account's own limiter.
func TestBotIngestSpendsTheAccountsUploadBudget(t *testing.T) {
	b := newBotEnv(t, func(d *Deps) { d.Cfg.RateUploadPerHour = 1 })
	_, ingest := b.boundIngest(t, "botowner3", "777777777777777777")
	if _, err := ingest(context.Background(), notify.IngestSource{
		Body: strings.NewReader("first"), Filename: "a.mp4", ContentType: "video/mp4", Size: 5,
	}); err != nil {
		t.Fatalf("the first /clip: %v", err)
	}
	_, err := ingest(context.Background(), notify.IngestSource{
		Body: strings.NewReader("second"), Filename: "b.mp4", ContentType: "video/mp4", Size: 6,
	})
	var ie *notify.IngestError
	if !errors.As(err, &ie) || ie.Sentence != notify.SentenceBusy {
		t.Fatalf("the second /clip = %v, want the 'slow down' sentence", err)
	}
}

// TestBotIngestCapsTheStreamAtMaxUploadBytes proves the size guard on an
// attachment that lies about its length: MAX_UPLOAD_BYTES is enforced on the
// BYTES, not on Discord's claim about them.
func TestBotIngestCapsTheStreamAtMaxUploadBytes(t *testing.T) {
	b := newBotEnv(t, func(d *Deps) { d.Cfg.MaxUploadBytes = 32 })
	_, ingest := b.boundIngest(t, "botowner4", "888888888888888888")
	_, err := ingest(context.Background(), notify.IngestSource{
		Body: io.LimitReader(zeroes{}, 1<<16), Filename: "big.mp4", ContentType: "video/mp4",
		Size: 4, // the lie
	})
	var ie *notify.IngestError
	if !errors.As(err, &ie) || ie.Sentence != notify.SentenceTooLarge {
		t.Fatalf("an oversized stream = %v, want the too-large sentence", err)
	}
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { return len(p), nil }

// TestClipCommandNameMatchesTheRegistration pins the spellings §3 forbids
// sharing by import: the DEFAULT names internal/notify publishes, the defaults
// internal/config hands BOTH sides (§20 errata 284, 315), and the option types
// both use. Since erratum 315 every pin has its twin: the upload command's name
// beside the link command's, the STRING type beside the ATTACHMENT one, and the
// link option's name.
func TestClipCommandNameMatchesTheRegistration(t *testing.T) {
	if got := notify.ClipCommandName(); got != config.DefaultCommandName {
		t.Fatalf("internal/notify registers %q by default, internal/config's default is %q", got, config.DefaultCommandName)
	}
	if got := notify.UploadCommandName(); got != config.DefaultUploadCommandName {
		t.Fatalf("internal/notify registers the upload command as %q by default, internal/config's default is %q", got, config.DefaultUploadCommandName)
	}
	// …and the loader is the third spelling, the one the other two are useless
	// without: an instance that configures no name of its own answers it.
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := testBotConfig(t, pub, "").CommandName(); got != config.DefaultCommandName {
		t.Fatalf("a bot configured without %s answers %q, want %q", config.EnvDiscordCommandName, got, config.DefaultCommandName)
	}
	if got := testBotConfig(t, pub, "").UploadCommandName(); got != config.DefaultUploadCommandName {
		t.Fatalf("a bot configured without %s answers %q, want %q", config.EnvDiscordUploadCommandName, got, config.DefaultUploadCommandName)
	}
	// The ATTACHMENT type, which since §20 erratum 286 is the type of the
	// command's ONE option: a drift here would make every `/clip` answer
	// "Attach a video to /clip." with the video attached.
	if got := notify.ClipOptionType(); got != optionTypeAttachment {
		t.Fatalf("internal/notify registers option type %d, this endpoint reads %d", got, optionTypeAttachment)
	}
	// …and the STRING type and the option's NAME, which `/clip link:` is found
	// by: a drift in either would answer every `/clip` with the not-ours line.
	if got := notify.LinkOptionType(); got != optionTypeString {
		t.Fatalf("internal/notify registers the link option as type %d, this endpoint reads %d", got, optionTypeString)
	}
	if got := notify.LinkOptionName(); got != linkOptionName {
		t.Fatalf("internal/notify registers the link option as %q, this endpoint reads %q", got, linkOptionName)
	}
	// …and the message command's (§20 erratum 315's addendum): its default
	// name in both packages and the loader, and the TYPE it is routed by.
	if got := notify.EnrichCommandName(); got != config.DefaultEnrichCommandName {
		t.Fatalf("internal/notify registers the message command as %q by default, internal/config's default is %q", got, config.DefaultEnrichCommandName)
	}
	if got := testBotConfig(t, pub, "").EnrichCommandName(); got != config.DefaultEnrichCommandName {
		t.Fatalf("a bot configured without %s answers %q", config.EnvDiscordEnrichCommandName, got)
	}
	if got := notify.MessageCommandType(); got != commandTypeMessage {
		t.Fatalf("internal/notify registers the message command as type %d, this endpoint routes type %d", got, commandTypeMessage)
	}
}

// TestInteractionsAnswerTheConfiguredCommandName is §20 erratum 284 on the
// INBOUND side: DISCORD_COMMAND_NAME decides which interaction this endpoint
// recognises, and the default stops being recognised the moment it is set.
//
// That second half is the point of the whole setting. Two instances — a dev box
// and a production box — may share a Discord server, so each must answer only
// its own command; an instance that still answered `/clip` after being renamed
// would upload into the wrong library, which is the one failure a rename exists
// to prevent.
func TestInteractionsAnswerTheConfiguredCommandName(t *testing.T) {
	// The UPLOAD command renamed (§20 erratum 315 gave it its own variable);
	// the link command's rename is proven in discord_link_test.go.
	const command = "upload-dev"
	b := newBotEnvWith(t, map[string]string{config.EnvDiscordUploadCommandName: command, config.EnvDiscordCommandName: "clip-dev"})
	const guild = "444444444444444444"
	b.boundGuild(t, "devguildowner", guild)

	// The renamed command reaches the binding and everything after it.
	att := clipBody(guild, "clip.mp4", "video/mp4", "https://cdn.discordapp.com/x/clip.mp4", 10)
	r := b.post(renameCommand(t, att, command))
	if r.code != http.StatusOK {
		t.Fatalf("a renamed /%s = %d, want 200: %s", command, r.code, r.body)
	}
	if got, _ := r.json["type"].(float64); int(got) != callbackDeferredMessage {
		t.Fatalf("a renamed /%s answered type %v, want %d (deferred)", command, r.json["type"], callbackDeferredMessage)
	}
	if b.bot.count() != 1 {
		t.Fatalf("%d job(s) handed over for a renamed command, want 1", b.bot.count())
	}
	// The DEFAULT names are not this instance's commands any more: each is
	// refused exactly as any other command is, with no job and no side effect.
	for _, name := range []string{config.DefaultUploadCommandName, config.DefaultCommandName} {
		r = b.post(renameCommand(t, att, name))
		if r.code != http.StatusOK {
			t.Fatalf("/%s on a renamed instance = %d, want 200: %s", name, r.code, r.body)
		}
		assertEphemeral(t, r, msgUnknown)
	}
	if b.bot.count() != 1 {
		t.Fatal("a default name on a renamed instance handed over a job")
	}
	// The refusals that NAME the command name the RENAMED one: an instance must
	// never point the channel at a command it does not answer.
	r = b.post(`{"type":2,"token":"t","guild_id":"` + guild + `","data":{"name":"` + command + `"}}`)
	assertEphemeral(t, r, msgNoInput(command))
	if strings.Contains(string(r.body), "/"+config.DefaultUploadCommandName+".") {
		t.Fatalf("a renamed instance sent the channel to /%s: %s", config.DefaultUploadCommandName, r.body)
	}
	// …and the unchanged instance is the mirror image of that.
	d := newBotEnv(t)
	d.boundGuild(t, "defaultguildowner", guild)
	if r := d.post(renameCommand(t, att, command)); r.code != http.StatusOK {
		t.Fatalf("/%s on a default instance = %d, want 200: %s", command, r.code, r.body)
	} else {
		assertEphemeral(t, r, msgUnknown)
	}
	if d.bot.count() != 0 {
		t.Fatalf("/%s handed over a job on an instance that publishes /%s", command, config.DefaultUploadCommandName)
	}
}

// --- a string option on the UPLOAD command (§20 errata 286, 315) -----------

// boundGuild binds a fresh account to a guild and hands both back.
func (b *botEnv) boundGuild(t *testing.T, name, guild string) db.User {
	t.Helper()
	u, _ := b.newUser(name)
	if _, err := b.db.BindGuild(context.Background(), guild, u.ID, b.now()); err != nil {
		t.Fatal(err)
	}
	return u
}

// TestInteractionsOldDefinitionsLinkOptionHandsOverNoJob: the UPLOAD command
// takes one option and it is `required`, so Discord refuses an empty one in the
// client — but a body is a body, and an `/upload` carrying a string `link` (the
// option §20 erratum 286 withdrew from the upload command and erratum 315 gave a
// command of its own) must be answered the way any upload with no usable
// attachment is: one line, no job, and NOT a different line per link. The link
// is `/clip`'s business; the upload command never reads it.
func TestInteractionsOldDefinitionsLinkOptionHandsOverNoJob(t *testing.T) {
	b := newBotEnv(t)
	const guild = "555555555555555555"
	u := b.boundGuild(t, "linkowner", guild)
	clip, _, _ := b.readyClip(u, clipOpts{title: "arc"})
	gone := b.deletingClip(u)

	for _, raw := range []string{
		testOrigin + "/" + clip.ID,                  // a clip that is here, ready, and ours
		testOrigin + "/clips/" + clip.ID,            // …by its legacy address
		testOrigin + "/embed/" + clip.ID,            // …by its player URL
		testOrigin + "/" + gone.ID,                  // a clip on its way out
		testOrigin + "/AbcDefGhiJkl",                // an id nobody minted
		"https://clips.elsewhere.test/AbcDefGhiJkl", // another host entirely
	} {
		t.Run(raw, func(t *testing.T) {
			r := b.post(legacyLinkBody(guild, raw))
			if r.code != http.StatusOK {
				t.Fatalf("a withdrawn link option = %d, want 200: %s", r.code, r.body)
			}
			assertEphemeral(t, r, msgNoInput(config.DefaultUploadCommandName))
		})
	}
	if b.bot.count() != 0 {
		t.Fatalf("%d job(s) were handed over for a withdrawn option", b.bot.count())
	}
}

// TestInteractionsOldDefinitionWithAFileStillUploads: the same old definition
// with the attachment filled in as well is an ordinary `/clip`. The attachment is
// found by TYPE and every other option is skipped in silence, which is what makes
// a stale command definition harmless rather than a refusal somebody has to
// diagnose.
func TestInteractionsOldDefinitionWithAFileStillUploads(t *testing.T) {
	b := newBotEnv(t)
	const guild = "666666666666666666"
	b.boundGuild(t, "linkowner2", guild)
	r := b.post(legacyBothBody(guild, testOrigin+"/AbcDefGhiJkl"))
	if got, _ := r.json["type"].(float64); int(got) != callbackDeferredMessage {
		t.Fatalf("an old definition carrying a file answered %s, want the deferred type", r.body)
	}
	job, ok := b.bot.last()
	if !ok || job.Ingest == nil {
		t.Fatal("no upload job was handed to the bot")
	}
	if job.URL != "https://cdn.discordapp.com/x/clip.mp4" || job.Filename != "clip.mp4" {
		t.Fatalf("the job does not carry the attachment: %+v", job)
	}
	if job.Command != "/upload" {
		t.Fatalf("the job names %q, want /upload", job.Command)
	}
}

// An old `/clip file:` (the definition a server holds until it re-registers)
// uploads as `/upload` does, and its job's log lines name /clip (run 8).
func TestInteractionsOldClipFileJobNamesClip(t *testing.T) {
	b := newBotEnv(t)
	const guild = "666666666666666667"
	b.boundGuild(t, "oldclipowner", guild)
	body := strings.Replace(clipBody(guild, "clip.mp4", "video/mp4", "https://cdn.discordapp.com/attachments/1/2/clip.mp4", 4096),
		`"name":"`+config.DefaultUploadCommandName+`"`, `"name":"`+config.DefaultCommandName+`"`, 1)
	r := b.post(body)
	if got, _ := r.json["type"].(float64); int(got) != callbackDeferredMessage {
		t.Fatalf("an old /clip file: answered %s, want the deferred type", r.body)
	}
	job, ok := b.bot.last()
	if !ok || job.Command != "/"+config.DefaultCommandName {
		t.Fatalf("the job names %q, want /%s", job.Command, config.DefaultCommandName)
	}
}

// TestInteractionsLogTheCommandThisInstanceAnswers: the access-log annotation
// carries the name this instance ANSWERS, not the literal `clip`. On a renamed
// instance the line read `discord=clip` for an interaction nobody could have
// sent under that name — and this is the one field an operator greps to see
// that a rename took (§20 erratum 284, amended).
func TestInteractionsLogTheCommandThisInstanceAnswers(t *testing.T) {
	const command = "upload-dev"
	const guild = "444444444444444444"
	b := newBotEnvWith(t, map[string]string{config.EnvDiscordUploadCommandName: command})
	b.boundGuild(t, "logownerdev", guild)
	att := clipBody(guild, "clip.mp4", "video/mp4", "https://cdn.discordapp.com/x/clip.mp4", 10)

	_, logs := b.postLogged(renameCommand(t, att, command))
	if !strings.Contains(logs, "discord="+command) {
		t.Fatalf("the access log does not name the command this instance answers:\n%s", logs)
	}
	if strings.Contains(logs, "discord="+config.DefaultUploadCommandName+" ") {
		t.Fatalf("the access log says discord=%s on a renamed instance:\n%s", config.DefaultUploadCommandName, logs)
	}
	// An unknown command keeps its own word: the field separates the two.
	if _, logs := b.postLogged(att); !strings.Contains(logs, "discord=unknown_command") {
		t.Fatalf("an unknown command is not logged as one:\n%s", logs)
	}

	// The default instance logs the default name: `discord=upload`.
	d := newBotEnv(t)
	d.boundGuild(t, "logownerdefault", guild)
	if _, logs := d.postLogged(att); !strings.Contains(logs, "discord="+config.DefaultUploadCommandName) {
		t.Fatalf("the shipped instance does not log discord=%s:\n%s", config.DefaultUploadCommandName, logs)
	}
}
