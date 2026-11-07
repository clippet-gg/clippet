package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clippet/internal/store"
)

// TestBootStoreMarker is §20 erratum 348 (c): `clippet serve` writes the folder
// store's marker into a fresh folder, and NEVER into one the database says
// should already hold clips — there it logs one ERROR and hands the ping loop
// the error that makes the instance serve degraded, so the marker's absence,
// the one signal `admin doctor` reads for an unmounted share, survives.
func TestBootStoreMarker(t *testing.T) {
	sum := func(n int64, err error) func(context.Context) (int64, error) {
		return func(context.Context) (int64, error) { return n, err }
	}
	ctx := context.Background()

	// A fresh folder over a database with no stored clips: written.
	dir := t.TempDir()
	if err := bootStoreMarker(ctx, dir, sum(0, nil), quietLog()); err != nil {
		t.Fatalf("fresh folder: %v", err)
	}
	if ok, _ := store.HasMarker(dir); !ok {
		t.Fatal("the marker was not written into a fresh folder")
	}

	// A folder with no marker over a database that accounts for clips: not
	// written, an ERROR, and a ping error the health line renders by class.
	dir = t.TempDir()
	var buf bytes.Buffer
	err := bootStoreMarker(ctx, dir, sum(4096, nil), slog.New(slog.NewTextHandler(&buf, nil)))
	var pe *store.PingError
	if !errors.As(err, &pe) || pe.HealthClass() != store.PingUnavailable {
		t.Fatalf("an unmarked folder over stored clips: %v", err)
	}
	if ok, _ := store.HasMarker(dir); ok {
		t.Fatal("the marker was written over a folder the database says holds clips")
	}
	// …and the store the process then serves through says so on every ping,
	// so /readyz's live ping cannot clear it.
	if perr := (unmarkedStore{Store: store.NewFake(), err: err}).Ping(ctx); perr != err {
		t.Fatalf("the unmarked store's ping: %v", perr)
	}
	// …and refuses every write, delete and byte read with the same error, so
	// nothing lands in the wrong folder and no row loses its bytes to a delete
	// that "succeeded" against a file that was never there.
	fake := store.NewFake()
	us := unmarkedStore{Store: fake, err: err}
	src := filepath.Join(t.TempDir(), "clip.mp4")
	if werr := os.WriteFile(src, []byte("x"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	if _, perr := us.PutFile(ctx, "c/abc/source.mp4", src, "video/mp4", nil); perr != err {
		t.Fatalf("the unmarked store's PutFile: %v", perr)
	}
	if _, serr := fake.Stat(ctx, "c/abc/source.mp4"); serr == nil {
		t.Fatal("the unmarked store's PutFile reached the folder")
	}
	if derr := us.DeleteKey(ctx, "c/abc/source.mp4"); derr != err {
		t.Fatalf("the unmarked store's DeleteKey: %v", derr)
	}
	if _, derr := us.DeletePrefix(ctx, "c/abc/"); derr != err {
		t.Fatalf("the unmarked store's DeletePrefix: %v", derr)
	}
	if _, rerr := us.GetRange(ctx, "c/abc/source.mp4", 0, -1); rerr != err {
		t.Fatalf("the unmarked store's GetRange: %v", rerr)
	}
	if rerr := us.Download(ctx, "c/abc/source.mp4", filepath.Join(t.TempDir(), "out"), nil); rerr != err {
		t.Fatalf("the unmarked store's Download: %v", rerr)
	}
	if log := buf.String(); !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "the marker was NOT written") ||
		!strings.Contains(log, "stored_bytes=4096") {
		t.Fatalf("the boot log: %s", log)
	}

	// The marker already there: nothing to decide, whatever the database says.
	if err := bootStoreMarker(ctx, t.TempDir(), sum(4096, nil), quietLog()); err == nil {
		t.Fatal("precondition: a second empty folder must refuse too")
	}
	marked := t.TempDir()
	if err := store.WriteMarker(marked); err != nil {
		t.Fatal(err)
	}
	if err := bootStoreMarker(ctx, marked, sum(4096, nil), quietLog()); err != nil {
		t.Fatalf("a marked folder: %v", err)
	}

	// A database that cannot answer keeps the old behaviour: written.
	dir = t.TempDir()
	if err := bootStoreMarker(ctx, dir, sum(0, errors.New("db busy")), quietLog()); err != nil {
		t.Fatalf("an unanswerable sum: %v", err)
	}
	if ok, _ := store.HasMarker(dir); !ok {
		t.Fatal("the marker was not written when the sum could not be read")
	}
}
