package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/urls"
)

// The Discord bot's outbound half (ROADMAP #33, §20 errata 253-256, 268). Every
// test here drives httptest servers standing in for Discord's API and its
// attachment CDN; nothing reaches the network, exactly as the notifier's own
// tests do not.

const (
	testBotToken   = "MTIzNDU2Nzg5MDEyMzQ1Njc4.GhIjKl.mNoPqRsTuVwXyZ0123456789abcdef"
	testIntToken   = "aW50ZXJhY3Rpb24tdG9rZW4tdGhhdC1pcy1zZWNyZXQ"
	testAppID      = "123456789012345678"
	testGuildID    = "222222222222222222"
	testAttachment = "attachments/1/2/clip.mp4"
)

// syncBuffer is a log sink a test may read while a goroutine writes to it. The
// bot logs from the goroutine that runs a `/clip`, and a bare bytes.Buffer
// read from the test at the same moment is a genuine data race — one the
// -race pass finds (§20 erratum 41), and the kind of thing that is a test bug
// rather than a product one only because the buffer is the test's.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// apiCall is one request the fake Discord API received.
type apiCall struct {
	method, path, auth string
	body               []byte
}

// botFixture is a Bot wired to two httptest servers plus everything a test
// needs to assert on: the calls the API saw, the bytes the ingest received and
// the log the bot wrote.
type botFixture struct {
	bot     *Bot
	api     *httptest.Server
	cdn     *httptest.Server
	logs    *syncBuffer
	mu      sync.Mutex
	calls   []apiCall
	fetched int
	// edited is closed when the deferred reply has been edited, so a test can
	// wait for the goroutine without sleeping.
	edited chan struct{}
	once   sync.Once
	// followed is edited's twin for a follow-up POST (§20 erratum 320).
	followed chan struct{}
	onceF    sync.Once
	// handler, when set, replaces the fake API's own answers (guarded by mu).
	handler http.HandlerFunc
}

// answerWith installs a fake-API handler for the rest of the test.
func (f *botFixture) answerWith(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
}

// newBotFixture builds the pair. `cdnBody` is what the attachment host serves;
// `cdnStatus` its status (200 unless a test wants a refusal).
func newBotFixture(t *testing.T, cdnStatus int, cdnBody string) *botFixture {
	t.Helper()
	f := &botFixture{logs: &syncBuffer{}, edited: make(chan struct{}), followed: make(chan struct{})}
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The override is read under the fixture's own mutex rather than by
		// swapping http.Server.Config.Handler: the accept loop is already
		// running, so that swap is a data race the -race pass would find
		// (§20 erratum 41's pass is part of the CI story).
		f.mu.Lock()
		override := f.handler
		f.mu.Unlock()
		if override != nil {
			override(w, r)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{method: r.Method, path: r.URL.Path,
			auth: r.Header.Get("Authorization"), body: body})
		f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/messages/@original") {
			f.once.Do(func() { close(f.edited) })
		}
		if isFollowup(r.Method, r.URL.Path) {
			f.onceF.Do(func() { close(f.followed) })
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	t.Cleanup(f.api.Close)
	f.cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.fetched++
		f.mu.Unlock()
		w.WriteHeader(cdnStatus)
		_, _ = io.WriteString(w, cdnBody)
	}))
	t.Cleanup(f.cdn.Close)
	base, _ := url.Parse("https://clips.example")
	f.bot = NewBot(BotDeps{
		AppID: testAppID, Token: testBotToken,
		URLs:      urls.New(base, "test"),
		Log:       slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		APIHost:   strings.TrimPrefix(f.api.URL, "http://"),
		APIScheme: "http",
		// The ALLOWLIST lives in internal/config and is tested there; here it
		// is a function, so a unit test can point at its own server without
		// the allowlist deciding what a test may do (Deps.Endpoint's reason).
		AllowAttachment: func(raw string) (*url.URL, error) { return url.Parse(raw) },
		MaxBytes:        1 << 20,
		CallTimeout:     2 * time.Second,
		FetchTimeout:    2 * time.Second,
		JobBudget:       5 * time.Second,
	})
	if f.bot == nil {
		t.Fatal("NewBot returned nil for a configured application")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.bot.Shutdown(ctx)
	})
	return f
}

func (f *botFixture) attachmentURL() string { return f.cdn.URL + "/" + testAttachment }

// quiesce waits for every goroutine the Bot has counted — its jobs, their
// retries, its redraws — to end: the WaitGroup Shutdown waits on, without
// closing the Bot. A log line those goroutines write is in the buffer when it
// returns, so the test reads the log once instead of polling it. Call it only
// once the job under test has been started (no Add from zero beside the Wait).
func (f *botFixture) quiesce(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { f.bot.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Minute): // a hang guard, never a budget
		t.Fatalf("the bot's goroutines never ended; log:\n%s", f.logs.String())
	}
}

func (f *botFixture) waitEdited(t *testing.T) apiCall {
	t.Helper()
	select {
	case <-f.edited:
	case <-time.After(5 * time.Second):
		t.Fatalf("the deferred reply was never edited; log:\n%s", f.logs.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasSuffix(c.path, "/messages/@original") {
			return c
		}
	}
	t.Fatal("no @original edit recorded")
	return apiCall{}
}

func (f *botFixture) fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetched
}

// content reads the `content` field of an edit body.
func content(t *testing.T, c apiCall) string {
	t.Helper()
	var p payload
	if err := json.Unmarshal(c.body, &p); err != nil {
		t.Fatalf("edit body is not the payload: %s", c.body)
	}
	if p.AllowedMentions.Parse == nil || len(p.AllowedMentions.Parse) != 0 {
		t.Fatalf("the edit does not carry an empty allowed_mentions parse list: %s", c.body)
	}
	return p.Content
}

func okJob(f *botFixture, ingest Ingest) ClipJob {
	return ClipJob{Token: testIntToken, URL: f.attachmentURL(), Filename: "clip.mp4",
		ContentType: "video/mp4", Size: 11, GuildID: testGuildID, Ingest: ingest}
}

func TestBotClipFetchesIngestsAndPostsTheLink(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	var got IngestSource
	var body []byte
	f.bot.Clip(okJob(f, func(ctx context.Context, src IngestSource) (IngestResult, error) {
		got = src
		body, _ = io.ReadAll(src.Body)
		return IngestResult{ClipID: "abc123XYZ789", CardReady: true}, nil
	}))
	edit := f.waitEdited(t)
	if edit.method != http.MethodPatch {
		t.Fatalf("the follow-up used %s, want PATCH", edit.method)
	}
	if want := "/api/v10/webhooks/" + testAppID + "/" + testIntToken + "/messages/@original"; edit.path != want {
		t.Fatalf("the follow-up went to %s, want %s", edit.path, want)
	}
	// The interaction token IS the credential for its own follow-up: the bot
	// token must not be sent here (§20 erratum 254).
	if edit.auth != "" {
		t.Fatalf("the follow-up carried an Authorization header: %q", edit.auth)
	}
	if c := content(t, edit); c != "https://clips.example/abc123XYZ789" {
		t.Fatalf("the reply's content is %q, want the share link and nothing else", c)
	}
	// …and nothing else (§20 erratum 316): the same message `/clip link:`
	// answers with, so the follow-up body IS ClipMessage's.
	want, _ := json.Marshal(ClipMessage(f.bot.urls, "abc123XYZ789"))
	if !bytes.Equal(edit.body, want) {
		t.Fatalf("the follow-up body is\n%s\nwant ClipMessage's\n%s", edit.body, want)
	}
	if string(body) != "video bytes" {
		t.Fatalf("the ingest received %q", body)
	}
	if got.Filename != "clip.mp4" || got.ContentType != "video/mp4" || got.Size != 11 {
		t.Fatalf("the ingest was handed %+v", got)
	}
	if f.fetches() != 1 {
		t.Fatalf("the CDN was called %d times, want once", f.fetches())
	}
}

func TestBotPostsTheLinkEvenWhenTheCardIsNotReadyYet(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	f.bot.Clip(okJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		return IngestResult{ClipID: "notreadyyet1", CardReady: false}, nil
	}))
	// Erratum 212/213's reasoning: the crawler hold makes Discord's own unfurl
	// retry, so a link that is not card-ready yet is still the right answer —
	// and the alternative (an apology) would be a link nobody ever gets.
	if c := content(t, f.waitEdited(t)); c != "https://clips.example/notreadyyet1" {
		t.Fatalf("the reply is %q, want the link anyway", c)
	}
	if !strings.Contains(f.logs.String(), "card_ready=false") {
		t.Fatalf("the log does not record that the card was not ready: %s", f.logs.String())
	}
}

func TestBotRefusesAnAttachmentOffTheAllowlist(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	f.bot.allow = func(string) (*url.URL, error) { return nil, errors.New("host not allowed") }
	f.bot.Clip(okJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		t.Error("the ingest ran for an attachment off the allowlist")
		return IngestResult{}, nil
	}))
	if c := content(t, f.waitEdited(t)); c != SentenceFetch {
		t.Fatalf("the reply is %q, want %q", c, SentenceFetch)
	}
	if f.fetches() != 0 {
		t.Fatal("a refused host was fetched anyway")
	}
}

func TestBotRefusesAnOversizedAttachmentWithoutFetchingIt(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	job := okJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		t.Error("the ingest ran for an oversized attachment")
		return IngestResult{}, nil
	})
	job.Size = 1<<20 + 1 // one byte over MaxBytes
	f.bot.Clip(job)
	if c := content(t, f.waitEdited(t)); c != SentenceTooLarge {
		t.Fatalf("the reply is %q, want %q", c, SentenceTooLarge)
	}
	if f.fetches() != 0 {
		t.Fatal("an oversized attachment was fetched anyway")
	}
}

func TestBotReportsACDNRefusal(t *testing.T) {
	f := newBotFixture(t, http.StatusForbidden, `{"message":"expired"}`)
	f.bot.Clip(okJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		t.Error("the ingest ran for a fetch that failed")
		return IngestResult{}, nil
	}))
	if c := content(t, f.waitEdited(t)); c != SentenceFetch {
		t.Fatalf("the reply is %q, want %q", c, SentenceFetch)
	}
}

func TestBotShowsTheIngestsOwnSentence(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	f.bot.Clip(okJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		return IngestResult{}, &IngestError{Sentence: SentenceBusy, Err: errors.New("rate limited: user u_123")}
	}))
	if c := content(t, f.waitEdited(t)); c != SentenceBusy {
		t.Fatalf("the reply is %q, want the ingest's own sentence %q", c, SentenceBusy)
	}
	// The CAUSE is the operator's and stays in the log; the reply is the
	// sentence alone.
	if !strings.Contains(f.logs.String(), "rate limited") {
		t.Fatalf("the cause never reached the log: %s", f.logs.String())
	}
}

func TestBotShowsOneGenericSentenceForAnUnclassifiedFailure(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	f.bot.Clip(okJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		return IngestResult{}, errors.New("sql: no such column /data/clippet.db")
	}))
	if c := content(t, f.waitEdited(t)); c != SentenceFailed {
		t.Fatalf("the reply is %q, want the generic sentence", c)
	}
	if strings.Contains(content(t, f.waitEdited(t)), "clippet.db") {
		t.Fatal("an internal path reached the Discord message")
	}
}

// TestBotScrubsBothTokens is the secret-hygiene gate (§20 errata 63, 84, 254):
// neither the bot token nor the interaction token may appear in the log,
// however hard the far end tries to echo them back.
func TestBotScrubsBothTokens(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	// A hostile (or merely chatty) Discord that quotes the whole request back.
	f.answerWith(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/@original") {
			f.once.Do(func() { close(f.edited) })
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"bad request for `+r.URL.Path+` with token `+testBotToken+`"}`)
	})
	f.bot.Clip(okJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		return IngestResult{ClipID: "abc123XYZ789"}, nil
	}))
	select {
	case <-f.edited:
	case <-time.After(5 * time.Second):
		t.Fatal("no edit was attempted")
	}
	// The retry and its log line are asynchronous: wait for the job's goroutine
	// to end, then read the whole log once.
	f.quiesce(t)
	logged := f.logs.String()
	if !strings.Contains(logged, "could not be edited") {
		t.Fatalf("no failure line:\n%s", logged)
	}
	if strings.Contains(logged, testBotToken) {
		t.Fatalf("the BOT token reached the log:\n%s", logged)
	}
	if strings.Contains(logged, testIntToken) {
		t.Fatalf("the INTERACTION token reached the log:\n%s", logged)
	}
	if !strings.Contains(logged, "…") {
		t.Fatalf("nothing was redacted, so the scrub never ran:\n%s", logged)
	}
}

func TestRegisterPublishesTheGuildCommandWithTheBotToken(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "")
	if err := f.bot.Register(context.Background(), testGuildID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 {
		t.Fatalf("Register made %d calls, want 1", len(f.calls))
	}
	c := f.calls[0]
	if c.method != http.MethodPut {
		t.Fatalf("Register used %s, want PUT (which replaces the guild's command set)", c.method)
	}
	if want := "/api/v10/applications/" + testAppID + "/guilds/" + testGuildID + "/commands"; c.path != want {
		t.Fatalf("Register went to %s, want %s", c.path, want)
	}
	if c.auth != "Bot "+testBotToken {
		t.Fatalf("Register did not authenticate as the bot: %q", c.auth)
	}
	var cmds []botCommandDef
	if err := json.Unmarshal(c.body, &cmds); err != nil || len(cmds) != 5 {
		t.Fatalf("Register sent %s (%v), want the five commands", c.body, err)
	}
	// THREE commands in ONE PUT since §20 erratum 315: two slash commands, each
	// with ONE option and it REQUIRED — `/upload file:` (Discord refuses an
	// empty one in its own client) and `/clip link:` — and the message command.
	up, cl, en := cmds[0], cmds[1], cmds[2]
	if up.Name != botUploadCommand || len(up.Options) != 1 {
		t.Fatalf("Register sent %+v first, want /upload with one option", up)
	}
	if o := up.Options[0]; o.Type != optionTypeAttachment || o.Name != botFileOption || !o.Required {
		t.Fatalf("the upload option is %+v, want a REQUIRED ATTACHMENT named %q", o, botFileOption)
	}
	if cl.Name != botCommand || len(cl.Options) != 1 {
		t.Fatalf("Register sent %+v second, want /clip with one option", cl)
	}
	if o := cl.Options[0]; o.Type != optionTypeString || o.Name != botLinkOption || !o.Required {
		t.Fatalf("the link option is %+v, want a REQUIRED STRING named %q", o, botLinkOption)
	}
	// The message command: type 3, its default name, and NOTHING else — no
	// description (Discord refuses one), no options, and no contexts or
	// integration_types (those are read only for GLOBAL commands; a guild
	// command is guild-only by construction).
	var raw []map[string]any
	if err := json.Unmarshal(c.body, &raw); err != nil {
		t.Fatal(err)
	}
	if en.Name != botEnrichCommand || en.Type != commandTypeMessage || len(raw[2]) != 2 {
		t.Fatalf("the message command is %v, want exactly name %q and type %d", raw[2], botEnrichCommand, commandTypeMessage)
	}
	// `/settings` (§20 erratum 318; run 4's panel contract §1), pinned key for
	// key: a CHAT_INPUT command with the contract's description, no options,
	// and MANAGE_GUILD as a STRING bitfield — and nothing else (no contexts or
	// integration_types, for the message command's reason above).
	st := raw[3]
	if len(st) != 4 || st["name"] != botSettingsCommand || st["type"] != float64(commandTypeChatInput) ||
		st["description"] != "This server's Clippet: the connection and the link defaults." ||
		st["default_member_permissions"] != "32" {
		t.Fatalf("the settings command is %v, want exactly name, type 1, the contract's description and default_member_permissions \"32\"", st)
	}
	// `/signin` (§20 erratum 341), key for key: a CHAT_INPUT command with its
	// description and nothing else — no options, and no default_member_permissions
	// (REGISTRATION decides who it lets in, not who sees it).
	si := raw[4]
	if len(si) != 3 || si["name"] != botSigninCommand || si["type"] != float64(commandTypeChatInput) ||
		si["description"] != signinDescription || len(signinDescription) > maxDescription {
		t.Fatalf("the signin command is %v, want exactly name %q, type 1 and its description", si, botSigninCommand)
	}
	for _, i := range []int{0, 1, 2, 4} {
		if _, has := raw[i]["default_member_permissions"]; has {
			t.Fatalf("command %d carries default_member_permissions: %v", i, raw[i])
		}
	}
	for _, cmd := range cmds[:2] {
		if cmd.Type != commandTypeChatInput || len(cmd.Description) > maxDescription || cmd.Description == "" {
			t.Fatalf("/%s: type %d, a %d-character description; Discord allows 1-%d", cmd.Name, cmd.Type, len(cmd.Description), maxDescription)
		}
		if o := cmd.Options[0]; o.Description == "" || len(o.Description) > maxDescription {
			t.Fatalf("/%s's option has a %d-character description", cmd.Name, len(o.Description))
		}
	}
	// The upload description names THIS instance, which is where a file Discord
	// refuses to attach at all is uploaded instead (ROADMAP #33's wish list, item 1).
	if !strings.Contains(up.Description, "clips.example") {
		t.Fatalf("the upload description does not name this instance: %q", up.Description)
	}
	// The settled words, verbatim (§20 erratum 315).
	if up.Description != "Upload a clip. You get a link that plays in Discord. Bigger files: clips.example" {
		t.Fatalf("the upload description is %q", up.Description)
	}
	if cl.Description != "Post a clip of this Clippet as a card." || cl.Options[0].Description != "A link to a clip on this Clippet" {
		t.Fatalf("the link command's words are %q / %q", cl.Description, cl.Options[0].Description)
	}
	if up.Options[0].Description != "The video or audio to upload" || strings.Join(up.Options[0].FileTypes, ",") != "video,audio" {
		t.Fatalf("the file option's description moved: %q", up.Options[0].Description)
	}
}

func TestRegisterReportsDiscordsRefusalWithoutTheToken(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "")
	f.answerWith(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"401: Unauthorized for token `+testBotToken+`","code":0}`)
	})
	err := f.bot.Register(context.Background(), testGuildID)
	if err == nil {
		t.Fatal("Register accepted a 401")
	}
	if strings.Contains(err.Error(), testBotToken) {
		t.Fatalf("the error carries the bot token: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("the error does not name the status: %v", err)
	}
}

// TestBotShutdownAnswersTheDeferredMessage: a `/clip` in flight when SIGTERM
// arrives must not leave a "thinking…" that never resolves (§20 erratum 256).
func TestBotShutdownAnswersTheDeferredMessage(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	started := make(chan struct{})
	f.bot.Clip(okJob(f, func(ctx context.Context, src IngestSource) (IngestResult, error) {
		close(started)
		<-ctx.Done() // the shutdown
		return IngestResult{}, ctx.Err()
	}))
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.bot.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if c := content(t, f.waitEdited(t)); c != SentenceRestart {
		t.Fatalf("the reply is %q, want %q", c, SentenceRestart)
	}
}

// TestClipAfterShutdownStartsNothing: the HTTP drain and the worker stop
// overlap, so an interaction CAN arrive after Shutdown began. It must not raise
// the WaitGroup a Wait is already blocked on (the one misuse the runtime
// panics on), and it must not start work whose context is already dead.
func TestClipAfterShutdownStartsNothing(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.bot.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	f.bot.Clip(okJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		t.Error("a job started after the shutdown")
		return IngestResult{}, nil
	}))
	// Nothing was fetched, nothing was called, and the log says why.
	if f.fetches() != 0 {
		t.Fatal("a post-shutdown /clip fetched an attachment")
	}
	if !strings.Contains(f.logs.String(), "arrived during shutdown") {
		t.Fatalf("the refusal is not in the log: %s", f.logs.String())
	}
}

func TestNewBotIsNilWhenTheApplicationIsNotConfigured(t *testing.T) {
	for _, d := range []BotDeps{
		{},
		{AppID: testAppID},
		{Token: testBotToken},
		{AppID: testAppID, Token: testBotToken}, // no API host
	} {
		if b := NewBot(d); b != nil {
			t.Fatalf("NewBot(%+v) built a bot", d)
		}
	}
}

func TestTokenSpellingsCoverTheEncodedForms(t *testing.T) {
	got := tokenSpellings("a.b-c_d~e")
	if len(got) == 0 || got[0] == "" {
		t.Fatalf("tokenSpellings = %v", got)
	}
	// Longest first, so replacing a short spelling can never eat half of a
	// longer one (secretSpellings' rule, applied to a bare token).
	for i := 1; i < len(got); i++ {
		if len(got[i]) > len(got[i-1]) {
			t.Fatalf("tokenSpellings is not longest-first: %v", got)
		}
	}
	if tokenSpellings("") != nil {
		t.Fatal("an empty token produced spellings")
	}
}
