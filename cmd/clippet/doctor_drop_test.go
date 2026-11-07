package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/drop"
	"clippet/internal/ids"
)

// The drop folder's doctor row (ROADMAP #42 (b), §20 erratum 296) and the
// `store` row's host-directory lines (ROADMAP #42 (a), §20 erratum 295).

// Off is the default, and the row says so rather than disappearing: a check
// that silently stops running reads as a clean bill of health.
func TestDoctorDropIsOffByDefault(t *testing.T) {
	fx := newFSFixture(t)
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	code, out := fx.doctor(t)
	if code != 0 {
		t.Fatalf("doctor: %d\n%s", code, out)
	}
	if v := verdictOf(out, "drop"); v != vOK {
		t.Fatalf("drop with no owner = %s\n%s", v, out)
	}
	if !strings.Contains(out, "DROP_FOLDER_OWNER is unset") {
		t.Fatalf("the off row does not name the variable:\n%s", out)
	}
	if !strings.Contains(out, "nothing at the root of the media directory is ever read") {
		t.Fatalf("the off row does not say what off MEANS:\n%s", out)
	}
}

// On, for an account that exists: whose library, and what is in the folder now.
func TestDoctorDropOnNamesTheOwnerAndTheFolder(t *testing.T) {
	fx := newFSFixture(t)
	fx.env["DROP_FOLDER_OWNER"] = "carol"
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	seedDropOwner(t, fx, "carol")
	// One file waiting, one refused earlier, one caught mid-ingest.
	write := func(parts ...string) {
		p := filepath.Join(append([]string{fx.media}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("waiting.mp4")
	write("broken.mp4")
	write("broken.mp4" + drop.FailedSuffix)
	write("again.mp4")
	write("again.mp4" + drop.NoteSuffix) // a duplicate's note: neither waiting nor failed
	write(drop.StageDir, "interrupted.mp4")

	code, out := fx.doctor(t)
	if code != 0 {
		t.Fatalf("doctor: %d\n%s", code, out)
	}
	if v := verdictOf(out, "drop"); v != vOK {
		t.Fatalf("drop with a live owner = %s\n%s", v, out)
	}
	for _, want := range []string{
		"owned by carol", fx.media,
		"3 file(s) waiting", "1 with a " + drop.FailedSuffix, "1 staged mid-ingest",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the drop row does not say %q:\n%s", want, out)
		}
	}
}

// An owner that names no account is a FAIL, because `clippet serve` refuses to
// boot on it — and a preflight that reported it after the restart would be
// telling an operator what the logs already had.
func TestDoctorDropFailsOnAnOwnerThatDoesNotExist(t *testing.T) {
	fx := newFSFixture(t)
	fx.env["DROP_FOLDER_OWNER"] = "nobody"
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	code, out := fx.doctor(t)
	if code != 1 {
		t.Fatalf("doctor exited %d on an owner that does not exist\n%s", code, out)
	}
	if v := verdictOf(out, "drop"); v != vFail {
		t.Fatalf("drop with an unknown owner = %s\n%s", v, out)
	}
	if !strings.Contains(out, "names no account") || !strings.Contains(out, "refuse to start") {
		t.Fatalf("the failure does not say what will happen:\n%s", out)
	}
}

// The `store` row names the HOST directory behind /media and which variable
// supplied it. The container cannot otherwise say where on the operator's own
// disk the bytes went (§20 erratum 295).
func TestDoctorStoreNamesTheHostDirectory(t *testing.T) {
	fx := newFSFixture(t)
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	// Nothing told it one: it says nothing at all rather than guessing. That
	// is every `s3` stack, every hand-rolled `docker run`, and every stack
	// older than this erratum.
	_, out := fx.doctor(t)
	if strings.Contains(out, "the host directory bound at") {
		t.Fatalf("the store row invented a host directory:\n%s", out)
	}

	// The operator named it.
	fx.env["STORAGE_DIR_HOST"] = "/srv/clippet-media"
	fx.env["STORAGE_DIR_HOST_SET"] = "1"
	_, out = fx.doctor(t)
	if !strings.Contains(out, "the host directory bound at "+fx.media+" is /srv/clippet-media (STORAGE_DIR_HOST in .env)") {
		t.Fatalf("the store row does not name the operator's own path:\n%s", out)
	}
	if v := verdictOf(out, "store"); v != vOK {
		t.Fatalf("store with a named host directory = %s\n%s", v, out)
	}

	// The desktop default supplied it: the same fact, worded as the default.
	fx.env["STORAGE_DIR_HOST"] = "/mnt/videos/clippet"
	fx.env["STORAGE_DIR_HOST_SET"] = ""
	_, out = fx.doctor(t)
	if !strings.Contains(out, "/mnt/videos/clippet (the desktop default") {
		t.Fatalf("the store row does not say the default supplied it:\n%s", out)
	}
	if v := verdictOf(out, "store"); v != vOK {
		t.Fatalf("store on the desktop default = %s\n%s", v, out)
	}

	// The one gap compose cannot close itself: neither HOME nor USERPROFILE
	// was exported, so `${HOME:-${USERPROFILE}}` was empty and the bind landed
	// at the root of the host filesystem. Compose v2 evaluates a nested `:?`
	// eagerly (scripts/fsdefault.sh), so this WARN is the only place it can be
	// caught.
	fx.env["STORAGE_DIR_HOST"] = "/Videos/clippet"
	_, out = fx.doctor(t)
	if v := verdictOf(out, "store"); v != vWarn {
		t.Fatalf("store on a root-level default = %s\n%s", v, out)
	}
	for _, want := range []string{"at the ROOT of the host filesystem", "HOME", "USERPROFILE", "STORAGE_DIR_HOST"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the warning does not say %q:\n%s", want, out)
		}
	}
}

// seedDropOwner registers the account the folder ingests as.
func seedDropOwner(t *testing.T, fx *fsFixture, name string) {
	t.Helper()
	d, err := db.Open(dbPath(fx.cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser(context.Background(), ids.New(), name, "$2a$12$notahash", fx.now.Unix()); err != nil {
		t.Fatal(err)
	}
}

// On a BUCKET instance the row reads the same (§20 erratum 296 (m)): the
// watcher wants a directory on this box, not a backend, and STORAGE_DIR already
// names one on `s3` — where it used to mean only "what `admin storage migrate
// --to fs` would copy into". Before the erratum this configuration did not
// load at all, so the row could not exist.
func TestDoctorDropOnABucketInstanceReadsTheSame(t *testing.T) {
	fx := newFSFixture(t)
	// A bucket nothing can reach: the `bucket` row's verdict is not what this
	// asserts, and a refused connection costs nothing.
	fx.env["STORAGE_BACKEND"] = "s3"
	fx.env["S3_ENDPOINT"] = "http://127.0.0.1:1"
	fx.env["S3_BUCKET"] = "clips"
	fx.env["S3_ACCESS_KEY"] = "ak"
	fx.env["S3_SECRET_KEY"] = "sk"
	fx.env["DROP_FOLDER_OWNER"] = "carol"
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	seedDropOwner(t, fx, "carol")
	if err := os.WriteFile(filepath.Join(fx.media, "waiting.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, out := fx.doctor(t)
	if v := verdictOf(out, "drop"); v != vOK {
		t.Fatalf("drop on a bucket instance with a media directory = %s\n%s", v, out)
	}
	for _, want := range []string{"owned by carol", "ROOT of " + fx.media, "1 file(s) waiting"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the drop row does not say %q:\n%s", want, out)
		}
	}
}
