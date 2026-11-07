// Package drop watches the ROOT of the media directory and turns a video left
// there into a clip (ROADMAP #42 (b), DESIGN.md §20 erratum 296).
//
// The shape of the feature in one sentence: **the folder is ours, so we may
// move things in it; what arrives in it is somebody's, so we take nothing we
// cannot ingest and we delete nothing we did not write.** `STORAGE_BACKEND=fs`
// already keeps its objects under `clips/` beside a `.clippet-store` marker, so
// the root of that directory was always the person's — this is what it is for.
//
// It is OFF unless `DROP_FOLDER_OWNER` names the one account that owns whatever
// arrives (DEPLOY.md §34). It runs on BOTH backends (erratum 296 (m)): nothing
// in this package knows or asks which one is in force, because the only thing
// it needs is `STORAGE_DIR` — a directory on this box. On `fs` that directory
// is the store and `clips/` is one of the sub-directories the rules below skip;
// on `s3` it holds nothing but the staging directory, the clips go to the
// bucket through the same ingest path, and the rename below is the same rename
// inside the same directory. It was `fs`-only for its first fortnight on the
// reading that "a bucket has no root anybody can drop a file into", which is
// true of the bucket and was never the question.
//
// # Why a poll and not fsnotify
//
// A poll, every few seconds, one level deep. fsnotify was considered and did
// not earn its place, for three reasons in descending order of weight:
//
//   - **The watch would not fire where this feature lives.** The folder is a
//     bind mount from a desktop (Docker Desktop's file sharing on Windows and
//     macOS) or a mounted NAS share. A host-side write does not generate an
//     inotify event inside the container on either — and an SMB/NFS server
//     tells a client nothing at all about what another machine wrote. A
//     watcher that silently never fires for the exact case it was built for is
//     worse than no watcher.
//   - **We need the timer anyway.** A file counts only once its size has been
//     STABLE across two polls; an event says "something changed", which is the
//     question rather than the answer, so the loop below would exist under the
//     events as well as beside them.
//   - **It is a dependency and an egress-free binary's dependency list is a
//     claim §15 makes.** One `os.ReadDir` of one directory every five seconds
//     costs nothing measurable beside a transcode.
//
// # What counts as a file to take
//
// In order, and every rejection is silent-but-for-one-DEBUG-line rather than an
// error, because the root of this folder is a place a person keeps things:
//
//   - a dotfile (the store's own marker, our staging directory, whatever the OS
//     dropped there) or a directory (`clips/` is one) — skipped, never logged;
//   - anything that is not a REGULAR file: a symlink, a socket, a device. §32's
//     rule is that nothing inside the store is followed through a symlink, and
//     this is that rule at the front door;
//   - a `.part`, `.crdownload` or `.tmp` suffix, or a `.failed.txt` or
//     `.clippet.txt` sibling we wrote ourselves;
//   - a size of zero, or a size or mtime that MOVED since the last tick — it is
//     still being written;
//   - a file this instance has already decided about (internal/db, migration
//     0018): ingested, or tried and failed;
//   - anything the transcoder's own probe does not accept as a video. A text
//     file is not a video, and neither is the front half of an MP4 whose moov
//     atom is at the END — which is where a recorder appending to a file leaves
//     it until the last moment. It is the last gate; it is NOT a completeness
//     check, and it was described as one until erratum 296 (k): a file written
//     FASTSTART carries its moov atom at the FRONT, so half of one probes
//     perfectly clean. What keeps half of a faststart file out is the
//     size-stability rule above — which an ABORTED copy defeats, because after
//     it aborts nothing moves again. DEPLOY §34.1 says so in the operator's
//     words, with the remedy: delete the clip and drop the file again.
//
// # What happens to a file that is taken
//
// It is RENAMED into a staging directory inside the store (`.clippet-drop/`),
// which is one rename on one filesystem: instant, atomic, and it takes the file
// out of the person's view the moment we commit to it, so nothing can pick it
// up twice and nothing can be half-read while a capture tool appends to it.
// From there it goes through the app's own upload path — the same quota, the
// same MAX_UPLOAD_BYTES, the same scratch precheck, the same per-account upload
// budget, the same ffprobe gate, the same defaults — and on success the staged
// file is removed, because the store now holds those bytes under the clip's own
// `original/` key, written by the store's atomic PutFile.
//
// That rename NEVER writes over a file already in the staging directory.
// os.Rename replaces its destination silently, and a name is only ever taken in
// there by an ORPHAN an earlier run was killed mid-ingest with — one
// recoverStaged deliberately refused to put back because the root name was
// occupied. Staging over it and then removing the result on a successful ingest
// would destroy the person's only copy of that video, which is the one thing
// this package promises never to do. The file at the root waits instead, and
// the orphan is named in a WARN (erratum 296 (f)).
//
// A refusal is one of two things, and the difference is the whole of what gets
// REMEMBERED. A refusal about the FILE — too large, empty, not a video, not
// readable, past the quota — is renamed back to exactly where it was, recorded
// in the decisions table, and a `<name>.failed.txt` names why in one sentence
// beside it. A refusal about this INSTANCE's moment — the hour's upload budget
// spent, no room on the working disk — is a NotNow: the file goes back, nothing
// is recorded and nothing is written beside it, and the next tick after a short
// backoff looks again, because dropping a capture folder in one go must not
// strand its own tail for thirty days (erratum 296 (l)).
//
// Nothing is deleted on any of those paths: the only file this package ever
// removes is one it moved or one it wrote.
package drop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clippet/internal/transcode"
)

const (
	// StageDir is the directory inside STORAGE_DIR a file is renamed into for
	// the length of its ingest. A dotted name for the same reason
	// `.clippet-store` has one: it belongs to the app and not to the person
	// whose folder this is, and the watcher's own dotfile rule then skips it
	// without a special case.
	StageDir = ".clippet-drop"

	// FailedSuffix is appended to the file's own name for the sibling that
	// says why an ingest did not happen. `clip.mp4` → `clip.mp4.failed.txt`:
	// the whole name, so the two sort together and neither hides the other.
	FailedSuffix = ".failed.txt"

	// NoteSuffix is the duplicate's sibling (§20 erratum 326, amended run 8):
	// `clip.mp4` → `clip.mp4.clippet.txt`, whose one line is `already here
	// <link>`. Nothing failed, so it does not wear FailedSuffix; the watcher
	// skips it exactly as it skips that one.
	NoteSuffix = ".clippet.txt"

	// DefaultInterval is "every few seconds". Short enough that dropping a
	// file and alt-tabbing to the library feels immediate (two ticks of
	// stability, so ~10 s before the encode starts), long enough that a NAS
	// share is asked for one directory listing twelve times a minute.
	DefaultInterval = 5 * time.Second

	// Retention bounds the decisions table. A file re-dropped a month after
	// the last one of the same name, size and mtime is a deliberate act and is
	// ingested again; what this window has to cover is a restart, which is
	// minutes (0018).
	Retention = 30 * 24 * time.Hour

	// probeTimeout bounds one ffprobe of a candidate. The upload path's own
	// gate uses the same budget; a probe that takes longer than this on a
	// share that is answering is not a video anybody is waiting for.
	probeTimeout = 30 * time.Second

	// retryBackoff is how long a file waits after a refusal that was about
	// this instance rather than about it (a NotNow, or an orphan of its own
	// name in the staging directory). Nothing is recorded for those, so
	// without a wait the watcher would re-probe and re-stage every one of them
	// on every tick — forty stranded files would be forty ffprobes every five
	// seconds for the hour it takes an upload budget to refill, and the file
	// would flicker out of the person's folder and back twelve times a minute.
	// A minute is long enough to cost nothing and short enough that a bulk
	// drop still drains by itself.
	retryBackoff = time.Minute
)

// NotNow wraps a refusal from the ingest path that is about this INSTANCE's
// momentary state rather than about the file: the account's upload budget is
// spent for this hour, the working disk has no room right now. It is the whole
// difference between a decision and a pause — see ingestOne, and erratum
// 296 (l).
//
// It lives here rather than in internal/httpapi because the watcher is the
// thing that has to tell the two apart, and §3's import graph has the ingest
// path importing this package rather than the other way round.
type NotNow struct{ Err error }

func (e *NotNow) Error() string { return e.Err.Error() }
func (e *NotNow) Unwrap() error { return e.Err }

// skipSuffixes are the "still being written" names, lowercased. Browsers and
// download managers write `.crdownload` and `.part`; almost everything else
// that streams to disk writes `.tmp`. A capture tool that writes straight to
// its final name is covered by the size-stability rule instead.
var skipSuffixes = []string{".part", ".crdownload", ".tmp"}

// Source is one dropped file on its way into the library: the bytes, and the
// one fact the filename carries. There is no declared content type — nobody
// declared one — so the ingest path derives it from the extension exactly as it
// does for a capture tool that sent none.
type Source struct {
	Body     io.Reader
	Filename string
	Size     int64
}

// Result is what the app made of it.
//
// Existing says the file was already a live clip of the folder's account —
// byte for byte — and ClipID is that clip (§20 erratum 326): nothing was
// created. Link is that clip's share link, for the note beside the file.
type Result struct {
	ClipID   string
	Existing bool
	Link     string
}

// Ingest is the app's own upload path, handed in by the caller — the same
// indirection internal/notify's bot uses and for the same reason: this package
// must not import internal/httpapi, and the ingest core must not grow a second
// copy anywhere (§3's import graph, §20 erratum 253).
type Ingest func(ctx context.Context, src Source) (Result, error)

// Seen is the decisions table (internal/db, migration 0018). An interface so a
// test can drive the watcher without a database, and so this package's
// dependency on internal/db is three method signatures rather than a schema.
type Seen interface {
	DropSeen(ctx context.Context, name string, size, mtime int64) (bool, error)
	MarkDropSeen(ctx context.Context, name string, size, mtime, at int64, clipID string) error
	PruneDropSeen(ctx context.Context, at int64) (int, error)
}

// Deps is everything the watcher needs. Dir, Owner and OwnerID come from
// `clippet serve`, which has already refused to boot if the account does not
// exist.
type Deps struct {
	Dir      string // STORAGE_DIR: the media directory, whose ROOT is watched
	Owner    string // DROP_FOLDER_OWNER, for the log lines and the doctor
	OwnerID  string // the account's id, resolved at boot
	Seen     Seen
	Probe    func(ctx context.Context, path string) (*transcode.ProbeResult, error)
	Log      *slog.Logger
	Now      func() time.Time
	Interval time.Duration // 0 → DefaultInterval
}

// Watcher is the poll loop. One per process, started by `clippet serve` after
// the worker is up.
type Watcher struct {
	d Deps
	// ingest is set once, at construction time, by internal/httpapi's
	// NewHandler — the handler is the only thing that can reach the ingest
	// core, and the watcher is the only thing that drives it on its own
	// schedule. Run reads it once and refuses to start without it.
	ingest Ingest
	// prev is the previous tick's (size, mtime) per name: the whole of the
	// stability rule. In memory on purpose — a restart simply waits one more
	// tick before taking anything, which is the safe direction.
	prev map[string]stat
	// said remembers which (name,size,mtime) we have already explained, so a
	// folder with a README in it does not write one DEBUG line every five
	// seconds for ever.
	said map[stat]bool
	// retryAfter is when a file we declined for a reason of OUR own — the
	// hour's upload budget, a full working disk, an orphan of the same name
	// left staged by an earlier run — may be looked at again. Nothing goes in
	// the decisions table for those, because none of them is the file's fault;
	// this is what stops "look again" meaning "every five seconds". Bounded by
	// the folder: an entry is dropped as soon as its name leaves it.
	retryAfter map[string]time.Time
}

type stat struct {
	name  string
	size  int64
	mtime int64
}

// New builds the watcher. It touches no disk and starts nothing.
func New(d Deps) *Watcher {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Interval <= 0 {
		d.Interval = DefaultInterval
	}
	return &Watcher{d: d, prev: map[string]stat{}, said: map[stat]bool{}, retryAfter: map[string]time.Time{}}
}

// SetIngest wires the app's upload path in. It is called once, during handler
// construction, before Run.
func (w *Watcher) SetIngest(in Ingest) { w.ingest = in }

// Owner is the account name the folder ingests as. The doctor and the settings
// panel read configuration rather than this, so nothing but a log line uses it.
func (w *Watcher) Owner() string { return w.d.Owner }

// OwnerID is that account's id, resolved by `clippet serve` at boot. It is what
// internal/httpapi binds the upload path to.
func (w *Watcher) OwnerID() string { return w.d.OwnerID }

// Run polls until ctx is done. It never returns an error: every failure it can
// meet is one file's, and a folder that cannot be listed at all is a WARN and a
// retry — the same posture the sweeper takes, because the rest of the instance
// works perfectly without this.
func (w *Watcher) Run(ctx context.Context) {
	if w.ingest == nil {
		// Unreachable in `serve`, which wires it before it starts this.
		w.d.Log.Error("drop folder: no ingest path is wired; the watcher will not run")
		return
	}
	// The folder is ours — the package doc's first sentence — so we make it if
	// it is not there. On `fs` the store has already created it and this is a
	// no-op; on `s3` (erratum 296 (m)) nothing else creates it, and a watcher
	// that WARNed twelve times a minute about a directory it was allowed to
	// make would be theatre. A failure is not fatal: the tick below reports a
	// folder it cannot list, and `admin doctor`'s `drop` row reports it before
	// the restart.
	if err := os.MkdirAll(w.d.Dir, 0o755); err != nil {
		w.d.Log.Warn("drop folder: the media directory could not be created", "dir", w.d.Dir, "err", err)
	}
	w.d.Log.Info("drop folder watching", "dir", w.d.Dir, "owner", w.d.Owner, "interval", w.d.Interval)
	w.recoverStaged()
	t := time.NewTicker(w.d.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// recoverStaged puts back anything a previous process left mid-ingest. It runs
// once, at boot, before the first tick.
//
// A file is in the staging directory only between its rename out of the root
// and the removal that follows a successful ingest, which is a window of one
// upload. A process killed inside it would otherwise leave the person's file
// somewhere they cannot see, for ever — so it goes back under its own name, and
// the decisions table (0018) is what stops the one that had ALREADY become a
// clip from becoming a second one.
func (w *Watcher) recoverStaged() {
	dir := filepath.Join(w.d.Dir, StageDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			w.d.Log.Warn("drop folder: the staging directory could not be read", "dir", dir, "err", err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		staged := filepath.Join(dir, e.Name())
		back := filepath.Join(w.d.Dir, e.Name())
		if _, err := os.Lstat(back); err == nil {
			// Something now owns that name at the root. Leave the staged copy
			// alone rather than overwrite either of them, and say so once.
			w.d.Log.Warn("drop folder: a file left staged by an earlier run cannot go back (a file of that name is at the root now); it is left in the staging directory",
				"file", e.Name(), "staged", staged)
			continue
		}
		if err := os.Rename(staged, back); err != nil {
			w.d.Log.Warn("drop folder: a file left staged by an earlier run could not be put back",
				"file", e.Name(), "err", err)
			continue
		}
		w.d.Log.Info("drop folder: a file left staged by an earlier run was put back at the root", "file", e.Name())
	}
}

// tick is one poll: list, decide, and ingest at most the files that are ready.
func (w *Watcher) tick(ctx context.Context) {
	entries, err := os.ReadDir(w.d.Dir)
	if err != nil {
		w.d.Log.Warn("drop folder: the media directory could not be listed", "dir", w.d.Dir, "err", err)
		return
	}
	now := map[string]stat{}
	// Every name this tick SAW, eligible or not. The housekeeping below is
	// keyed on it rather than on `now`, because a name that never becomes a
	// candidate — a symlink, a fifo — never reaches `now` at all, and forgetting
	// its explanation while it is still sitting in the folder is how "one DEBUG
	// line per file" became one every five seconds for ever (erratum 296 (d)).
	seen := map[string]bool{}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		name := e.Name()
		seen[name] = true
		if !w.eligible(e) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // it went away between the listing and the stat: not ours to worry about
		}
		s := stat{name: name, size: info.Size(), mtime: info.ModTime().Unix()}
		now[name] = s
		if s.size == 0 {
			continue
		}
		// The stability rule: the same size and the same mtime as the previous
		// tick. A first sighting is never taken, which is what makes a file
		// being written at any speed safe — it is only ever taken once nothing
		// has moved for a whole interval.
		if prev, ok := w.prev[name]; !ok || prev != s {
			continue
		}
		w.consider(ctx, s)
	}
	w.prev = now
	// Forget the explanations, and the waits, for files that are no longer
	// there — so both maps are bounded by the folder rather than by the run's
	// length.
	for s := range w.said {
		if !seen[s.name] {
			delete(w.said, s)
		}
	}
	for name := range w.retryAfter {
		if !seen[name] {
			delete(w.retryAfter, name)
		}
	}
}

// NotRegular is Eligible's reason for a symlink, a socket, a fifo or a device.
// It is a constant because the watcher logs it verbatim and a test counts the
// line: the wording is part of this package's contract.
const NotRegular = "is not a regular file"

// Eligible is the name-and-kind half of the rules, which needs no stat: is this
// entry, by its name and its type alone, a video file somebody put in a folder
// for this app to take?
//
// It is EXPORTED because `clippet admin import` walks a folder of old clips
// through the same upload path and must accept exactly what this folder accepts
// (§20 erratum 307). A second list would be two answers to "what is a file we
// take", and the day they disagreed nobody would find out from a test.
//
// `why` is the sentence for a rejection an operator should hear about, and is
// EMPTY for the ones nobody wants said out loud: a dotfile (the store's marker,
// the staging directory, whatever the OS dropped there), a directory (`clips/`
// is one), and this app's own `.failed.txt` and `.clippet.txt` notes. Those are not the person's
// videos and a line about each of them is noise in both readers — one DEBUG
// line every five seconds here, one `refused` line per file there.
func Eligible(e os.DirEntry) (ok bool, why string) {
	name := e.Name()
	if strings.HasPrefix(name, ".") || e.IsDir() {
		return false, ""
	}
	if !e.Type().IsRegular() {
		// A symlink, a socket, a fifo, a device. §32's symlink rule at the
		// front door: nothing in this store is followed through one.
		return false, NotRegular
	}
	if IsNote(name) {
		return false, ""
	}
	lower := strings.ToLower(name)
	for _, suf := range skipSuffixes {
		if strings.HasSuffix(lower, suf) {
			return false, "is still being written (its name ends in " + suf + ")"
		}
	}
	return true, ""
}

// IsNote reports whether name is one of this app's own siblings: a
// `.failed.txt` or a `.clippet.txt`, in any case.
func IsNote(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, FailedSuffix) || strings.HasSuffix(lower, NoteSuffix)
}

// eligible is Eligible with this watcher's one log line: the untouchable file
// is the only rejection eligibility itself explains, and it explains it once
// per (name, size, mtime) (erratum 296 (d)).
func (w *Watcher) eligible(e os.DirEntry) bool {
	ok, why := Eligible(e)
	if !ok && why == NotRegular {
		w.sayOnce(stat{name: e.Name()}, why)
	}
	return ok
}

// consider runs the gates that cost something — the database, an open, a probe
// — and then ingests.
func (w *Watcher) consider(ctx context.Context, s stat) {
	// A file this instance declined for a reason of its own is waiting, not
	// decided: it is asked again, but not twelve times a minute.
	if until, ok := w.retryAfter[s.name]; ok && w.d.Now().Before(until) {
		return
	}
	seen, err := w.d.Seen.DropSeen(ctx, s.name, s.size, s.mtime)
	if err != nil {
		w.d.Log.Warn("drop folder: the decisions table could not be read", "file", s.name, "err", err)
		return
	}
	if seen {
		w.sayOnce(s, "has already been ingested or refused once; `touch` it to have it looked at again")
		return
	}
	path := filepath.Join(w.d.Dir, s.name)
	// READABILITY first, and it is the one rejection that writes a sibling: a
	// file whose permissions this process cannot get past is not a mistake
	// about what the folder is for, it is one thing wrong that the person can
	// fix — and silence would leave them watching a folder that never empties.
	f, err := os.Open(path)
	if err != nil {
		w.refuse(ctx, s, fmt.Sprintf("it could not be opened for reading: %s", Cause(err)))
		return
	}
	_ = f.Close()
	// The transcoder's own probe, on the file where it lies: what keeps a text
	// file, a screenshot or a spreadsheet at the root untouched, and what
	// catches the front half of an MP4 whose moov atom has not been written
	// yet. It is NOT a completeness check — see the package doc and erratum
	// 296 (k) — so "finished writing" still rests on the stability rule above.
	if err := w.probe(ctx, path); err != nil {
		// The one sentence the drop folder and `admin import` both say (§20
		// erratum 328 (f)); the file is left where it lies.
		w.sayOnce(s, "this instance cannot read a video or audio stream out of it, so it is left alone: "+Cause(err))
		return
	}
	w.ingestOne(ctx, s)
}

// ProbeVideo runs `probe` under this package's own budget and reports the one
// verdict it needs: can the pipeline read a video stream out of this file.
//
// Exported for `clippet admin import`, which applies the same gate to the same
// kind of file before it reads one (§20 erratum 307). A nil `probe` is a test
// without ffmpeg; `serve` and the CLI both inject one.
func ProbeVideo(ctx context.Context, probe func(context.Context, string) (*transcode.ProbeResult, error), path string) error {
	if probe == nil {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	res, err := probe(pctx, path)
	if err != nil {
		return err
	}
	if res == nil || (res.Kind != transcode.KindAudio && res.VideoIndex < 0) {
		// A sound is something the pipeline reads too (§20 erratum 328): an
		// audio result has no video index by construction and is not refused.
		return errors.New("no video stream")
	}
	return nil
}

// probe is ProbeVideo with this watcher's injected prober.
func (w *Watcher) probe(ctx context.Context, path string) error {
	return ProbeVideo(ctx, w.d.Probe, path)
}

// ingestOne stages the file, runs it through the app's upload path, and
// records the decision either way.
func (w *Watcher) ingestOne(ctx context.Context, s stat) {
	path := filepath.Join(w.d.Dir, s.name)
	stageDir := filepath.Join(w.d.Dir, StageDir)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		w.d.Log.Warn("drop folder: the staging directory could not be created", "dir", stageDir, "err", err)
		return
	}
	staged := filepath.Join(stageDir, s.name)
	// That name is already taken in there, which can only mean one thing: an
	// earlier run was killed mid-ingest with a file of this name, and the boot
	// pass refused to put it back because THIS file was already at the root.
	// os.Rename would replace it without a word and the ingest below would then
	// remove what it wrote over — somebody's only copy of a video, gone, with
	// one WARN from a previous boot to show for it. So this file waits, and the
	// orphan gets named where an operator will see it.
	if _, err := os.Lstat(staged); err == nil {
		if w.once(s) {
			w.d.Log.Warn("drop folder: a file of that name is already staged from an earlier run, so this one is left alone; move the staged file out of the way (or delete it, if its clip was made) and this one is taken on the next look",
				"file", s.name, "staged", staged)
		}
		w.retryAfter[s.name] = w.d.Now().Add(retryBackoff)
		return
	}
	// The rename is the commit point: from here the file is ours, it is out of
	// the person's view, and nothing else can pick it up. One rename inside one
	// directory tree — never a copy, and never a path that leaves STORAGE_DIR.
	if err := os.Rename(path, staged); err != nil {
		w.d.Log.Warn("drop folder: the file could not be moved out of the way", "file", s.name, "err", err)
		return
	}
	f, err := os.Open(staged)
	if err != nil {
		w.putBack(staged, path, s.name)
		w.refuse(ctx, s, fmt.Sprintf("it could not be opened for reading: %s", Cause(err)))
		return
	}
	res, err := w.ingest(ctx, Source{Body: f, Filename: s.name, Size: s.size})
	_ = f.Close()
	if err != nil {
		w.putBack(staged, path, s.name)
		var notNow *NotNow
		if errors.As(err, &notNow) {
			// Nothing about the FILE was wrong: this instance is out of upload
			// budget for the hour, or out of room on its working disk. A row in
			// the decisions table would strand it for thirty days — drop a
			// capture folder of a hundred clips with the default 60/hour and
			// forty of them would need a human to `touch` each one — and a
			// `.failed.txt` would blame the file for the instance's minute.
			w.sayOnce(s, "is not being taken just now, and will be looked at again: "+Cause(err))
			w.retryAfter[s.name] = w.d.Now().Add(retryBackoff)
			return
		}
		w.refuse(ctx, s, Cause(err))
		return
	}
	delete(w.retryAfter, s.name)
	if res.Existing {
		w.alreadyHere(ctx, s, staged, path, res)
		return
	}
	// The row BEFORE the removal, and that order is the whole reason the table
	// exists: a crash between them leaves a staged file the next boot puts back
	// at the root, and only a recorded decision stops it becoming a second clip
	// of the same video.
	w.mark(ctx, s, res.ClipID)
	if err := os.Remove(staged); err != nil {
		w.d.Log.Warn("drop folder: the staged file could not be removed after a successful ingest",
			"file", s.name, "clip", res.ClipID, "staged", staged, "err", err)
	}
	// An earlier attempt's explanation, or an earlier duplicate's note, is no
	// longer true.
	_ = os.Remove(path + FailedSuffix)
	_ = os.Remove(path + NoteSuffix)
	w.d.Log.Info("drop folder: ingested", "file", s.name, "clip", res.ClipID, "owner", w.d.Owner, "bytes", s.size)
}

// alreadyHere is the duplicate's answer (§20 erratum 326): the file is already
// a clip of this account, byte for byte, so nothing was created. The file goes
// back exactly where the person left it — it is theirs, and this package never
// removes a file that did not become a clip — the decision is recorded with the
// existing clip's id (0018's memory, unchanged: `touch` it to be asked again),
// and the note beside it says where the clip is. The note is its own sibling,
// NoteSuffix (amended run 8: until then it wore FailedSuffix, so its name said
// failed while its text said already here); the watcher skips it as it skips
// that one, and its whole text is one line. An earlier refusal's note is no
// longer true and goes.
func (w *Watcher) alreadyHere(ctx context.Context, s stat, staged, path string, res Result) {
	w.putBack(staged, path, s.name)
	w.mark(ctx, s, res.ClipID)
	_ = os.Remove(path + FailedSuffix)
	note := path + NoteSuffix
	if err := os.WriteFile(note, []byte("already here "+res.Link+"\n"), 0o644); err != nil {
		w.d.Log.Warn("drop folder: the already-here note could not be written", "file", s.name, "err", err)
	}
	w.d.Log.Info("drop folder: already here", "file", s.name, "clip", res.ClipID, "owner", w.d.Owner)
}

// putBack returns a staged file to exactly where the person left it.
func (w *Watcher) putBack(staged, path, name string) {
	if err := os.Rename(staged, path); err != nil {
		w.d.Log.Warn("drop folder: the file could not be put back after a failed ingest; it is in the staging directory",
			"file", name, "staged", staged, "err", err)
	}
}

// refuse records the decision and writes the sibling that says why. The file
// itself is already back where it was.
func (w *Watcher) refuse(ctx context.Context, s stat, why string) {
	w.mark(ctx, s, "")
	delete(w.retryAfter, s.name)
	// An earlier duplicate's `already here` is no longer true (run 8).
	_ = os.Remove(filepath.Join(w.d.Dir, s.name+NoteSuffix))
	path := filepath.Join(w.d.Dir, s.name+FailedSuffix)
	body := fmt.Sprintf("clippet could not make a clip out of %q:\n\n  %s\n\n"+
		"The file has been left exactly where it was. Fix the cause and save or `touch` it\n"+
		"again and this instance will look at it once more; delete this note whenever you like.\n",
		s.name, why)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		w.d.Log.Warn("drop folder: the .failed.txt note could not be written", "file", s.name, "err", err)
	}
	w.d.Log.Warn("drop folder: refused", "file", s.name, "owner", w.d.Owner, "reason", why)
}

// mark records one decision and prunes anything past the retention window. A
// prune failure is not worth a line: the table is bounded by the folder's own
// traffic and the next decision tries again.
func (w *Watcher) mark(ctx context.Context, s stat, clipID string) {
	now := w.d.Now()
	if err := w.d.Seen.MarkDropSeen(ctx, s.name, s.size, s.mtime, now.Unix(), clipID); err != nil {
		w.d.Log.Warn("drop folder: the decision could not be recorded; this file may be looked at again after a restart",
			"file", s.name, "err", err)
		return
	}
	if _, err := w.d.Seen.PruneDropSeen(ctx, now.Add(-Retention).Unix()); err != nil {
		w.d.Log.Debug("drop folder: old decisions could not be pruned", "err", err)
	}
}

// once reports whether (name, size, mtime) has not been explained yet, and
// remembers that it now has been. A file that CHANGES is a different key and is
// explained again; the tick's housekeeping forgets a key as soon as its name
// leaves the folder.
func (w *Watcher) once(s stat) bool {
	if w.said[s] {
		return false
	}
	w.said[s] = true
	return true
}

// sayOnce logs one DEBUG line per (name, size, mtime) — so a folder somebody
// keeps other things in explains itself exactly once per file rather than
// twelve times a minute.
func (w *Watcher) sayOnce(s stat, what string) {
	if w.once(s) {
		w.d.Log.Debug("drop folder: "+what, "file", s.name)
	}
}

// Cause is one sentence from an error, with nothing structural in it. Errors
// from the ingest path are already worded for a person (internal/httpapi's drop
// sentences); everything else here is an os error, whose text is a path and a
// reason the person owns. Exported for `clippet admin import`, which prints the
// same kind of sentence on the same kind of failure (§20 erratum 307).
func Cause(err error) string {
	if err == nil {
		return "unknown"
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg)
}
