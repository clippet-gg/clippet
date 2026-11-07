package main

// admin_import_test.go — `clippet admin import <dir>` (ROADMAP #51, DESIGN.md
// §20 erratum 307).
//
// The contract these pin, in the order the verb applies it: the three verdicts
// it prints, that the folder it reads is never written to, that a second run
// imports nothing, that `--dry-run` creates nothing at all, that a refusal
// leaves no row and no object behind, that the instance's own refusals surface
// with their own word and a non-zero exit, and that the owner must exist.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/transcode"
)

// newImportFixture is an `fs` instance with an account, plus a folder of old
// clips OUTSIDE every directory this instance owns — which is what a
// `-v /path/to/old/clips:/import` bind mount is.
//
// The probe is injected rather than stubbed on disk: a file whose name contains
// "junk" is not a video and everything else is, which is the only thing these
// tests need ffprobe to decide.
func newImportFixture(t *testing.T, extra map[string]string) (*fsFixture, string) {
	t.Helper()
	fx := newFSFixture(t)
	// The scratch floor is 512 MiB by default and this fixture's scratch is a
	// temp directory on whatever disk the test runs on: a precheck about the
	// operator's headroom is not what these assert.
	fx.env["SCRATCH_MIN_FREE_BYTES"] = "0"
	for k, v := range extra {
		fx.env[k] = v
	}
	cfg, err := config.Load(fx.getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fx.cfg = cfg
	fx.probe = func(_ context.Context, path string) (*transcode.ProbeResult, error) {
		if strings.Contains(strings.ToLower(filepath.Base(path)), "junk") {
			return nil, &transcode.ErrPermanent{Err: errNotAVideo}
		}
		return &transcode.ProbeResult{VideoIndex: 0}, nil
	}
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	seedDropOwner(t, fx, "carol")
	return fx, t.TempDir()
}

// errNotAVideo is what the injected probe answers for a file that is not one.
var errNotAVideo = &probeRefusal{}

type probeRefusal struct{}

func (*probeRefusal) Error() string { return "moov atom not found" }

// plant writes a file into the folder to import.
func plant(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// clipsOf is what the account owns right now, by id.
func clipsOf(t *testing.T, fx *fsFixture) []string {
	t.Helper()
	d, err := db.Open(dbPath(fx.cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.ListAllClipIDsAndKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// The three verdicts, and the rule the whole verb rests on: the folder it reads
// is not touched. An import READS; the person keeps their folder.
func TestAdminImportMakesAClipPerVideoAndNeverTouchesTheFolder(t *testing.T) {
	fx, dir := newImportFixture(t, nil)
	plant(t, dir, "Team fight 3.mp4", "video-one-bytes")
	plant(t, dir, "Holiday.mkv", "video-two-different-bytes")
	plant(t, dir, "notes-junk.txt", "this is not a video")

	code, out := fx.admin(t, "import", dir, "--owner", "carol")
	if code != 0 {
		t.Fatalf("import exited %d\n%s", code, out)
	}
	if n := strings.Count(out, "imported "); n != 2 {
		t.Fatalf("want two imported lines, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "refused not-a-video notes-junk.txt") {
		t.Fatalf("the junk file was not refused by name and reason:\n%s", out)
	}
	if !strings.Contains(out, "2 imported, 0 seen, 1 refused,") {
		t.Fatalf("the summary does not count the three verdicts:\n%s", out)
	}
	if ids := clipsOf(t, fx); len(ids) != 2 {
		t.Fatalf("the instance holds %d clips, want 2", len(ids))
	}
	// The originals are all still exactly where they were.
	for _, name := range []string{"Team fight 3.mp4", "Holiday.mkv", "notes-junk.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("the import moved or removed %q: %v", name, err)
		}
	}
	// And it wrote nothing of its own into the folder — no `.failed.txt`, no
	// staging directory: those are the drop folder's, in a folder this app owns.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the import left something in the folder: %v", names)
	}
}

// The title is the file name without its extension — erratum 60's rule for
// every other upload, and the only thing anybody typed here either.
func TestAdminImportTitlesTheClipAfterTheFile(t *testing.T) {
	fx, dir := newImportFixture(t, nil)
	plant(t, dir, "Team fight 3.mp4", "video-one-bytes")
	if code, out := fx.admin(t, "import", dir, "--owner", "carol"); code != 0 {
		t.Fatalf("import exited %d\n%s", code, out)
	}
	ids := clipsOf(t, fx)
	if len(ids) != 1 {
		t.Fatalf("want one clip, got %d", len(ids))
	}
	_, out := fx.admin(t, "ls", ids[0])
	if !strings.Contains(out, "Team fight 3") {
		t.Fatalf("the clip is not titled after the file:\n%s", out)
	}
	// And it is the named ACCOUNT's, which `ls --owner` is the verb for.
	_, owned := fx.admin(t, "ls", "--owner", "carol")
	if !strings.Contains(owned, ids[0]) || !strings.Contains(owned, "1 clips") {
		t.Fatalf("the clip is not owned by the named account:\n%s", owned)
	}
}

// Idempotent by CONTENT: a second run over the same folder imports nothing and
// says so per file, naming the clip the bytes already are.
func TestAdminImportASecondTimeImportsNothing(t *testing.T) {
	fx, dir := newImportFixture(t, nil)
	plant(t, dir, "one.mp4", "video-one-bytes")
	plant(t, dir, "two.mp4", "video-two-bytes-longer")
	if code, out := fx.admin(t, "import", dir, "--owner", "carol"); code != 0 {
		t.Fatalf("the first import exited %d\n%s", code, out)
	}
	first := clipsOf(t, fx)
	if len(first) != 2 {
		t.Fatalf("the first import made %d clips, want 2", len(first))
	}

	code, out := fx.admin(t, "import", dir, "--owner", "carol")
	if code != 0 {
		t.Fatalf("the second import exited %d\n%s", code, out)
	}
	if !strings.Contains(out, "0 imported, 2 seen, 0 refused,") {
		t.Fatalf("the second run did not recognise what it already holds:\n%s", out)
	}
	for _, id := range first {
		if !strings.Contains(out, "seen "+id+" ") {
			t.Fatalf("the second run does not name the clip %s the bytes already are:\n%s", id, out)
		}
	}
	if after := clipsOf(t, fx); len(after) != 2 {
		t.Fatalf("the second run changed the clip count to %d", len(after))
	}
}

// A file of the SAME SIZE but different bytes is a different video, and the
// size index is only the cheap half of the question.
func TestAdminImportDoesNotConfuseTwoFilesOfOneSize(t *testing.T) {
	fx, dir := newImportFixture(t, nil)
	plant(t, dir, "first.mp4", "aaaaaaaaaaaaaaaa")
	if code, out := fx.admin(t, "import", dir, "--owner", "carol"); code != 0 {
		t.Fatalf("import exited %d\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(dir, "first.mp4")); err != nil {
		t.Fatal(err)
	}
	plant(t, dir, "second.mp4", "bbbbbbbbbbbbbbbb") // same length, other bytes

	code, out := fx.admin(t, "import", dir, "--owner", "carol")
	if code != 0 {
		t.Fatalf("import exited %d\n%s", code, out)
	}
	if !strings.Contains(out, "1 imported, 0 seen,") {
		t.Fatalf("a different video of the same size was taken for the first:\n%s", out)
	}
}

// --dry-run prints the verdict it would reach for every file — `seen`
// included, which is how an operator checks idempotence before committing to
// it — and creates nothing.
func TestAdminImportDryRunTouchesNothing(t *testing.T) {
	fx, dir := newImportFixture(t, nil)
	plant(t, dir, "one.mp4", "video-one-bytes")
	plant(t, dir, "notes-junk.txt", "not a video")

	code, out := fx.admin(t, "import", dir, "--owner", "carol", "--dry-run")
	if code != 0 {
		t.Fatalf("--dry-run exited %d\n%s", code, out)
	}
	if !strings.Contains(out, "import one.mp4") {
		t.Fatalf("the plan does not name what it would import:\n%s", out)
	}
	if !strings.Contains(out, "refused not-a-video notes-junk.txt") {
		t.Fatalf("the plan does not name what it would refuse:\n%s", out)
	}
	if !strings.Contains(out, "nothing was imported") {
		t.Fatalf("the plan does not say it imported nothing:\n%s", out)
	}
	if ids := clipsOf(t, fx); len(ids) != 0 {
		t.Fatalf("--dry-run created %d clips", len(ids))
	}
	if _, err := os.Stat(filepath.Join(fx.media, "clips")); err == nil {
		t.Fatal("--dry-run wrote something into the store")
	}

	// And the run that follows it does exactly what the plan said.
	if code, out := fx.admin(t, "import", dir, "--owner", "carol"); code != 0 {
		t.Fatalf("the real run exited %d\n%s", code, out)
	}
	if ids := clipsOf(t, fx); len(ids) != 1 {
		t.Fatalf("the real run made %d clips, want 1", len(ids))
	}
}

// A refused file leaves NOTHING: no row, and nothing under the clip prefix in
// the store. `ingestCore` cleans up after itself and the verb adds no path of
// its own that could skip it.
func TestAdminImportLeavesNoRowForARefusedFile(t *testing.T) {
	fx, dir := newImportFixture(t, nil)
	plant(t, dir, "notes-junk.mp4", "not a video after all")
	plant(t, dir, "empty.mp4", "")

	code, out := fx.admin(t, "import", dir, "--owner", "carol")
	if code != 0 {
		t.Fatalf("a folder of nothing importable exited %d (a file's own refusal is not an error)\n%s", code, out)
	}
	if !strings.Contains(out, "refused empty empty.mp4") {
		t.Fatalf("an empty file was not refused as empty:\n%s", out)
	}
	if ids := clipsOf(t, fx); len(ids) != 0 {
		t.Fatalf("a refused file left %d rows behind", len(ids))
	}
	if entries, err := os.ReadDir(filepath.Join(fx.media, "clips")); err == nil && len(entries) > 0 {
		t.Fatalf("a refused file left %d object prefixes behind", len(entries))
	}
}

// The storage quota is THIS INSTANCE's refusal, not the file's: it surfaces
// with its own word, it says the one sentence every surface of the cap says,
// and it makes the command exit non-zero — because making room and running it
// again is exactly what the operator should do.
func TestAdminImportSurfacesTheQuotaRefusal(t *testing.T) {
	fx, dir := newImportFixture(t, map[string]string{
		"STORAGE_BUDGET_BYTES": "10",
		"STORAGE_BUDGET_HARD":  "1",
	})
	plant(t, dir, "one.mp4", "this is more than ten bytes long")
	plant(t, dir, "two.mp4", "and so is this one, by some way")

	code, out := fx.admin(t, "import", dir, "--owner", "carol")
	if !strings.Contains(out, "refused quota ") {
		t.Fatalf("the quota refusal does not carry its own word:\n%s", out)
	}
	if !strings.Contains(out, "storage quota") {
		t.Fatalf("the quota refusal does not say what to do about it:\n%s", out)
	}
	if code != 1 {
		t.Fatalf("an import stopped by the quota exited %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "run this again") {
		t.Fatalf("the closing line does not tell the operator what to do:\n%s", out)
	}
}

// A file larger than this instance accepts is refused without being read, by
// the size the filesystem already reported — the declared-size gate every
// upload route applies to Content-Length.
func TestAdminImportRefusesAFileTooLargeWithoutReadingIt(t *testing.T) {
	fx, dir := newImportFixture(t, map[string]string{"MAX_UPLOAD_BYTES": "8"})
	plant(t, dir, "big.mp4", "rather more than eight bytes")

	code, out := fx.admin(t, "import", dir, "--owner", "carol")
	if code != 0 {
		t.Fatalf("a file's own refusal made the command exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "refused too-large big.mp4") || !strings.Contains(out, "MAX_UPLOAD_BYTES") {
		t.Fatalf("an oversized file was not refused by name and variable:\n%s", out)
	}
	if ids := clipsOf(t, fx); len(ids) != 0 {
		t.Fatalf("an oversized file made %d clips", len(ids))
	}
}

// The owner is required and must exist: there is no default account, and a typo
// must not quietly become somebody else's library.
func TestAdminImportNeedsAnOwnerThatExists(t *testing.T) {
	fx, dir := newImportFixture(t, nil)
	plant(t, dir, "one.mp4", "video-one-bytes")

	if code, out := fx.admin(t, "import", dir); code != 2 {
		t.Fatalf("import with no --owner exited %d, want 2 (usage)\n%s", code, out)
	}
	code, out := fx.admin(t, "import", dir, "--owner", "nobody")
	if code != 1 {
		t.Fatalf("import for an account that does not exist exited %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "no such user") {
		t.Fatalf("the refusal does not say what is wrong:\n%s", out)
	}
	if ids := clipsOf(t, fx); len(ids) != 0 {
		t.Fatalf("an import with no owner made %d clips", len(ids))
	}
}

// --recursive walks sub-directories; without it the walk is one level, exactly
// as the drop folder's is. Dotted directories are skipped either way.
func TestAdminImportRecursiveWalksSubFolders(t *testing.T) {
	fx, dir := newImportFixture(t, nil)
	plant(t, dir, "top.mp4", "video-at-the-top")
	plant(t, dir, filepath.Join("2019", "old.mp4"), "video-in-a-subfolder")
	plant(t, dir, filepath.Join(".hidden", "secret.mp4"), "video-in-a-dotted-folder")

	code, out := fx.admin(t, "import", dir, "--owner", "carol")
	if code != 0 {
		t.Fatalf("import exited %d\n%s", code, out)
	}
	if !strings.Contains(out, "1 imported, 0 seen, 0 refused,") {
		t.Fatalf("a one-level import did not stop at one level:\n%s", out)
	}

	code, out = fx.admin(t, "import", dir, "--owner", "carol", "--recursive")
	if code != 0 {
		t.Fatalf("--recursive exited %d\n%s", code, out)
	}
	if !strings.Contains(out, "1 imported, 1 seen, 0 refused,") {
		t.Fatalf("--recursive did not take the sub-folder (and leave the dotted one):\n%s", out)
	}
	if !strings.Contains(out, "imported ") || !strings.Contains(out, "2019/old.mp4") {
		t.Fatalf("the sub-folder's file is not named by its path:\n%s", out)
	}
	if strings.Contains(out, "secret.mp4") {
		t.Fatalf("a dotted directory was walked:\n%s", out)
	}
}

// This instance's OWN directories are not a folder of old clips. Pointing the
// verb at the store would walk every rendition of every clip back through the
// upload path — with --recursive, the whole library, re-imported.
func TestAdminImportRefusesThisInstancesOwnDirectories(t *testing.T) {
	fx, _ := newImportFixture(t, nil)
	for _, dir := range []string{fx.media, fx.data, fx.scratch, filepath.Join(fx.media, "clips")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		code, out := fx.admin(t, "import", dir, "--owner", "carol")
		if code != 2 {
			t.Fatalf("import %s exited %d, want 2\n%s", dir, code, out)
		}
		if !strings.Contains(out, "this instance's own directories") {
			t.Fatalf("the refusal does not say why %s is not importable:\n%s", dir, out)
		}
	}
}

// A path that is not a directory, or is not there at all, is a usage error
// rather than an empty run that reports success.
func TestAdminImportNeedsADirectory(t *testing.T) {
	fx, dir := newImportFixture(t, nil)
	plant(t, dir, "one.mp4", "video-one-bytes")
	if code, out := fx.admin(t, "import", filepath.Join(dir, "one.mp4"), "--owner", "carol"); code != 2 {
		t.Fatalf("import of a FILE exited %d, want 2\n%s", code, out)
	}
	if code, out := fx.admin(t, "import", filepath.Join(dir, "nope"), "--owner", "carol"); code != 2 {
		t.Fatalf("import of a missing directory exited %d, want 2\n%s", code, out)
	}
}

// The verb is in `admin help`, because a verb nobody can find is a verb nobody
// runs (the same rule every other subcommand's line keeps).
func TestAdminImportIsInTheUsage(t *testing.T) {
	var b strings.Builder
	adminUsage(&b)
	if !strings.Contains(b.String(), "import <dir> --owner <username>") {
		t.Fatalf("admin help does not carry the import verb:\n%s", b.String())
	}
}

// The two name-shaped refusals carry their own word: an operator who pointed at
// this folder hears about a half-written download and about something that is
// not a file, and hears nothing about a dotfile or a sub-folder.
func TestAdminImportNamesWhatItWillNotEvenOpen(t *testing.T) {
	fx, dir := newImportFixture(t, nil)
	plant(t, dir, "half.mp4.part", "a download in flight")
	plant(t, dir, ".DS_Store", "the OS put this here")
	if err := os.MkdirAll(filepath.Join(dir, "2019"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "half.mp4.part"), filepath.Join(dir, "link.mp4")); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}

	code, out := fx.admin(t, "import", dir, "--owner", "carol")
	if code != 0 {
		t.Fatalf("import exited %d\n%s", code, out)
	}
	if !strings.Contains(out, "refused still-being-written half.mp4.part") {
		t.Fatalf("a download in flight was not refused by its own word:\n%s", out)
	}
	if !strings.Contains(out, "refused not-a-file link.mp4") {
		t.Fatalf("a symlink was not refused by its own word (and must never be followed):\n%s", out)
	}
	if strings.Contains(out, ".DS_Store") || strings.Contains(out, "2019") {
		t.Fatalf("a dotfile or a sub-folder was mentioned; both are skipped in silence:\n%s", out)
	}
	if !strings.Contains(out, "0 imported, 0 seen, 2 refused,") {
		t.Fatalf("the summary counts something other than the two refusals:\n%s", out)
	}
}
