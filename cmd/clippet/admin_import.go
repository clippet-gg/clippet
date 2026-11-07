package main

// admin_import.go — `clippet admin import <dir>` (ROADMAP #51, DESIGN.md §20
// erratum 307).
//
//	clippet admin import <dir> --owner <username> [--recursive] [--dry-run]
//
// The one-time walk that moves somebody off Streamable, or off a hard disk full
// of clips nobody has watched since 2019: every video in a folder goes through
// the app's own upload path and becomes an ordinary clip owned by one account.
// It is the drop folder's (§34) sibling — the drop folder watches for what
// arrives NEXT, this reads what is there ALREADY — and it deliberately shares
// the drop folder's own predicate and probe rather than growing a second idea
// of what a video file is (`drop.Eligible`, `drop.ProbeVideo`).
//
// Three rules, and the reasons are the project's own:
//
//   - **THE ORIGINALS ARE NEVER TOUCHED.** Not moved, not renamed, not deleted,
//     not on success, not on a re-run — the folder is the PERSON's, and unlike
//     the drop folder this app was never given it. The rule that a destructive
//     action needs its exact target printed and an explicit yes is satisfied here the way
//     `storage migrate` satisfies it: by there being no destructive action at
//     all. `.failed.txt` notes are the drop folder's; an import writes nothing
//     into the folder it reads.
//   - **IT IS IDEMPOTENT BY CONTENT.** A second run over the same folder
//     imports nothing and says so, file by file. The memory is
//     `clips.source_etag` — the sha256 of the original the store computed while
//     writing it — and not a table of its own, so there is one fact in one
//     place and deleting a clip really does mean its content is no longer here.
//     A file is hashed only when this account already owns a clip of exactly
//     its size (migration 0020), so the common first run reads each file once
//     and the common second run reads it once and writes nothing.
//   - **THE PLAN IS AVAILABLE BEFORE ANY BYTE MOVES.** `--dry-run` prints the
//     verdict it would reach for every file — including `seen` — and stops.
//
// It runs in a container of its own beside the live instance and needs no
// listener: the clip is created `queued` exactly as an upload is, and the
// serving instance's worker claims it on its next poll.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"clippet/internal/drop"
	"clippet/internal/httpapi"
	"clippet/internal/transcode"
	"clippet/internal/urls"
)

func (a *admin) importUsage() {
	fmt.Fprintln(a.env.stderr, "usage: clippet admin import <dir> --owner <username> [--recursive] [--dry-run]")
	fmt.Fprintln(a.env.stderr, "  Walks a folder of existing videos through this instance's own upload path, once.")
	fmt.Fprintln(a.env.stderr, "  The files are READ and nothing else: nothing is moved, renamed or deleted, ever.")
	fmt.Fprintln(a.env.stderr, "  Running it twice imports nothing the second time (DEPLOY.md §34.4).")
}

// importCounts is what the summary line is made of.
type importCounts struct {
	imported, seen, refused, instanceRefused int
	bytes                                    int64
}

func (a *admin) importDir(args []string) int {
	fset := a.flags("import")
	owner := fset.String("owner", "", "the account every imported clip belongs to (required)")
	recursive := fset.Bool("recursive", false, "walk sub-directories too")
	dry := fset.Bool("dry-run", false, "print what each file would do and import nothing")
	dir, err := parseWithArg(fset, args)
	if err != nil || dir == "" || strings.TrimSpace(*owner) == "" {
		a.importUsage()
		return 2
	}
	u, ok := a.resolveUser(strings.TrimSpace(*owner))
	if !ok {
		return 1
	}
	root, code := a.importRoot(dir)
	if code != 0 {
		return code
	}

	st, err := a.store()
	if err != nil {
		return a.fail("import", err)
	}
	// The probe is built ONCE: it is a closure over FFPROBE_PATH and every
	// candidate below is asked with the same one.
	probe := a.env.probe
	if probe == nil {
		probe = transcode.Probe(a.cfg.Transcode.FFprobe)
	}
	strip := a.env.strip
	if strip == nil {
		strip = transcode.StripVerified(a.cfg.Transcode.FFmpeg, a.cfg.Transcode.FFprobe, transcode.ResolveThreads(a.cfg.Transcode.Threads))
	}
	im := httpapi.NewImporter(httpapi.Deps{
		Cfg: withAdminTimeout(a.cfg), DB: a.db, Store: st,
		URLs: urls.New(a.cfg.PublicBaseURL, version),
		// WARN and above, on stderr, so the operator's own lines on stdout are
		// the report and a problem underneath one of them is still visible.
		Log:       slog.New(slog.NewTextHandler(a.env.stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Probe:     probe,
		Strip:     strip,
		FreeBytes: freeBytes,
	})

	w := a.env.stdout
	fmt.Fprintf(w, "import: %s -> %s (%s)\n", root, u.Username, orRecursive(*recursive))
	fmt.Fprintf(w, "import: the files are READ; nothing in that folder is moved, renamed or deleted\n")
	if *dry {
		fmt.Fprintf(w, "import: --dry-run: this is the plan and nothing is imported\n")
	}

	files, err := importWalk(root, *recursive)
	if err != nil {
		return a.fail("import", err)
	}
	// NOT `a.ctx`. The whole-invocation budget is ten minutes (adminTimeout),
	// which is the right ceiling for a verb that ASKS something and the wrong one
	// for a verb that MOVES a folder: a thousand old clips is an hour of spooling
	// and hashing, and a context that expired half way through would turn every
	// remaining file into an `instance` refusal with nothing wrong anywhere. This
	// verb is idempotent and interruptible instead — Ctrl-C it and run it again,
	// and what got through is `seen` — and every call underneath still carries the
	// store's own per-operation timeout (STORAGE_OP_TIMEOUT, floored for the CLI).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c importCounts
	for _, f := range files {
		a.importOne(ctx, im, probe, u.ID, f, *dry, &c)
	}
	fmt.Fprintf(w, "import: %d imported, %d seen, %d refused, %d bytes (%s)\n",
		c.imported, c.seen, c.refused, c.bytes, humanBytes(c.bytes))
	if *dry {
		fmt.Fprintf(w, "import: --dry-run: nothing was imported\n")
	}
	if c.instanceRefused > 0 {
		// The drop folder's own rule (erratum 296 (l)), pointed at an exit code
		// instead of at a text file: if fixing something here and running this
		// again could change the answer, it is THIS INSTANCE's refusal and the
		// operator has to know without reading every line. A file that is not a
		// video would be refused the same way tomorrow and is not an error.
		fmt.Fprintf(w, "import: %d file(s) were refused for a reason that is this instance's (space, the quota, the store, the database): fix the cause and run this again — what is already imported will be `seen`\n",
			c.instanceRefused)
		return 1
	}
	return 0
}

// importOne decides one file and prints its line.
func (a *admin) importOne(ctx context.Context, im *httpapi.Importer, probe func(context.Context, string) (*transcode.ProbeResult, error), ownerID string, f importFile, dry bool, c *importCounts) {
	w := a.env.stdout
	refuse := func(tag, why string, instance bool) {
		c.refused++
		if instance {
			c.instanceRefused++
		}
		fmt.Fprintf(w, "refused %s %s: %s\n", tag, f.rel, why)
	}
	if f.why != "" {
		// A name the drop folder would not take either, and one worth saying out
		// loud because the operator POINTED at this folder: something that is not
		// a regular file (a symlink, a socket — §32's rule at the front door), or
		// a half-written download. The silent ones never reach here.
		tag := "still-being-written"
		if f.why == drop.NotRegular {
			tag = "not-a-file"
		}
		refuse(tag, f.why, false)
		return
	}
	if f.size == 0 {
		refuse("empty", "it is empty", false)
		return
	}
	if max := a.cfg.MaxUploadBytes; max > 0 && f.size > max {
		// The declared-size gate the HTTP upload applies to Content-Length
		// before it reads a byte (§8.3). A file this instance would refuse at
		// the end is refused here without being read at all.
		refuse("too-large", fmt.Sprintf("it is %s and this instance accepts %s (MAX_UPLOAD_BYTES)",
			humanBytes(f.size), humanBytes(max)), false)
		return
	}
	// Already here? Asked by SIZE first, so a file whose size matches nothing
	// this account owns is never read twice.
	digests, err := im.Duplicates(ctx, ownerID, f.size)
	if err != nil {
		refuse("database", "this instance could not be asked whether it already has it", true)
		return
	}
	if len(digests) > 0 {
		sum, err := hashFile(f.path)
		if err != nil {
			refuse("unreadable", "it could not be read: "+drop.Cause(err), false)
			return
		}
		for _, d := range digests {
			if d.ETag == sum {
				c.seen++
				fmt.Fprintf(w, "seen %s %s\n", d.ID, f.rel)
				return
			}
		}
	}
	// The transcoder's own probe, on the file where it lies: what keeps a
	// screenshot, a text file or a spreadsheet out, and the same gate the drop
	// folder applies before it commits to anything.
	if err := drop.ProbeVideo(ctx, probe, f.path); err != nil {
		refuse("not-a-video", "this instance cannot read a video stream out of it: "+drop.Cause(err), false)
		return
	}
	if dry {
		fmt.Fprintf(w, "import %s\n", f.rel)
		return
	}
	src, err := os.Open(f.path)
	if err != nil {
		refuse("unreadable", "it could not be opened for reading: "+drop.Cause(err), false)
		return
	}
	res, err := im.Import(ctx, ownerID, httpapi.ImportSource{Body: src, Filename: f.name, Size: f.size})
	_ = src.Close()
	if err != nil {
		var ref *httpapi.ImportRefusal
		if errors.As(err, &ref) {
			refuse(ref.Tag, ref.Why, ref.Instance)
			return
		}
		refuse("instance", "this instance could not make a clip out of it", true)
		return
	}
	if res.Existing {
		// The ingest core's duplicate answer (§20 erratum 326): a clip of these
		// bytes appeared after the pre-check above. The verb's word for it.
		c.seen++
		fmt.Fprintf(w, "seen %s %s\n", res.ClipID, f.rel)
		return
	}
	c.imported++
	c.bytes += res.Bytes
	fmt.Fprintf(w, "imported %s %s\n", res.ClipID, f.rel)
}

// importFile is one candidate: where it is, what to print for it, and the
// name/kind verdict the drop folder's own predicate reached.
type importFile struct {
	path string // absolute
	rel  string // what the operator sees: relative to the root they named
	name string // the leaf, which is what the clip is titled after
	size int64
	why  string // "" when it is a file to take; otherwise why it is not
}

// importWalk lists the candidates, in the order an operator reads a folder.
// Directories and dotfiles are skipped in silence, exactly as the drop folder
// skips them: `.DS_Store` and a sub-folder are not refusals, and a line about
// each of them would bury the ones that matter.
func importWalk(root string, recursive bool) ([]importFile, error) {
	var out []importFile
	add := func(dir string, e os.DirEntry) {
		ok, why := drop.Eligible(e)
		if !ok && why == "" {
			return
		}
		p := filepath.Join(dir, e.Name())
		rel, err := filepath.Rel(root, p)
		if err != nil {
			rel = e.Name()
		}
		f := importFile{path: p, rel: filepath.ToSlash(rel), name: e.Name(), why: why}
		if ok {
			info, err := e.Info()
			if err != nil {
				return // it went away between the listing and the stat
			}
			f.size = info.Size()
		}
		out = append(out, f)
	}
	if !recursive {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			add(root, e)
		}
		return out, nil
	}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if p != root && strings.HasPrefix(e.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		add(filepath.Dir(p), e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// importRoot resolves the directory and refuses the three this app owns.
//
// A folder of old clips is somebody's own; DATA_DIR, SCRATCH_DIR and STORAGE_DIR
// are this instance's, and pointing an import at one of them would walk the
// store's own objects back through the upload path — every rendition of every
// clip re-imported as a new clip, with `--recursive` doing it to the whole
// library. The rule is the one `config.storageDir` already applies to those
// same three directories, which is a refusal an operator has met before.
func (a *admin) importRoot(dir string) (string, int) {
	root, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(a.env.stderr, "admin import: %s: %v\n", dir, err)
		return "", 2
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		fmt.Fprintf(a.env.stderr, "admin import: %s cannot be read: %v\n", root, err)
		return "", 2
	}
	if !info.IsDir() {
		fmt.Fprintf(a.env.stderr, "admin import: %s is not a directory (import takes a FOLDER; one file is an upload)\n", root)
		return "", 2
	}
	for _, own := range []struct{ name, dir string }{
		{"STORAGE_DIR", a.cfg.StorageDir}, {"DATA_DIR", a.cfg.DataDir}, {"SCRATCH_DIR", a.cfg.ScratchDir},
	} {
		if own.dir == "" {
			continue
		}
		od := filepath.Clean(own.dir)
		if root == od || importWithin(root, od) || importWithin(od, root) {
			fmt.Fprintf(a.env.stderr, "admin import: %s is %s=%s, or holds it, or sits inside it: those are this instance's own directories and what is in them is not a folder of old clips. Mount the folder you want to import somewhere else (DEPLOY.md §34.4)\n",
				root, own.name, od)
			return "", 2
		}
	}
	return root, 0
}

// importWithin reports whether child sits under parent. The separator is
// appended so `/datax` is not inside `/data`.
func importWithin(child, parent string) bool {
	return strings.HasPrefix(child, strings.TrimRight(parent, string(filepath.Separator))+string(filepath.Separator))
}

// hashFile is the sha256 of a file's bytes, streamed. It is read only when this
// account already owns a clip of exactly this size, which is what keeps a first
// run over a large folder to one read per file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseWithArg is parseWithID's twin for a verb whose positional is a PATH
// rather than a clip id: at most one, before, between or after the flags.
func parseWithArg(fs *flag.FlagSet, args []string) (string, error) {
	arg := ""
	for {
		if err := fs.Parse(args); err != nil {
			return "", err
		}
		if fs.NArg() == 0 {
			return arg, nil
		}
		if arg != "" {
			return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		arg, args = fs.Arg(0), fs.Args()[1:]
	}
}

func orRecursive(recursive bool) string {
	if recursive {
		return "sub-directories included"
	}
	return "one level, no recursion"
}
