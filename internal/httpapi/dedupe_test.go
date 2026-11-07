package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/drop"
	"clippet/internal/notify"
	"clippet/internal/store"
)

// The duplicate answer (§20 erratum 326): a
// byte-identical upload of a live clip THIS account already holds answers that
// clip and writes nothing — no row, no object, no job — on every path, and the
// opt-out `duplicate=allow` makes a new clip as before.

// uploadChecked is e.upload WITHOUT the harness's opt-out: the duplicate check
// runs. extra parts go between the metadata and the video.
func (e *env) uploadChecked(meta, filename string, video []byte, extra []part, opts ...reqOpt) resp {
	e.t.Helper()
	parts := []part{dedupeOn, {name: "metadata", contentType: "application/json", data: []byte(meta)}}
	parts = append(parts, extra...)
	parts = append(parts, part{name: "video", filename: filename, contentType: "video/mp4", data: video})
	body, ct := multipartBody(e.t, parts...)
	return e.do(http.MethodPost, "/api/clips", body, append([]reqOpt{withHeader("Content-Type", ct)}, opts...)...)
}

// countRows counts a table's rows through a second connection.
func (e *env) countRows(table string) int {
	e.t.Helper()
	raw, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		e.t.Fatal(err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// Twice the same bytes → the same id, one object, one row, one job; the second
// answer is 200 (never 201) with `"existing": true`, the second request's own
// fields are not applied, and the log says it in one INFO line with no file
// name.
func TestDuplicateUploadAnswersTheExistingClip(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	video := testVideo(4000)

	first := e.uploadChecked(`{"title":"First"}`, "a.mp4", video, nil, withCookie(cookie))
	if first.code != 201 || first.has("existing") {
		t.Fatalf("first: %d %s", first.code, first.body)
	}
	id := first.str("id")
	objects, rows, jobs := len(e.fake.Keys()), e.countRows("clips"), e.countRows("jobs")
	spoolBefore, _ := os.ReadDir(e.scratch)

	e.logBuf.Reset()
	second := e.uploadChecked(`{"title":"Second","tags":["other"],"unlisted":true}`, "secret-name.mp4", video, nil, withCookie(cookie))
	if second.code != 200 || second.str("id") != id || second.json["existing"] != true {
		t.Fatalf("second: %d %s", second.code, second.body)
	}
	if got := len(e.fake.Keys()); got != objects {
		t.Fatalf("objects %d → %d: a duplicate wrote to the bucket", objects, got)
	}
	if got := e.countRows("clips"); got != rows {
		t.Fatalf("clip rows %d → %d", rows, got)
	}
	if got := e.countRows("jobs"); got != jobs {
		t.Fatalf("job rows %d → %d", jobs, got)
	}
	c := e.getClip(id)
	if c.Title != "First" || c.Unlisted || second.str("title") != "First" {
		t.Fatalf("the second upload's fields were applied: %q unlisted=%v", c.Title, c.Unlisted)
	}
	if tags, _ := e.db.TagsOf(context.Background(), id); len(tags) != 0 {
		t.Fatalf("the second upload's tags were applied: %v", tags)
	}
	log := e.logBuf.String()
	if !strings.Contains(log, `msg="upload deduped" clip=`+id+" bytes=4000") {
		t.Fatalf("no dedupe line: %s", log)
	}
	if strings.Contains(log, "secret-name") || strings.Contains(log, "upload accepted") {
		t.Fatalf("the dedupe logged a file name or an acceptance: %s", log)
	}
	// The duplicate's spool was removed: scratch holds what it held before.
	if left, _ := os.ReadDir(e.scratch); len(left) != len(spoolBefore) {
		t.Fatalf("scratch held %d entries and holds %d after a duplicate", len(spoolBefore), len(left))
	}
}

// The opt-out: `duplicate=allow` as a part between the metadata and the video
// (POST /api/clips) or as a capture field makes a NEW clip; an empty value
// leaves the check on, and any other value is the grammar's 400 (run 8),
// before a byte of the file is read.
func TestDuplicateOptOutMakesANewClip(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	video := testVideo(3000)
	first := e.uploadChecked(`{}`, "a.mp4", video, nil, withCookie(cookie))
	if first.code != 201 {
		t.Fatalf("first: %d %s", first.code, first.body)
	}
	allowed := e.uploadChecked(`{}`, "a.mp4", video, []part{textPart(duplicateField, "allow")}, withCookie(cookie))
	if allowed.code != 201 || allowed.str("id") == first.str("id") || allowed.has("existing") {
		t.Fatalf("with the opt-out: %d %s", allowed.code, allowed.body)
	}
	other := e.uploadChecked(`{}`, "a.mp4", video, []part{textPart(duplicateField, "yes")}, withCookie(cookie))
	if other.code != 400 || other.errCode() != "invalid_request" || other.str("message") != msgInvalidDuplicate {
		t.Fatalf("a value other than allow: %d %s", other.code, other.body)
	}
	empty := e.uploadChecked(`{}`, "a.mp4", video, []part{textPart(duplicateField, " ")}, withCookie(cookie))
	if empty.code != 200 || empty.json["existing"] != true {
		t.Fatalf("an empty value (not sent: the check stays on): %d %s", empty.code, empty.body)
	}
	// A different text part there is still missing_video, exactly as before.
	bad := e.uploadChecked(`{}`, "a.mp4", video, []part{textPart("title", "x")}, withCookie(cookie))
	if bad.code != 400 || bad.errCode() != "missing_video" {
		t.Fatalf("another part between metadata and video: %d %s", bad.code, bad.body)
	}

	cap1 := e.capture([]part{dedupeOn, filePart("file", "a.mp4", video)}, withCookie(cookie))
	if cap1.code != 200 || cap1.json["existing"] != true {
		t.Fatalf("capture duplicate: %d %s", cap1.code, cap1.body)
	}
	cap2 := e.capture([]part{textPart(duplicateField, "allow"), filePart("file", "a.mp4", video)}, withCookie(cookie))
	if cap2.code != 201 || cap2.has("existing") {
		t.Fatalf("capture with the opt-out: %d %s", cap2.code, cap2.body)
	}
	cap3 := e.capture([]part{textPart(duplicateField, "Allow!"), filePart("file", "a.mp4", video)}, withCookie(cookie))
	if cap3.code != 400 || cap3.errCode() != "invalid_request" || cap3.str("message") != msgInvalidDuplicate {
		t.Fatalf("capture with another value: %d %s", cap3.code, cap3.body)
	}
	cap4 := e.capture([]part{textPart(duplicateField, ""), filePart("file", "a.mp4", video)}, withCookie(cookie))
	if cap4.code != 200 || cap4.json["existing"] != true {
		t.Fatalf("capture with an empty value: %d %s", cap4.code, cap4.body)
	}
}

// Never across accounts: another account's identical bytes are its own clip.
func TestDuplicateNeverAcrossAccounts(t *testing.T) {
	e := newEnv(t)
	_, a := e.newUser("alice")
	_, b := e.newUser("bob")
	video := testVideo(2500)
	ra := e.uploadChecked(`{"unlisted":true}`, "a.mp4", video, nil, withCookie(a))
	rb := e.uploadChecked(`{}`, "a.mp4", video, nil, withCookie(b))
	if ra.code != 201 || rb.code != 201 || ra.str("id") == rb.str("id") || rb.has("existing") {
		t.Fatalf("alice %d %s / bob %d %s", ra.code, ra.body, rb.code, rb.body)
	}
}

// Only a LIVE clip is answered: queued (above), processing and ready match; a
// failed clip, a `deleting` one and one past its expiry never do.
func TestDuplicateMatchesOnlyALiveClip(t *testing.T) {
	video := testVideo(1000) // seedClip's own default source
	ten := int64(10)
	cases := []struct {
		name  string
		seed  func(e *env, u db.User) db.Clip
		match bool
	}{
		{"processing", func(e *env, u db.User) db.Clip { return e.processingClip(u, clipOpts{source: video}) }, true},
		{"ready", func(e *env, u db.User) db.Clip { c, _, _ := e.readyClip(u, clipOpts{source: video}); return c }, true},
		{"failed", func(e *env, u db.User) db.Clip { return e.failedClip(u, clipOpts{source: video}) }, false},
		{"deleting", func(e *env, u db.User) db.Clip { return e.deletingClip(u) }, false},
		{"expired", func(e *env, u db.User) db.Clip {
			return e.seedClip(u, clipOpts{source: video, ttl: &ten, createdAt: e.now() - 100})
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			u, cookie := e.newUser("carol")
			prior := tc.seed(e, u)
			r := e.uploadChecked(`{}`, "a.mp4", video, nil, withCookie(cookie))
			if tc.match {
				if r.code != 200 || r.str("id") != prior.ID {
					t.Fatalf("a %s clip was not answered: %d %s", tc.name, r.code, r.body)
				}
				return
			}
			if r.code != 201 || r.str("id") == prior.ID {
				t.Fatalf("a %s clip was answered: %d %s", tc.name, r.code, r.body)
			}
		})
	}
}

// The spool's hash is the store's: spoolBody hashes what it writes with the
// same function PutFile applies, so the duplicate check compares like with
// like and `source_etag` is what it matches against.
func TestSpoolHashIsTheStoresHash(t *testing.T) {
	video := testVideo(70000)
	h := newHandler(Deps{Cfg: withDefaults(newEnv(t).cfg), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	dir := filepath.Join(t.TempDir(), "spool")
	dst := filepath.Join(dir, "source.mp4")
	n, sum, err := h.spoolBody(context.Background(), bytes.NewReader(video), dir, dst)
	if err != nil || n != int64(len(video)) {
		t.Fatalf("spool: %d %v", n, err)
	}
	res, err := store.NewFake().PutFile(context.Background(), "k", dst, "video/mp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum != res.SHA256 || sum != sha256hex(video) {
		t.Fatalf("spool %s, store %s, bytes %s", sum, res.SHA256, sha256hex(video))
	}
	// And through the route: the clip records exactly that hash.
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	r := e.uploadChecked(`{}`, "a.mp4", video, nil, withCookie(cookie))
	if r.code != 201 || e.getClip(r.str("id")).SourceETag != sum {
		t.Fatalf("the clip's etag is not the spool's hash: %d %s", r.code, r.body)
	}
	if strings.Contains(e.logBuf.String(), "differ") {
		t.Fatalf("the two hashes differed: %s", e.logBuf.String())
	}
}

// The inbound URL: a duplicate is the same two shapes with 200.
func TestDuplicateInboundAnswers200(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("carol")
	raw := e.newInbound(u)
	video := testVideo(4000)
	first := e.inbound(raw, []part{dedupeOn, filePart("file", "a.mp4", video)})
	if first.code != 201 {
		t.Fatalf("first: %d %s", first.code, first.body)
	}
	text := e.inbound(raw, []part{dedupeOn, filePart("file", "a.mp4", video)})
	if text.code != 200 || string(text.body) != string(first.body) {
		t.Fatalf("text duplicate: %d %q, want 200 %q", text.code, text.body, first.body)
	}
	js := e.inbound(raw, []part{dedupeOn, filePart("file", "a.mp4", video)}, withHeader("Accept", "application/json"))
	if js.code != 200 || js.json["existing"] != true || testOrigin+"/"+js.str("id")+"\n" != string(first.body) {
		t.Fatalf("json duplicate: %d %s", js.code, js.body)
	}
}

// The Clippet Auto URL: 200 + existing for a duplicate, the instruction's
// `duplicate: allow` makes a new clip, and another value is the grammar's 400.
func TestDuplicateAutoFetch(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	e, _ := afEnv(t, stub)
	u, _ := e.newUser("carol")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	first := e.autoFetch(raw, afBody())
	if first.code != 201 {
		t.Fatalf("first: %d %s", first.code, first.body)
	}
	dup := e.autoFetch(raw, afBody())
	if dup.code != 200 || dup.str("id") != first.str("id") || dup.json["existing"] != true {
		t.Fatalf("duplicate: %d %s", dup.code, dup.body)
	}
	allowed := e.autoFetch(raw, afBody(func(m map[string]any) { m["duplicate"] = "allow" }))
	if allowed.code != 201 || allowed.str("id") == first.str("id") {
		t.Fatalf("with the opt-out: %d %s", allowed.code, allowed.body)
	}
	bad := e.autoFetch(raw, afBody(func(m map[string]any) { m["duplicate"] = "please" }))
	if bad.code != 400 {
		t.Fatalf("a bad opt-out value: %d %s", bad.code, bad.body)
	}
}

// formDupIngest runs the job's ingest with FIXED bytes, so the second call is
// the duplicate.
func formDupIngest(t *testing.T, job notify.ClipJob) notify.IngestResult {
	t.Helper()
	const body = "the same video bytes"
	res, err := job.Ingest(context.Background(), notify.IngestSource{
		Body: strings.NewReader(body), Filename: job.Filename, ContentType: "video/mp4", Size: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return res
}

// THE TRAP (the opinion's §3): the upload form never applies a second
// person's fields to the clip that was already there — not a submit that
// arrives after the duplicate (answered, not written, no redraw), and not one
// held before it (dropped).
func TestUploadFormDuplicateAppliesNothing(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "dupowner")
	_, job, _ := b.openForm(t, "clip.mp4")
	if !job.Form {
		t.Fatal("the form's job is not marked Form")
	}
	first := formDupIngest(t, job)
	if first.Existing {
		t.Fatal("the first upload was a duplicate")
	}
	before := b.getClip(first.ClipID)

	// A submit AFTER the duplicate.
	nonce, job, _ := b.openForm(t, "clip.mp4")
	second := formDupIngest(t, job)
	if !second.Existing || second.ClipID != first.ClipID {
		t.Fatalf("second: %+v", second)
	}
	if second.SubmitDropped {
		t.Fatal("no submit was held, yet the job is told to say the line: the later submit would hear it twice")
	}
	r := b.post(formSubmit(formGuild, nonce, "Someone else's title", "1", "theirs"))
	assertEphemeral(t, r, notify.SentenceFormNotApplied)
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "again", "1", "")), msgFormExpired)

	// A submit held BEFORE the duplicate.
	nonce, job, _ = b.openForm(t, "clip.mp4")
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "Held title", "7", "held")), msgSaved)
	if third := formDupIngest(t, job); !third.Existing || !third.SubmitDropped {
		t.Fatalf("third (a held submit was answered Changes saved. and dropped; the job must tell them): %+v", third)
	}

	after := b.getClip(first.ClipID)
	if after.Title != before.Title || (after.TTLSeconds == nil) != (before.TTLSeconds == nil) ||
		(after.TTLSeconds != nil && *after.TTLSeconds != *before.TTLSeconds) {
		t.Fatalf("the existing clip was rewritten: %q %v → %q %v", before.Title, before.TTLSeconds, after.Title, after.TTLSeconds)
	}
	if tags := b.tags(t, first.ClipID); len(tags) != 0 {
		t.Fatalf("tags were applied: %v", tags)
	}
	if got := b.bot.redrawn(); len(got) != 0 {
		t.Fatalf("a redraw was asked for: %v", got)
	}
}

// The drop folder: a file that is already a clip of the folder's account stays
// where it is, with `already here <link>` beside it, and nothing is created.
// The note is `<name>.clippet.txt` (erratum 326, amended run 8), never the
// `.failed.txt` a refusal writes — and an earlier refusal's note goes.
func TestDropDuplicateLeavesTheFileWithANote(t *testing.T) {
	dir := t.TempDir()
	e, w, owner := newBucketDropEnv(t, dir)
	body := testVideo(2048)
	prior := e.seedClip(owner, clipOpts{source: body})
	if err := os.WriteFile(filepath.Join(dir, "again.mp4"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "again.mp4"+drop.FailedSuffix)
	if err := os.WriteFile(stale, []byte("an earlier refusal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(dir, "again.mp4"+drop.NoteSuffix)
	runWatcher(t, w, func() bool { _, err := os.Stat(note); return err == nil })
	got, err := os.ReadFile(note)
	if err != nil {
		t.Fatalf("no note; the folder holds %v", dirNames(t, dir))
	}
	if want := "already here " + testOrigin + "/" + prior.ID + "\n"; string(got) != want {
		t.Fatalf("the note is %q, want %q", got, want)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("the earlier refusal's .failed.txt is still beside a file that is already here")
	}
	if _, err := os.Stat(filepath.Join(dir, "again.mp4")); err != nil {
		t.Fatalf("the file did not stay where it was: %v", err)
	}
	if n := e.countRows("clips"); n != 1 {
		t.Fatalf("%d clip rows, want the one", n)
	}
}

// The import verb asks the core's question: a failed clip of the same bytes is
// not "already here", a ready one is.
func TestImporterDuplicatesIsTheCoresQuestion(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser("carol")
	im := NewImporter(Deps{Cfg: e.cfg, DB: e.db, Store: e.fake, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	video := testVideo(1000)
	e.failedClip(u, clipOpts{source: video})
	if d, err := im.Duplicates(context.Background(), u.ID, int64(len(video))); err != nil || len(d) != 0 {
		t.Fatalf("a failed clip is already here: %v %v", d, err)
	}
	c, _, _ := e.readyClip(u, clipOpts{source: video})
	d, err := im.Duplicates(context.Background(), u.ID, int64(len(video)))
	if err != nil || len(d) != 1 || d[0].ID != c.ID || d[0].ETag != sha256hex(video) {
		t.Fatalf("a ready clip is not already here: %v %v", d, err)
	}
}
