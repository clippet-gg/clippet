package httpapi

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/drop"
	"clippet/internal/ids"
	"clippet/internal/transcode"
)

// dropRecorder is the watcher as this package sees it: an owner id, and
// somewhere for NewHandler to put the upload path.
type dropRecorder struct {
	id     string
	ingest drop.Ingest
}

func (d *dropRecorder) OwnerID() string          { return d.id }
func (d *dropRecorder) SetIngest(in drop.Ingest) { d.ingest = in }

// newDropEnv builds an env whose handler has wired the drop folder to a real
// account. The user is created inside the mutate hook because NewHandler asks
// the watcher for its owner id while it is being built — which is exactly the
// order `clippet serve` boots in (resolve the account, then the handler).
func newDropEnv(t *testing.T, mutate ...func(*Deps)) (*env, *dropRecorder, db.User) {
	t.Helper()
	rec := &dropRecorder{}
	var owner db.User
	e := newEnv(t, func(d *Deps) {
		u, err := d.DB.CreateUser(context.Background(), ids.New(), "dropowner", "$2a$12$notahash", d.Started.Unix())
		if err != nil {
			t.Fatal(err)
		}
		owner = u
		rec.id = u.ID
		d.Cfg.DropFolderOwner = "dropowner"
		d.Drop = rec
		for _, m := range mutate {
			m(d)
		}
	})
	return e, rec, owner
}

// The wiring itself: a watcher handed to NewHandler leaves with THIS handler's
// upload path, bound to the account `serve` resolved. Nothing else can give it
// one, and a second ingest of its own would carry a second upload budget.
func TestDropWatcherIsWiredToTheHandlersUploadPath(t *testing.T) {
	_, rec, _ := newDropEnv(t)
	if rec.ingest == nil {
		t.Fatal("NewHandler did not hand the watcher an ingest")
	}
}

// A file the folder takes becomes an ordinary clip: the owner is the configured
// account, the title is the filename without its extension (erratum 60's rule
// for every other upload), and the worker is woken.
func TestDropIngestCreatesAClipForTheOwner(t *testing.T) {
	e, rec, owner := newDropEnv(t)
	body := testVideo(2048)

	res, err := rec.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "Team fight 3.mp4", Size: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("the drop ingest failed: %v", err)
	}
	if res.ClipID == "" {
		t.Fatal("the drop ingest returned no clip id")
	}
	clip := e.getClip(res.ClipID)
	if clip.OwnerID != owner.ID {
		t.Fatalf("the clip is owned by %q, want the configured account %q", clip.OwnerID, owner.ID)
	}
	if clip.Title != "Team fight 3" {
		t.Fatalf("the title is %q, want the filename without its extension", clip.Title)
	}
	if clip.SourceFilename != "Team fight 3.mp4" {
		t.Fatalf("the source filename is %q", clip.SourceFilename)
	}
	if e.worker.woken.Load() == 0 {
		t.Fatal("the worker was not woken")
	}
}

// The gates are the upload routes' own, and the sentences that come back are
// the ones that will be sitting in a text file beside the video.
func TestDropIngestRefusesWhatTheUploadRoutesRefuse(t *testing.T) {
	// Too large: the same MAX_UPLOAD_BYTES, through the same capped reader.
	_, rec, _ := newDropEnv(t, func(d *Deps) { d.Cfg.MaxUploadBytes = 64 })
	body := testVideo(2048)
	if _, err := rec.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "big.mp4", Size: int64(len(body)),
	}); err == nil || !strings.Contains(err.Error(), "MAX_UPLOAD_BYTES") {
		t.Fatalf("an oversized drop was answered %v", err)
	}

	// Not a video: the same ffprobe gate, and a sentence that does not blame
	// the instance for what the file is.
	e, rec, _ := newDropEnv(t)
	e.probe = func(context.Context, string) (*transcode.ProbeResult, error) {
		return nil, &transcode.ErrPermanent{Err: errors.New("moov atom not found")}
	}
	if _, err := rec.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "half.mp4", Size: int64(len(body)),
	}); err == nil || !strings.Contains(err.Error(), "video or audio stream") {
		t.Fatalf("a non-video drop was answered %v", err)
	}

	// The storage quota, whose ONE sentence every surface of the cap says —
	// the same string the 507 and the bot's reply carry. The first drop fills
	// the line; the second meets it.
	_, rec2, _ := newDropEnv(t, func(d *Deps) {
		d.Cfg.StorageBudgetBytes, d.Cfg.StorageBudgetHard = int64(len(body)), true
	})
	if _, err := rec2.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "first.mp4", Size: int64(len(body)),
	}); err != nil {
		t.Fatalf("the first drop into an empty capped instance: %v", err)
	}
	if _, err := rec2.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "over.mp4", Size: int64(len(body)),
	}); err == nil || err.Error() != quotaRefusal {
		t.Fatalf("a drop past the quota was answered %v", err)
	}

	// The ACCOUNT's own upload budget, which the folder does not get to skip.
	e3, rec3, _ := newDropEnv(t, func(d *Deps) { d.Cfg.RateUploadPerHour = 1 })
	_ = e3
	if _, err := rec3.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(testVideo(1024)), Filename: "one.mp4", Size: 1024,
	}); err != nil {
		t.Fatalf("the first drop of the hour was refused: %v", err)
	}
	if _, err := rec3.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(testVideo(1024)), Filename: "two.mp4", Size: 1024,
	}); err == nil || !strings.Contains(err.Error(), "RATE_UPLOAD_PER_HOUR") {
		t.Fatalf("a drop past the account's upload budget was answered %v", err)
	}
}

// Which refusals are the FILE's and which are this instance's minute — the
// difference between a row in `drop_seen` with a text file beside the video for
// thirty days, and a file the next tick picks up by itself (erratum 296 (l)).
// A hundred clips dropped at once meets the hourly budget on clip 61, and the
// other forty must not need a human to `touch` each one.
func TestDropIngestSeparatesAMomentaryRefusalFromADecision(t *testing.T) {
	isNotNow := func(err error) bool {
		var n *drop.NotNow
		return errors.As(err, &n)
	}
	body := testVideo(2048)

	// The instance's minute: the hour's upload budget.
	_, rec, _ := newDropEnv(t, func(d *Deps) { d.Cfg.RateUploadPerHour = 1 })
	if _, err := rec.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "bulk1.mp4", Size: int64(len(body)),
	}); err != nil {
		t.Fatalf("the first drop of the hour was refused: %v", err)
	}
	_, err := rec.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "bulk2.mp4", Size: int64(len(body)),
	})
	if !isNotNow(err) {
		t.Fatalf("the hour's upload budget was answered as a decision about the file: %v", err)
	}

	// The instance's minute: no room on the working disk. The precheck answers
	// with the size the filesystem already reported, before a byte is copied.
	_, recS, _ := newDropEnv(t, func(d *Deps) { d.Cfg.ScratchMinFree = 1 << 62 })
	if _, err := recS.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "scratch.mp4", Size: int64(len(body)),
	}); !isNotNow(err) {
		t.Fatalf("a momentarily full working disk was answered as a decision about the file: %v", err)
	}

	// And the file's own, which stay decisions: too large, and past the quota.
	_, recBig, _ := newDropEnv(t, func(d *Deps) { d.Cfg.MaxUploadBytes = 64 })
	if _, err := recBig.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "big.mp4", Size: int64(len(body)),
	}); err == nil || isNotNow(err) {
		t.Fatalf("a file too large for this instance was answered %v", err)
	}
	_, recQ, _ := newDropEnv(t, func(d *Deps) {
		d.Cfg.StorageBudgetBytes, d.Cfg.StorageBudgetHard = int64(len(body)), true
	})
	if _, err := recQ.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "first.mp4", Size: int64(len(body)),
	}); err != nil {
		t.Fatalf("the first drop into an empty capped instance: %v", err)
	}
	if _, err := recQ.ingest(context.Background(), drop.Source{
		Body: bytes.NewReader(body), Filename: "over.mp4", Size: int64(len(body)),
	}); err == nil || isNotNow(err) {
		t.Fatalf("a drop past the quota was answered %v", err)
	}
}

// --- the drop folder on a BUCKET instance (§20 erratum 296 (m)) --------------

// newBucketDropEnv is newDropEnv's sibling for a `s3` instance: a REAL watcher
// over a directory of its own, wired to this handler's upload path, with the
// clips going to the bucket-shaped Fake. It is the whole of what the erratum
// changed — the watcher itself never knew which backend it was on.
func newBucketDropEnv(t *testing.T, dir string, mutate ...func(*Deps)) (*env, *drop.Watcher, db.User) {
	t.Helper()
	var w *drop.Watcher
	var owner db.User
	e := newEnv(t, func(d *Deps) {
		u, err := d.DB.CreateUser(context.Background(), ids.New(), "dropowner", "$2a$12$notahash", d.Started.Unix())
		if err != nil {
			t.Fatal(err)
		}
		owner = u
		d.Cfg.StorageBackend = "s3"
		d.Cfg.StorageDir = dir
		d.Cfg.DropFolderOwner = "dropowner"
		for _, m := range mutate {
			m(d)
		}
		// The real watcher, with the real decisions table (migration 0018) and
		// no prober — `serve` injects ffprobe; nothing here needs one.
		w = drop.New(drop.Deps{
			Dir: dir, Owner: u.Username, OwnerID: u.ID, Seen: d.DB,
			Log: d.Log, Interval: 5 * time.Millisecond,
		})
		d.Drop = w
	})
	return e, w, owner
}

// runWatcher runs the watcher until `until` answers true, and stops it.
func runWatcher(t *testing.T, w *drop.Watcher, until func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !until() {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond) // sleep: the drop watcher reports no pass; its rows are what until() reads
	}
	cancel()
	<-done
}

// A video dropped into the folder of a BUCKET instance becomes a clip whose
// original is IN THE BUCKET, and the person's folder is clear. Nothing about
// the watcher changed to make this true: the staged file goes down the same
// ingest path, which writes through whichever store this instance opened.
func TestDropOnABucketInstanceIngestsIntoTheBucket(t *testing.T) {
	dir := t.TempDir()
	e, w, owner := newBucketDropEnv(t, dir)
	body := testVideo(2048)
	if err := os.WriteFile(filepath.Join(dir, "Team fight 3.mp4"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	var id string
	runWatcher(t, w, func() bool {
		rows, err := e.db.ListAllClipIDsAndKeys(context.Background())
		if err != nil || len(rows) == 0 {
			return false
		}
		id = rows[0].ID
		return true
	})
	if id == "" {
		t.Fatalf("the dropped file never became a clip; the folder holds %v", dirNames(t, dir))
	}
	clip := e.getClip(id)
	if clip.OwnerID != owner.ID {
		t.Fatalf("the clip is owned by %q, want the configured account", clip.OwnerID)
	}
	// The bytes are in the STORE, under the clip's own key, and the store here
	// is the bucket-shaped Fake rather than a directory.
	info, err := e.fake.Stat(context.Background(), clip.SourceKey)
	if err != nil {
		t.Fatalf("the original is not in the bucket under %q: %v", clip.SourceKey, err)
	}
	if info.Size != int64(len(body)) {
		t.Fatalf("the bucket holds %d bytes of %d", info.Size, len(body))
	}
	// And the folder is clear: the file left the root, and the staging
	// directory — which stays, because it is ours — holds nothing.
	for _, name := range dirNames(t, dir) {
		if name != drop.StageDir {
			t.Fatalf("the folder still holds %q after a successful ingest", name)
		}
	}
	if staged := dirNames(t, filepath.Join(dir, drop.StageDir)); len(staged) != 0 {
		t.Fatalf("the staged copy was not removed after a successful ingest: %v", staged)
	}
}

// A refusal on a bucket instance leaves the file exactly where it was, with the
// note beside it — the same promise the folder has always made, and the one
// worth re-proving on the backend that had never run it.
func TestDropOnABucketInstanceKeepsAFileItRefuses(t *testing.T) {
	dir := t.TempDir()
	_, w, _ := newBucketDropEnv(t, dir, func(d *Deps) { d.Cfg.MaxUploadBytes = 64 })
	body := testVideo(2048)
	if err := os.WriteFile(filepath.Join(dir, "big.mp4"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	note := filepath.Join(dir, "big.mp4"+drop.FailedSuffix)
	runWatcher(t, w, func() bool {
		_, err := os.Stat(note)
		return err == nil
	})
	if _, err := os.Stat(note); err != nil {
		t.Fatalf("a refused file got no note beside it; the folder holds %v", dirNames(t, dir))
	}
	if _, err := os.Stat(filepath.Join(dir, "big.mp4")); err != nil {
		t.Fatalf("the refused file did not stay where it was: %v", err)
	}
	b, err := os.ReadFile(note)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "MAX_UPLOAD_BYTES") {
		t.Fatalf("the note does not say why: %s", b)
	}
}

// dirNames is what is at the root of the folder right now, staging included.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
