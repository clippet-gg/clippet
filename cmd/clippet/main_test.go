package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
)

func testEnv(dataDir string) func(string) string {
	m := map[string]string{
		"PUBLIC_BASE_URL": "http://localhost:8481",
		"S3_ENDPOINT":     "http://minio:9000",
		"S3_BUCKET":       "clips",
		"S3_ACCESS_KEY":   "ak",
		"S3_SECRET_KEY":   "sk",
		"DATA_DIR":        dataDir,
	}
	return func(k string) string { return m[k] }
}

func TestRunDispatch(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, testEnv(""), &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "clippet ") {
		t.Fatalf("version: %d %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"--help"}, testEnv(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), "usage") {
		t.Fatalf("help: %d %q", code, out.String())
	}
	// Bad config → 2, names the variable.
	errOut.Reset()
	if code := run([]string{"migrate"}, func(string) string { return "" }, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "PUBLIC_BASE_URL") {
		t.Fatalf("bad config: %d %q", code, errOut.String())
	}
	errOut.Reset()
	if code := run([]string{"bogus"}, testEnv(t.TempDir()), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("unknown: %d %q", code, errOut.String())
	}
	if code := run([]string{"admin", "stats"}, testEnv(t.TempDir()), &out, &errOut); code != 2 {
		t.Fatalf("admin stub: %d", code)
	}
}

func TestRunMigrate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	cfg, err := config.Load(testEnv(dir))
	if err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(cfg); code != 0 {
		t.Fatalf("runMigrate = %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "clippet.db")); err != nil {
		t.Fatalf("db not created: %v", err)
	}
	if code := runMigrate(cfg); code != 0 {
		t.Fatalf("runMigrate again = %d", code)
	}
}

func TestParseFFmpegVersion(t *testing.T) {
	cases := map[string]string{
		"ffmpeg version 7.1.1 Copyright (c) 2000-2025 the FFmpeg developers\nbuilt with gcc": "7.1.1",
		"ffmpeg version n7.1.1-static https://example\n":                                     "n7.1.1-static",
		"garbage": "unknown",
	}
	for in, want := range cases {
		if got := parseFFmpegVersion(in); got != want {
			t.Errorf("parseFFmpegVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestScratchJanitor covers the full §12 rule: row missing or in
// ready|failed|deleting → removed; mtime older than 24h → removed unless the
// clip is processing; fresh dirs of queued clips, invalid names and stray
// files are left alone.
func TestScratchJanitor(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := config.Load(testEnv(filepath.Join(dir, "data")))
	if code := runMigrate(cfg); code != 0 {
		t.Fatal("migrate")
	}
	database, err := openForTest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := t.Context()
	u, err := database.CreateUser(ctx, ids.New(), "carol", "h", 1)
	if err != nil {
		t.Fatal(err)
	}
	newClip := func() (db.Clip, db.Job) {
		t.Helper()
		id := ids.New()
		c, j, err := database.CreateClipWithJob(ctx, db.NewClip{ID: id, OwnerID: u.ID, Title: id,
			SourceKey: "clips/" + id + "/original/source.mp4", SourceFilename: "f.mp4",
			SourceContentType: "video/mp4", SourceETag: "e", Now: 1})
		if err != nil {
			t.Fatal(err)
		}
		return c, j
	}
	claimed := func() (db.Clip, db.Job) {
		t.Helper()
		c, _ := newClip()
		j, cc, ok, err := database.ClaimJob(ctx, 2)
		if err != nil || !ok || cc.ID != c.ID {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		return cc, j
	}
	// Each clip is created and moved to its state before the next, so
	// ClaimJob (oldest queued first) always takes the intended one.
	ready, jr := claimed()
	if ok, _, err := database.FinalizeTranscode(ctx, jr.ID, ready.ID, db.ReadyFields{RenditionKey: "r", PosterKey: "p"}, false, 3); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	failed, jf := claimed()
	if _, err := database.FailAttempt(ctx, jf.ID, db.ClaimStamp(jf), failed.ID, "bad", db.FailPermanent, db.Limits{}, 3); err != nil {
		t.Fatal(err)
	}
	processing, _ := claimed()
	queuedFresh, _ := newClip()
	queuedStale, _ := newClip()
	deleting, _ := newClip()
	if err := database.MarkDeleting(ctx, deleting.ID, 4); err != nil {
		t.Fatal(err)
	}

	scratch := filepath.Join(dir, "scratch")
	for _, name := range []string{ready.ID, failed.ID, processing.ID, queuedFresh.ID, queuedStale.ID, deleting.ID,
		"aZ3kQ9xB2mLp" /* no row */, "notavalidid"} {
		if err := os.MkdirAll(filepath.Join(scratch, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(scratch, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stale := now.Add(-janitorStaleAge - time.Hour)
	for _, name := range []string{processing.ID, queuedStale.ID} {
		if err := os.Chtimes(filepath.Join(scratch, name), stale, stale); err != nil {
			t.Fatal(err)
		}
	}

	scratchJanitor(ctx, scratch, database, newLogger(cfg), now)

	want := map[string]bool{ // name → gone
		ready.ID: true, failed.ID: true, deleting.ID: true, "aZ3kQ9xB2mLp": true,
		queuedStale.ID: true,  // mtime > 24h and not processing
		processing.ID:  false, // mtime > 24h but processing: the worker owns it
		queuedFresh.ID: false, // queued and fresh: the worker's fast path
		"notavalidid":  false, "stray.txt": false,
	}
	for name, wantGone := range want {
		_, err := os.Stat(filepath.Join(scratch, name))
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s: gone=%v want %v", name, gone, wantGone)
		}
	}
}

// TestWarnShippedCredentials is §20 erratum 104: a stock deployment runs on
// credentials printed in this repository, and the `:-` form in compose treats
// an EMPTY S3_ACCESS_KEY exactly like an unset one — a quiet way to land back
// on them after a half-finished switch to R2/B2. The line fires on the
// COMBINATION (shipped credentials + a public origin that is not loopback),
// because a container cannot see its own port publish, and it never prints
// the values.
func TestWarnShippedCredentials(t *testing.T) {
	base := func(u string) *url.URL {
		p, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name    string
		baseURL string
		ak, sk  string
		want    bool
	}{
		{"dev stack on loopback", "http://localhost:8481", shippedAccessKey, shippedSecretKey, false},
		{"dev stack on 127.0.0.1", "http://127.0.0.1:8481", shippedAccessKey, shippedSecretKey, false},
		{"public origin, shipped pair", "https://clips.example.com", shippedAccessKey, shippedSecretKey, true},
		{"public origin, shipped secret only", "https://clips.example.com", "real-key", shippedSecretKey, true},
		{"public origin, real credentials", "https://clips.example.com", "real-key", "real-secret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &captureHandler{}
			cfg := config.Config{PublicBaseURL: base(tc.baseURL)}
			cfg.S3.AccessKey, cfg.S3.SecretKey = tc.ak, tc.sk
			warnShippedCredentials(slog.New(h), cfg)
			if got := len(h.recs) > 0; got != tc.want {
				t.Fatalf("warned=%v, want %v", got, tc.want)
			}
			for _, r := range h.recs {
				if strings.Contains(r.Message, shippedSecretKey) {
					t.Fatal("the warning printed the secret")
				}
				if r.Level != slog.LevelWarn {
					t.Fatalf("level = %v", r.Level)
				}
			}
		})
	}
}

type captureHandler struct{ recs []slog.Record }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.recs = append(h.recs, r)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }
