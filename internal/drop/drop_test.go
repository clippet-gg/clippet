package drop

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clippet/internal/transcode"
)

// --- the fakes --------------------------------------------------------------

// memSeen is migration 0018's table in a map. The KEY is the same triple the
// real one keys on, which is the whole of what these tests want to assert about
// it.
type memSeen struct {
	rows   map[stat]string // key → clip id ("" = a refusal)
	pruned int
	fail   error
}

func newMemSeen() *memSeen { return &memSeen{rows: map[stat]string{}} }

func (m *memSeen) DropSeen(_ context.Context, name string, size, mtime int64) (bool, error) {
	if m.fail != nil {
		return false, m.fail
	}
	_, ok := m.rows[stat{name, size, mtime}]
	return ok, nil
}

func (m *memSeen) MarkDropSeen(_ context.Context, name string, size, mtime, _ int64, clipID string) error {
	m.rows[stat{name, size, mtime}] = clipID
	return nil
}

func (m *memSeen) PruneDropSeen(context.Context, int64) (int, error) {
	m.pruned++
	return 0, nil
}

// fixture is one watcher over one temp directory, with the ingest and the probe
// scripted.
type fixture struct {
	t    *testing.T
	dir  string
	seen *memSeen
	w    *Watcher
	// logs is everything the watcher said, DEBUG and above: "one line per
	// file" is a behaviour of this package and not a nicety, so a test reads
	// it.
	logs *bytes.Buffer
	// now is the watcher's clock, so a test steps over a retry backoff instead
	// of sleeping through one.
	now time.Time

	// ingested is every filename handed to the ingest, in order; ingestErr is
	// what the ingest answers; bodies is what it read.
	ingested  []string
	bodies    map[string][]byte
	ingestErr error
	// probeErr is what the probe answers for a given base name.
	probeErr map[string]error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), seen: newMemSeen(), logs: &bytes.Buffer{},
		now:    time.Unix(1_700_000_000, 0),
		bodies: map[string][]byte{}, probeErr: map[string]error{}}
	f.w = New(Deps{
		Dir: f.dir, Owner: "carol", OwnerID: "u1", Seen: f.seen,
		Now: func() time.Time { return f.now },
		Log: slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Probe: func(_ context.Context, path string) (*transcode.ProbeResult, error) {
			if err := f.probeErr[filepath.Base(path)]; err != nil {
				return nil, err
			}
			return &transcode.ProbeResult{VideoIndex: 0}, nil
		},
	})
	f.w.SetIngest(func(_ context.Context, src Source) (Result, error) {
		b, err := io.ReadAll(src.Body)
		if err != nil {
			return Result{}, err
		}
		f.ingested = append(f.ingested, src.Filename)
		f.bodies[src.Filename] = b
		if f.ingestErr != nil {
			return Result{}, f.ingestErr
		}
		return Result{ClipID: "clip-" + src.Filename}, nil
	})
	return f
}

// write puts a file at the root of the folder.
func (f *fixture) write(name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// settle runs the two ticks a stable file needs: one to see it, one to take it.
func (f *fixture) settle() {
	f.w.tick(context.Background())
	f.w.tick(context.Background())
}

func (f *fixture) exists(parts ...string) bool {
	_, err := os.Lstat(filepath.Join(append([]string{f.dir}, parts...)...))
	return err == nil
}

// saidTimes counts the log lines carrying a phrase — the assertion behind every
// "exactly once" in this package's contract.
func (f *fixture) saidTimes(phrase string) int {
	return strings.Count(f.logs.String(), phrase)
}

func (f *fixture) read(parts ...string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{f.dir}, parts...)...))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// --- the tests --------------------------------------------------------------

// A file is taken only once nothing about it has moved for a whole tick, and
// taking it means the person's folder no longer holds it.
func TestDropTakesAStableFileAndLeavesAGrowingOne(t *testing.T) {
	f := newFixture(t)
	f.write("clip.mp4", "video-bytes")

	f.w.tick(context.Background())
	if len(f.ingested) != 0 {
		t.Fatalf("a file was taken on its FIRST sighting: %v", f.ingested)
	}
	if !f.exists("clip.mp4") {
		t.Fatal("the file left the root before it was stable")
	}

	f.w.tick(context.Background())
	if len(f.ingested) != 1 || f.ingested[0] != "clip.mp4" {
		t.Fatalf("a stable file was not ingested: %v", f.ingested)
	}
	if got := string(f.bodies["clip.mp4"]); got != "video-bytes" {
		t.Fatalf("the ingest read %q", got)
	}
	if f.exists("clip.mp4") {
		t.Fatal("the file is still at the root after a successful ingest")
	}
	if f.exists(StageDir, "clip.mp4") {
		t.Fatal("the staged copy was not removed after a successful ingest")
	}
	if id := f.seen.rows[f.key("clip.mp4", 11)]; id != "clip-clip.mp4" {
		t.Fatalf("the decision was not recorded with its clip id: %q", id)
	}
}

// A file that is still being written keeps changing, and a changing file is
// never taken however many ticks pass.
func TestDropNeverTakesAGrowingFile(t *testing.T) {
	f := newFixture(t)
	f.write("growing.mp4", "aaa")
	for i := 0; i < 4; i++ {
		f.w.tick(context.Background())
		f.write("growing.mp4", strings.Repeat("a", 4+i))
	}
	if len(f.ingested) != 0 {
		t.Fatalf("a growing file was taken: %v", f.ingested)
	}
	if !f.exists("growing.mp4") {
		t.Fatal("a growing file left the root")
	}
}

// The root of this folder is a place a person keeps things. These are the names
// the watcher never touches, whatever else is true of them.
func TestDropSkipsWhatIsNotItsBusiness(t *testing.T) {
	f := newFixture(t)
	names := []string{
		".clippet-store",       // the store's own marker
		".hidden.mp4",          // any dotfile
		"half.mp4.part",        // a download in flight
		"half.mp4.crdownload",  //
		"half.mp4.tmp",         //
		"clip.mp4.failed.txt",  // our own note from an earlier refusal
		"clip.mp4.clippet.txt", // our own note beside a duplicate
		"CLIP.MP4.CLIPPET.TXT", // in any case
		"empty.mp4",            // zero bytes
	}
	for _, n := range names {
		f.write(n, "x")
	}
	if err := os.WriteFile(filepath.Join(f.dir, "empty.mp4"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.dir, "clips", "abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.settle()
	if len(f.ingested) != 0 {
		t.Fatalf("the watcher took something it should not have: %v", f.ingested)
	}
	for _, n := range names {
		if !f.exists(n) {
			t.Fatalf("%s left the root", n)
		}
	}
	if !f.exists("clips", "abc") {
		t.Fatal("the store's own tree was disturbed")
	}
}

// Anything the probe does not accept as a video stays exactly where it is, with
// no note beside it: a screenshot in this folder is not a failure.
func TestDropLeavesANonVideoAlone(t *testing.T) {
	f := newFixture(t)
	f.write("notes.txt", "hello")
	f.probeErr["notes.txt"] = errors.New("no video stream")
	f.settle()
	if len(f.ingested) != 0 {
		t.Fatalf("a non-video was ingested: %v", f.ingested)
	}
	if !f.exists("notes.txt") {
		t.Fatal("a non-video left the root")
	}
	if f.exists("notes.txt" + FailedSuffix) {
		t.Fatal("a non-video got a .failed.txt: the folder is a place people keep things")
	}
	if len(f.seen.rows) != 0 {
		t.Fatalf("a non-video was recorded as decided: %v", f.seen.rows)
	}
}

// A refused ingest puts the file back exactly where it was, says why beside it,
// and never tries it again.
func TestDropPutsBackWhatAnIngestRefused(t *testing.T) {
	f := newFixture(t)
	f.ingestErr = errors.New("it is larger than this instance accepts (MAX_UPLOAD_BYTES)")
	f.write("big.mp4", "video-bytes")
	f.write("big.mp4"+NoteSuffix, "already here somewhere") // an earlier duplicate's note
	f.settle()
	if f.exists("big.mp4" + NoteSuffix) {
		t.Fatal("a refusal left an earlier duplicate's already-here note beside the file")
	}

	if len(f.ingested) != 1 {
		t.Fatalf("the ingest was not attempted once: %v", f.ingested)
	}
	if !f.exists("big.mp4") {
		t.Fatal("a refused file was not put back at the root")
	}
	if f.exists(StageDir, "big.mp4") {
		t.Fatal("a refused file was left in the staging directory")
	}
	note := f.read("big.mp4" + FailedSuffix)
	if !strings.Contains(note, "MAX_UPLOAD_BYTES") || !strings.Contains(note, "big.mp4") {
		t.Fatalf("the note does not say what happened: %q", note)
	}
	// And it is decided: further ticks do nothing at all.
	f.settle()
	f.settle()
	if len(f.ingested) != 1 {
		t.Fatalf("a refused file was retried: %v", f.ingested)
	}
}

// The one refusal that is about the FILE rather than about the clip: this
// process cannot read it. Skipped when the test runs as root, for whom a
// mode-000 file is readable anyway — E2E step 91 proves it on the real image,
// which runs as uid 10001.
func TestDropWritesANoteForAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-000 file is still readable, so there is nothing to refuse")
	}
	f := newFixture(t)
	f.write("locked.mp4", "video-bytes")
	if err := os.Chmod(filepath.Join(f.dir, "locked.mp4"), 0o000); err != nil {
		t.Skipf("this filesystem does not honour chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(f.dir, "locked.mp4"), 0o644) })
	f.settle()

	if len(f.ingested) != 0 {
		t.Fatalf("an unreadable file reached the ingest: %v", f.ingested)
	}
	if !f.exists("locked.mp4") {
		t.Fatal("an unreadable file left the root")
	}
	note := f.read("locked.mp4" + FailedSuffix)
	if !strings.Contains(note, "could not be opened") {
		t.Fatalf("the note does not say what happened: %q", note)
	}
}

// The restart case, which is the whole reason migration 0018 exists: a file the
// database has already decided about is never ingested a second time, whatever
// put it back at the root.
func TestDropNeverIngestsTheSameFileTwice(t *testing.T) {
	f := newFixture(t)
	f.write("again.mp4", "video-bytes")
	f.settle()
	if len(f.ingested) != 1 {
		t.Fatalf("the first ingest did not happen: %v", f.ingested)
	}
	// The same bytes, under the same name, with the same mtime: the shape our
	// own recovery pass puts back.
	key := f.key("again.mp4", 11)
	f.write("again.mp4", "video-bytes")
	if err := os.Chtimes(filepath.Join(f.dir, "again.mp4"), time.Unix(key.mtime, 0), time.Unix(key.mtime, 0)); err != nil {
		t.Fatal(err)
	}
	f.settle()
	if len(f.ingested) != 1 {
		t.Fatalf("the same file was ingested twice: %v", f.ingested)
	}
	if !f.exists("again.mp4") {
		t.Fatal("a file that was already decided about was moved anyway")
	}

	// A file whose CONTENT changed is a different file and is taken.
	f.write("again.mp4", "different-bytes-entirely")
	f.settle()
	if len(f.ingested) != 2 {
		t.Fatalf("an edited file was not re-ingested: %v", f.ingested)
	}
}

// A process killed mid-ingest leaves a staged file. The next boot puts it back
// where the person left it rather than leaving it somewhere they cannot see.
func TestDropRecoversAStagedFileAtBoot(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.dir, StageDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, StageDir, "interrupted.mp4"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.w.recoverStaged()
	if !f.exists("interrupted.mp4") {
		t.Fatal("a staged file was not put back at the root")
	}
	if f.exists(StageDir, "interrupted.mp4") {
		t.Fatal("the staged copy is still there")
	}

	// And when the name is taken at the root, neither file is touched.
	f.write("taken.mp4", "at the root")
	if err := os.WriteFile(filepath.Join(f.dir, StageDir, "taken.mp4"), []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.w.recoverStaged()
	if got := f.read("taken.mp4"); got != "at the root" {
		t.Fatalf("the file at the root was overwritten: %q", got)
	}
	if got := f.read(StageDir, "taken.mp4"); got != "staged" {
		t.Fatalf("the staged file was lost: %q", got)
	}
}

// The orphan recoverStaged deliberately left alone has to survive the TICKS
// that follow, not only the boot pass. os.Rename replaces its destination
// silently and a successful ingest then removes what it wrote over — so an
// unguarded stage destroys the person's only copy of the video an earlier run
// was killed in the middle of.
func TestDropNeverStagesOverAnOrphanOfTheSameName(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.dir, StageDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, StageDir, "clip.mp4"), []byte("the orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.write("clip.mp4", "video-bytes")

	f.w.recoverStaged() // WARNs: the root name is taken, so the orphan stays put
	f.settle()          // …and this is the tick that used to destroy it

	if got := f.read(StageDir, "clip.mp4"); got != "the orphan" {
		t.Fatalf("the staged file was overwritten by the file at the root: %q", got)
	}
	if len(f.ingested) != 0 {
		t.Fatalf("a file was staged over an orphan of its own name: %v", f.ingested)
	}
	if got := f.read("clip.mp4"); got != "video-bytes" {
		t.Fatalf("the file at the root did not stay where it was: %q", got)
	}
	if len(f.seen.rows) != 0 {
		t.Fatalf("the collision was recorded as a decision about the file: %v", f.seen.rows)
	}
	if f.exists("clip.mp4" + FailedSuffix) {
		t.Fatal("the collision wrote a note blaming the file for it")
	}
	if n := f.saidTimes("already staged"); n != 1 {
		t.Fatalf("the collision was reported %d times, want once", n)
	}

	// And once the orphan is out of the way the file is taken like any other.
	if err := os.Remove(filepath.Join(f.dir, StageDir, "clip.mp4")); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * retryBackoff)
	f.settle()
	if len(f.ingested) != 1 || f.ingested[0] != "clip.mp4" {
		t.Fatalf("the file was not taken once the staging directory was clear: %v", f.ingested)
	}
}

// A refusal that is about this INSTANCE's minute — the hour's upload budget,
// a momentarily full working disk — is a pause, not a verdict. Recording it
// would strand the tail of a bulk drop for thirty days behind a text file each.
func TestDropDoesNotDecideAgainstAFileTheInstanceMerelyCannotTakeYet(t *testing.T) {
	f := newFixture(t)
	f.ingestErr = &NotNow{Err: errors.New("this account has reached its upload limit for the hour (RATE_UPLOAD_PER_HOUR)")}
	f.write("bulk.mp4", "video-bytes")
	f.settle()

	if len(f.ingested) != 1 {
		t.Fatalf("the ingest was not attempted once: %v", f.ingested)
	}
	if !f.exists("bulk.mp4") {
		t.Fatal("the file was not put back at the root")
	}
	if f.exists("bulk.mp4" + FailedSuffix) {
		t.Fatal("a momentary refusal wrote a note blaming the file")
	}
	if len(f.seen.rows) != 0 {
		t.Fatalf("a momentary refusal was recorded as a decision: %v", f.seen.rows)
	}
	// It waits rather than re-staging and re-probing twelve times a minute.
	f.settle()
	if len(f.ingested) != 1 {
		t.Fatalf("the file was retried inside the backoff: %v", f.ingested)
	}
	// …and once the budget has refilled it is taken, with nobody touching it.
	f.ingestErr = nil
	f.now = f.now.Add(2 * retryBackoff)
	f.settle()
	if len(f.ingested) != 2 {
		t.Fatalf("the file was not looked at again after the backoff: %v", f.ingested)
	}
	if f.exists("bulk.mp4") {
		t.Fatal("the file is still at the root after it was finally taken")
	}
}

// One DEBUG line per file means ONE, for the lifetime of the file in the
// folder. A name that never becomes a candidate at all — a symlink, a fifo —
// is the case that used to explain itself every five seconds for ever, because
// the housekeeping that bounds the map only knew about candidates.
func TestDropExplainsAnUntouchableFileExactlyOnce(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink(filepath.Join(f.dir, "elsewhere.mp4"), filepath.Join(f.dir, "linked.mp4")); err != nil {
		t.Skipf("this filesystem does not make symlinks: %v", err)
	}
	for i := 0; i < 5; i++ {
		f.w.tick(context.Background())
	}
	if n := f.saidTimes("is not a regular file"); n != 1 {
		t.Fatalf("a symlink at the root was explained %d times over five ticks, want once", n)
	}
	if !f.exists("linked.mp4") {
		t.Fatal("the symlink was touched")
	}
}

// A database that cannot answer refuses nothing and takes nothing: the file
// stays where it is and the next tick asks again.
func TestDropTakesNothingWhenTheDecisionsTableCannotBeRead(t *testing.T) {
	f := newFixture(t)
	f.seen.fail = errors.New("database is locked")
	f.write("clip.mp4", "video-bytes")
	f.settle()
	if len(f.ingested) != 0 {
		t.Fatalf("a file was taken with no way to remember it: %v", f.ingested)
	}
	if !f.exists("clip.mp4") {
		t.Fatal("the file left the root")
	}
	if f.exists("clip.mp4" + FailedSuffix) {
		t.Fatal("an unreadable database wrote a note blaming the file")
	}
}

// The folder is OURS, so the watcher makes it. On `fs` the store has already
// created it; on `s3` (§20 erratum 296 (m)) nothing else does, and a watcher
// that warned every five seconds about a directory it was allowed to create
// would be the first thing an operator turned off.
func TestDropMakesTheFolderItWatches(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.dir, "not-there-yet")
	w := New(Deps{
		Dir: dir, Owner: "carol", OwnerID: "u1", Seen: f.seen,
		Now: func() time.Time { return f.now },
		Log: slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	w.SetIngest(func(context.Context, Source) (Result, error) { return Result{}, nil })
	// A cancelled context: Run makes the folder, recovers anything staged and
	// returns without ever ticking, which is exactly the boot moment under test.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("the watcher did not make the folder it watches: %v", err)
	}
}

// key is the (name, size, mtime) the folder's own rules produce for a file.
func (f *fixture) key(name string, size int64) stat {
	f.t.Helper()
	fi, err := os.Stat(filepath.Join(f.dir, name))
	if err != nil {
		// The file has been ingested and is gone: read the one row the fake
		// recorded for that name instead.
		for s := range f.seen.rows {
			if s.name == name {
				return s
			}
		}
		f.t.Fatalf("no file and no decision for %s", name)
	}
	return stat{name: name, size: fi.Size(), mtime: fi.ModTime().Unix()}
}
