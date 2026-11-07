package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/notify"
)

// Enrich on a message that carries a VIDEO (§20 erratum 323): with no link of
// ours in the text, the first video is uploaded through `/upload`'s own path —
// for the message's poster or a member holding Administrator or Manage Server
// only. Fixtures are Discord's documented shape (developers/interactions/
// application-commands, Message Commands, "Example Interaction": data.target_id,
// data.resolved.messages{id: {attachments, author{id}, content}}; the invoking
// member's `user.id` and `permissions`).

const (
	evGuild   = "999999999999999981"
	evPoster  = "100000000000000001"
	evOther   = "100000000000000002"
	evCDN     = "https://cdn.discordapp.com/attachments/1/2/clip.mp4"
	evTarget  = "1500000000000000001"
	evMaxSize = 1 << 20 // the harness's MAX_UPLOAD_BYTES
)

type evAtt struct {
	filename, contentType, url string
	size                       int64
}

// evOn turns DISCORD_ENRICH_UPLOAD on (it is off by default): every test of the
// upload itself runs with it.
func evOn(d *Deps) { d.Cfg.EnrichUpload = true }

func evVideo() evAtt { return evAtt{"clip.mp4", "video/mp4", evCDN, 4096} }

// enrichMessageBody is Enrich run by member (`memberID`, `perms`) on one
// message posted by `authorID`, with `content` and `atts`. "" for memberID
// omits the member's user; "" for authorID omits the author.
func enrichMessageBody(memberID, perms, authorID, content string, atts ...evAtt) string {
	list := []any{}
	for i, a := range atts {
		list = append(list, map[string]any{
			"id": "77700000000000000" + string(rune('0'+i)), "filename": a.filename, "url": a.url,
			"proxy_url": a.url, "size": a.size, "content_type": a.contentType,
		})
	}
	msg := map[string]any{"id": evTarget, "content": content, "embeds": []any{}, "attachments": list}
	if authorID != "" {
		msg["author"] = map[string]any{"id": authorID, "username": "poster-name-never-read"}
	}
	member := map[string]any{"permissions": perms}
	if memberID != "" {
		member["user"] = map[string]any{"id": memberID, "username": "member-name-never-read"}
	}
	body, _ := json.Marshal(map[string]any{
		"type": interactionCommand, "token": "int-token-abc", "guild_id": evGuild, "member": member,
		"data": map[string]any{
			"name": config.DefaultEnrichCommandName, "type": commandTypeMessage, "target_id": evTarget,
			"resolved": map[string]any{"messages": map[string]any{evTarget: msg}},
		},
	})
	return string(body)
}

func assertDeferred(t *testing.T, r resp) {
	t.Helper()
	if r.code != http.StatusOK {
		t.Fatalf("= %d: %s", r.code, r.body)
	}
	if strings.TrimSpace(string(r.body)) != `{"type":5}` {
		t.Fatalf("answered %s, want the deferral {\"type\":5}", r.body)
	}
}

// The poster's Enrich on their own video: `/upload`'s deferral and its job —
// the bound account's ingest, the attachment, the token — and NOT the form,
// even with DISCORD_UPLOAD_FORM on.
func TestEnrichVideoByThePosterUploads(t *testing.T) {
	b := newFormEnv(t, evOn) // UploadForm ON: Enrich must still not open it
	u := b.boundGuild(t, "evposter", evGuild)
	r := b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "gg", evVideo()))
	assertDeferred(t, r)
	job, ok := b.bot.last()
	if !ok || b.bot.count() != 1 {
		t.Fatalf("%d job(s) handed over, want 1", b.bot.count())
	}
	if job.URL != evCDN || job.Filename != "clip.mp4" || job.Size != 4096 || job.ContentType != "video/mp4" ||
		job.GuildID != evGuild || job.Token != "int-token-abc" || job.Ingest == nil {
		t.Fatalf("the job does not carry the attachment: %+v", job)
	}
	if job.Followup || job.Failed != nil {
		t.Fatalf("the job is a form's (Followup %v): Enrich opens no form", job.Followup)
	}
	if job.Command != "Enrich" {
		t.Fatalf("an Enrich job's log lines would name %q", job.Command)
	}
	// The ingest is the BOUND account's, and the title is the file name.
	id := runIngest(t, job)
	c := b.getClip(id)
	if c.OwnerID != u.ID || c.Title != "clip" {
		t.Fatalf("the clip is owner %q title %q, want the bound account and the file name", c.OwnerID, c.Title)
	}
}

// A manager — Administrator or Manage Server — may Enrich somebody else's video.
func TestEnrichVideoByAManagerUploads(t *testing.T) {
	b := newBotEnv(t, evOn)
	b.boundGuild(t, "evmanager", evGuild)
	for i, perms := range []string{permManage, permAdmin, permBothAndMore} {
		assertDeferred(t, b.post(enrichMessageBody(evOther, perms, evPoster, "", evVideo())))
		if b.bot.count() != i+1 {
			t.Fatalf("perms %s: %d job(s), want %d", perms, b.bot.count(), i+1)
		}
	}
	// …and with no user on the member at all: the permission alone decides.
	assertDeferred(t, b.post(enrichMessageBody("", permManage, evPoster, "", evVideo())))
}

// Anyone else: one private line, and no job — so nothing is fetched.
func TestEnrichVideoByAStrangerIsRefused(t *testing.T) {
	b := newBotEnv(t, evOn)
	b.boundGuild(t, "evstranger", evGuild)
	for _, tc := range []struct{ name, body string }{
		{"another member", enrichMessageBody(evOther, permOrdinary, evPoster, "", evVideo())},
		{"no user on the member", enrichMessageBody("", permOrdinary, evPoster, "", evVideo())},
		{"no author on the message", enrichMessageBody(evPoster, permOrdinary, "", "", evVideo())},
		{"neither id", enrichMessageBody("", permOrdinary, "", "", evVideo())},
		// Two EMPTY ids are equal strings, and must not read as "the poster".
		{"two empty ids", strings.ReplaceAll(enrichMessageBody(evPoster, permOrdinary, evPoster, "", evVideo()), `"id":"`+evPoster+`"`, `"id":""`)},
		{"no permissions at all", enrichMessageBody(evOther, "", evPoster, "", evVideo())},
		// The refusal comes before the size check: a stranger learns nothing about the file.
		{"a stranger on a file too large", enrichMessageBody(evOther, permOrdinary, evPoster, "", evAtt{"big.mp4", "video/mp4", evCDN, 1 << 30})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertEphemeral(t, b.post(tc.body), msgEnrichNotYours)
		})
	}
	// No member object at all (not Discord's shape in a server; a body is a body).
	body := strings.Replace(enrichMessageBody(evPoster, permOrdinary, evPoster, "", evVideo()),
		`"member":{"permissions":"`+permOrdinary+`","user":{"id":"`+evPoster+`","username":"member-name-never-read"}},`, "", 1)
	if strings.Contains(body, `"member"`) {
		t.Fatalf("the member was not removed: %s", body)
	}
	assertEphemeral(t, b.post(body), msgEnrichNotYours)
	if b.bot.count() != 0 {
		t.Fatalf("%d job(s) handed over for a stranger's Enrich", b.bot.count())
	}
}

// A link of ours in the text still wins: the link's clip is posted at once and
// the video is not uploaded.
func TestEnrichLinkWinsOverAVideo(t *testing.T) {
	b := newBotEnv(t, evOn)
	u := b.boundGuild(t, "evlink", evGuild)
	clip, _, _ := b.readyClip(u, clipOpts{title: "arc"})
	assertPosted(t, b, b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "see "+testOrigin+"/"+clip.ID, evVideo())), clip)
	// A tombstone link answers "gone" — still no upload.
	gone := b.deletingClip(u)
	assertEphemeral(t, b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, testOrigin+"/"+gone.ID, evVideo())), msgLinkGone)
	// A link of another host, or an id of ours nobody minted, names no clip here: the video is taken.
	assertDeferred(t, b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "https://elsewhere.test/AbcDefGhiJkl "+testOrigin+"/AbcDefGhiJkl", evVideo())))
	if b.bot.count() != 1 {
		t.Fatalf("%d job(s), want 1 (only the message whose links name nothing here)", b.bot.count())
	}
}

// The FIRST video only; images and other files are skipped; no video at all is
// today's no-link line.
func TestEnrichTakesTheFirstVideo(t *testing.T) {
	b := newBotEnv(t, evOn)
	b.boundGuild(t, "evfirst", evGuild)
	img := evAtt{"shot.png", "image/png", "https://cdn.discordapp.com/attachments/1/2/shot.png", 100}
	second := evAtt{"second.webm", "video/webm", "https://cdn.discordapp.com/attachments/1/2/second.webm", 200}
	// By extension when Discord sends no content type.
	byExt := evAtt{"noct.mov", "", "https://cdn.discordapp.com/attachments/1/2/noct.mov", 300}
	noURL := evAtt{"nourl.mp4", "video/mp4", "", 1}
	assertDeferred(t, b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "", img, noURL, evVideo(), second)))
	if job, _ := b.bot.last(); job.URL != evCDN {
		t.Fatalf("took %q, want the first video %q", job.URL, evCDN)
	}
	assertDeferred(t, b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "", byExt)))
	if job, _ := b.bot.last(); job.Filename != "noct.mov" {
		t.Fatalf("took %q, want the extension-typed video", job.Filename)
	}
	for _, tc := range []struct {
		name string
		atts []evAtt
	}{
		{"an image alone", []evAtt{img}},
		{"a text file", []evAtt{{"notes.txt", "text/plain", "https://cdn.discordapp.com/x/notes.txt", 10}}},
		{"nothing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertEphemeral(t, b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "gg", tc.atts...)), msgEnrichNoLink)
		})
	}
	// A stranger on a message with no video is told the no-link line, not the refusal.
	assertEphemeral(t, b.post(enrichMessageBody(evOther, permOrdinary, evPoster, "", img)), msgEnrichNoLink)
	if b.bot.count() != 2 {
		t.Fatalf("%d job(s), want 2", b.bot.count())
	}
}

// `/upload`'s refusals apply as they stand: too large, and the generic line
// without a bot; an unbound server is refused before the message is read.
func TestEnrichVideoMeetsUploadsRefusals(t *testing.T) {
	b := newBotEnv(t, evOn)
	b.boundGuild(t, "evrefuse", evGuild)
	r := b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "", evAtt{"big.mp4", "video/mp4", evCDN, evMaxSize + 1}))
	assertEphemeral(t, r, notify.SentenceTooLarge+" (limit "+humanBytes(evMaxSize)+")")
	unbound := strings.Replace(enrichMessageBody(evPoster, permOrdinary, evPoster, "", evVideo()), evGuild, "111111111111111111", 1)
	assertEphemeral(t, b.post(unbound), msgUnbound)
	if b.bot.count() != 0 {
		t.Fatalf("%d job(s) for refused Enrich", b.bot.count())
	}
	nb := newBotEnv(t, evOn, func(d *Deps) { d.Bot = nil })
	nb.boundGuild(t, "evnobot", evGuild)
	assertEphemeral(t, nb.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "", evVideo())), msgUnavailable)
}

// A MANAGED instance (the official bot's forwarder relays Discord's bytes)
// answers a video Enrich exactly as one with its own application does.
func TestManagedInstanceEnrichUploadsAVideo(t *testing.T) {
	b := newBotEnvWith(t, map[string]string{config.EnvDiscordBotToken: config.ManagedToken}, evOn)
	b.boundGuild(t, "evmanaged", evGuild)
	assertDeferred(t, b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "", evVideo())))
	if job, ok := b.bot.last(); !ok || job.URL != evCDN || job.Token != "int-token-abc" {
		t.Fatal("no upload job on the managed instance")
	}
}

// The access log carries the outcome word and nothing of the message or the
// people: no author or member id, no URL, no text, no name.
func TestEnrichVideoLogsTheOutcomeAndNoIdentity(t *testing.T) {
	b := newBotEnv(t, evOn)
	b.boundGuild(t, "evlog", evGuild)
	_, logs := b.postLogged(enrichMessageBody(evPoster, permOrdinary, evPoster, "secret words", evVideo()))
	if !strings.Contains(logs, "discord=enrich") || !strings.Contains(logs, "enrich=upload") {
		t.Fatalf("the access log lacks the command or the outcome:\n%s", logs)
	}
	_, refused := b.postLogged(enrichMessageBody(evOther, permOrdinary, evPoster, "secret words", evVideo()))
	if !strings.Contains(refused, "reject=not_poster") {
		t.Fatalf("the refusal is not on the access log:\n%s", refused)
	}
	for _, leak := range []string{evPoster, evOther, "cdn.discordapp.com", "secret words", "never-read", "clip.mp4"} {
		if strings.Contains(logs+refused, leak) {
			t.Fatalf("%q reached the access log:\n%s\n%s", leak, logs, refused)
		}
	}
	// A refused upload logs its refusal and NOT the outcome word: `enrich=upload`
	// means a job was handed over.
	_, big := b.postLogged(enrichMessageBody(evPoster, permOrdinary, evPoster, "", evAtt{"big.mp4", "video/mp4", evCDN, evMaxSize + 1}))
	if !strings.Contains(big, "reject=too_large") || strings.Contains(big, "enrich=upload") {
		t.Fatalf("a refused Enrich upload logged:\n%s", big)
	}
}

// DISCORD_ENRICH_UPLOAD off — the DEFAULT (§20 erratum 323): Enrich answers
// links only, exactly as before. The poster's Enrich on a video is the no-link
// line, with no job (so no fetch), and no `enrich=` or `not_poster` field —
// for the poster and a stranger alike. The switch is taken from config.Load
// with the variable unset — the shipped default — so flipping that default in
// internal/config fails this test.
func TestEnrichUploadOffByDefaultAnswersLinksOnly(t *testing.T) {
	env := map[string]string{
		"PUBLIC_BASE_URL": testOrigin, "S3_ENDPOINT": "http://minio:9000", "S3_BUCKET": "b",
		"S3_ACCESS_KEY": "a", "S3_SECRET_KEY": "s",
	}
	loaded, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	b := newBotEnv(t, func(d *Deps) { d.Cfg.EnrichUpload = loaded.EnrichUpload })
	if b.cfg.EnrichUpload {
		t.Fatal("the test env has DISCORD_ENRICH_UPLOAD on")
	}
	u := b.boundGuild(t, "evoff", evGuild)
	for _, body := range []string{
		enrichMessageBody(evPoster, permOrdinary, evPoster, "gg", evVideo()),
		enrichMessageBody(evOther, permOrdinary, evPoster, "gg", evVideo()),
		enrichMessageBody(evOther, permManage, evPoster, "gg", evVideo()),
	} {
		r, logs := b.postLogged(body)
		assertEphemeral(t, r, msgEnrichNoLink)
		if !strings.Contains(logs, "reject=no_link") || strings.Contains(logs, "enrich=") || strings.Contains(logs, "not_poster") {
			t.Fatalf("with the switch off the access log says:\n%s", logs)
		}
	}
	if b.bot.count() != 0 {
		t.Fatalf("%d job(s) with DISCORD_ENRICH_UPLOAD off", b.bot.count())
	}
	// A link of ours is still answered, as before.
	clip, _, _ := b.readyClip(u, clipOpts{title: "arc"})
	assertPosted(t, b, b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, testOrigin+"/"+clip.ID, evVideo())), clip)
}

// --- through a REAL notify.Bot -----------------------------------------------

// evDiscord is an httptest stand-in for Discord's API and CDN at once; it counts
// what the CDN half served.
type evDiscord struct {
	srv     *httptest.Server
	mu      sync.Mutex
	cdnHits int
	calls   []string
	// called is closed and replaced on every recorded call (guarded by mu).
	called chan struct{}
}

func newEvDiscord(t *testing.T) *evDiscord {
	f := &evDiscord{called: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/attachments/") {
			f.mu.Lock()
			f.cdnHits++
			f.mu.Unlock()
			_, _ = io.WriteString(w, "fake video bytes")
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+string(body))
		close(f.called)
		f.called = make(chan struct{})
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"1"}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *evDiscord) snapshot() (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cdnHits, strings.Join(f.calls, "\n")
}

// waitEdit waits for the deferred reply's edit (a PATCH of @original).
func (f *evDiscord) waitEdit(t *testing.T) string {
	t.Helper()
	guard := time.NewTimer(time.Minute) // a hang guard, never a budget
	defer guard.Stop()
	for {
		f.mu.Lock()
		next := f.called
		f.mu.Unlock()
		_, calls := f.snapshot()
		if strings.Contains(calls, "PATCH ") {
			return calls
		}
		select {
		case <-next:
		case <-guard.C:
			t.Fatalf("the deferred reply was never edited: %s", calls)
		}
	}
}

// newEnrichEnvWithRealBot: the bot's allowlist is `allow`; nil is serve's own
// config.ValidAttachmentURL, the production gate.
func newEnrichEnvWithRealBot(t *testing.T, f *evDiscord, allow func(string) (*url.URL, error)) *botEnv {
	host := strings.TrimPrefix(f.srv.URL, "http://")
	if allow == nil {
		allow = config.ValidAttachmentURL
	}
	var bot *notify.Bot
	b := newFormEnv(t, evOn, func(d *Deps) { // the form ON: Enrich still defers and edits
		bot = notify.NewBot(notify.BotDeps{
			AppID: "123456789012345678", Token: "MTIzNDU2Nzg5MDEyMzQ1Njc4.GhIjKl.mNoPqRsTuVwXyZ0123456789abcdef",
			URLs: d.URLs, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			APIHost: host, APIScheme: "http", AllowAttachment: allow,
			MaxBytes: 1 << 20, CallTimeout: 2 * time.Second, FetchTimeout: 2 * time.Second, JobBudget: 5 * time.Second,
		})
		d.Bot = bot
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = bot.Shutdown(ctx)
	})
	return b
}

// The whole path: the poster's Enrich → the deferral → the CDN fetched once →
// a clip in the BOUND account titled by the file name → the deferred reply
// edited into its bare link (§20 erratum 316).
func TestEnrichVideoWholePath(t *testing.T) {
	f := newEvDiscord(t)
	host := strings.TrimPrefix(f.srv.URL, "http://")
	// The CDN is the test server; the allowlist rewrites the real host to it, as a test may.
	b := newEnrichEnvWithRealBot(t, f, func(raw string) (*url.URL, error) {
		u, err := config.ValidAttachmentURL(raw) // the production gate first
		if err != nil {
			return nil, err
		}
		u.Scheme, u.Host = "http", host
		return u, nil
	})
	owner := b.boundGuild(t, "evwhole", evGuild)
	// A stranger first, on the SAME attachment this env can fetch (run 8): the
	// refusal is all that happens — the one CDN hit below is the poster's.
	assertEphemeral(t, b.post(enrichMessageBody(evOther, permOrdinary, evPoster, "gg", evVideo())), msgEnrichNotYours)
	assertDeferred(t, b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "gg", evVideo())))
	calls := f.waitEdit(t)
	if !strings.Contains(calls, "PATCH /api/v10/webhooks/123456789012345678/int-token-abc/messages/@original ") {
		t.Fatalf("the edit is not the deferred reply's: %s", calls)
	}
	if strings.Contains(calls, "POST ") {
		t.Fatalf("Enrich posted a follow-up (it opens no form): %s", calls)
	}
	m := regexp.MustCompile(`"content":"` + regexp.QuoteMeta(testOrigin) + `/([A-Za-z0-9]+)"`).FindStringSubmatch(calls)
	if m == nil {
		t.Fatalf("no link was posted: %s", calls)
	}
	c := b.getClip(m[1])
	if c.OwnerID != owner.ID || c.Title != "clip" {
		t.Fatalf("the clip is owner %q title %q, want the bound account and the file name", c.OwnerID, c.Title)
	}
	if hits, _ := f.snapshot(); hits != 1 {
		t.Fatalf("the CDN served %d request(s), want 1 (a second is the stranger's)", hits)
	}
}

// A non-CDN attachment URL is refused exactly as `/upload` refuses one: the
// deferral, then the fetch sentence — by serve's own allowlist, before a socket
// is opened (the stub, which IS where the URL points, sees nothing). And a
// stranger's Enrich reaches nobody at all.
func TestEnrichVideoOffTheCDNAndAStrangerFetchNothing(t *testing.T) {
	f := newEvDiscord(t)
	b := newEnrichEnvWithRealBot(t, f, nil)
	owner := b.boundGuild(t, "evcdn", evGuild)
	stub := f.srv.URL + "/attachments/1/2/clip.mp4"
	for _, raw := range []string{stub, strings.Replace(evCDN, "cdn.discordapp.com", "cdn.discordapp.com.evil.test", 1)} {
		assertEphemeral(t, b.post(enrichMessageBody(evOther, permOrdinary, evPoster, "", evAtt{"clip.mp4", "video/mp4", raw, 4096})), msgEnrichNotYours)
	}
	time.Sleep(200 * time.Millisecond) // sleep: a negative check (nothing reaches the stub); load can only lengthen it
	if hits, calls := f.snapshot(); hits != 0 || calls != "" {
		t.Fatalf("a stranger's Enrich reached the stub: %d CDN hit(s), calls %q", hits, calls)
	}
	assertDeferred(t, b.post(enrichMessageBody(evPoster, permOrdinary, evPoster, "", evAtt{"clip.mp4", "video/mp4", stub, 4096})))
	calls := f.waitEdit(t)
	if !strings.Contains(calls, notify.SentenceFetch) {
		t.Fatalf("the edit is not the fetch sentence: %s", calls)
	}
	if hits, _ := f.snapshot(); hits != 0 {
		t.Fatalf("the off-CDN attachment was fetched (%d hit(s))", hits)
	}
	clips, _, err := b.db.ListClips(context.Background(), db.ListQuery{ViewerID: owner.ID, Mine: true, Limit: 10})
	if err != nil || len(clips) != 0 {
		t.Fatalf("a clip exists for a refused attachment: %v %v", clips, err)
	}
}
