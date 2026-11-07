package main

// snapshot_test.go — the database snapshot in the bucket (§20 erratum 343):
// the debounce on a fake clock, Touch never blocking, the three objects and
// their order, the owner guard, the boot rule, the failure record, the doctor
// row's four shapes, `objects _clippet`, `orphans` leaving the prefix alone and
// `restore --from-store`.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
)

// fakeClock drives the loop's timers by hand. After registers a deadline;
// Advance moves the clock and fires every deadline it has reached.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
	w  []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.t
		return ch
	}
	c.w = append(c.w, fakeWaiter{at: c.t.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	keep := c.w[:0]
	for _, w := range c.w {
		if w.at.After(c.t) {
			keep = append(keep, w)
			continue
		}
		w.ch <- c.t
	}
	c.w = keep
}

// waitDeadline waits until a deadline at `at` is registered: the loop has
// taken the news in and is waiting on its debounce.
func (c *fakeClock) waitDeadline(t *testing.T, at time.Time) { c.waitDeadlines(t, at, 1) }

// waitDeadlines waits until n timers for `at` are registered — a loop
// iteration that set one and then went round again sets a second.
func (c *fakeClock) waitDeadlines(t *testing.T, at time.Time, n int) {
	t.Helper()
	for i := 0; i < 400; i++ {
		c.mu.Lock()
		got := 0
		for _, w := range c.w {
			if w.at.Equal(at) {
				got++
			}
		}
		c.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fewer than %d timer(s) were set for %s", n, at)
}

// snapshotCfg is a migrated instance in a temp dir, on `s3`, snapshot on.
func snapshotCfg(t *testing.T, extra map[string]string) config.Config {
	t.Helper()
	root := t.TempDir()
	m := map[string]string{
		"PUBLIC_BASE_URL": "http://localhost:8481", "S3_ENDPOINT": "http://minio:9000", "S3_BUCKET": "clips",
		"S3_ACCESS_KEY": "ak", "S3_SECRET_KEY": "sk",
		"DATA_DIR": filepath.Join(root, "data"), "SCRATCH_DIR": filepath.Join(root, "scratch"),
	}
	for k, v := range extra {
		m[k] = v
	}
	cfg, err := config.Load(func(k string) string { return m[k] })
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{cfg.DataDir, cfg.ScratchDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

// migratedDB is cfg's database, migrated, holding one user and n clips.
func migratedDB(t *testing.T, cfg config.Config, n int) *db.DB {
	t.Helper()
	if code := runMigrate(cfg); code != 0 {
		t.Fatal("migrate")
	}
	d, err := db.Open(dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()
	u, err := d.CreateUser(ctx, ids.New(), "carol", "h", 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		id := ids.New()
		k := store.KeysFor("", id, "mp4")
		if _, _, err := d.CreateClipWithJob(ctx, db.NewClip{ID: id, OwnerID: u.ID, Title: "t", SourceKey: k.Original,
			SourceFilename: "f.mp4", SourceSize: 3, SourceContentType: "video/mp4", SourceETag: "e", Now: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// loopFixture is a snapshotter on a fake clock whose upload is a counter.
type loopFixture struct {
	clock   *fakeClock
	s       *dbSnapshotter
	uploads chan time.Time
	cancel  context.CancelFunc
	done    chan struct{}
}

func newLoop(t *testing.T, every time.Duration) *loopFixture {
	t.Helper()
	cfg := snapshotCfg(t, nil)
	cfg.DBSnapshotEvery = every
	fx := &loopFixture{clock: &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}, uploads: make(chan time.Time, 64),
		done: make(chan struct{})}
	fx.s = newDBSnapshotter(cfg, nil, store.NewFake(), quietLog())
	fx.s.now, fx.s.after = fx.clock.Now, fx.clock.After
	fx.s.write = func(context.Context) (int64, error) {
		fx.uploads <- fx.clock.Now()
		return 42, nil
	}
	return fx
}

func (fx *loopFixture) start() {
	ctx, cancel := context.WithCancel(context.Background())
	fx.cancel = cancel
	go func() {
		defer close(fx.done)
		fx.s.Run(ctx)
	}()
}

func (fx *loopFixture) stop(t *testing.T) {
	t.Helper()
	fx.cancel()
	select {
	case <-fx.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop on cancel")
	}
}

func (fx *loopFixture) upload(t *testing.T, what string) time.Time {
	t.Helper()
	select {
	case at := <-fx.uploads:
		return at
	case <-time.After(5 * time.Second):
		t.Fatalf("no upload: %s", what)
	}
	return time.Time{}
}

func (fx *loopFixture) none(t *testing.T, what string) {
	t.Helper()
	select {
	case at := <-fx.uploads:
		t.Fatalf("an upload at %s: %s", at, what)
	case <-time.After(60 * time.Millisecond):
	}
}

// TestSnapshotDebounce: a burst of changes is ONE upload, made when the window
// since the last one closes (the trailing write), and nothing after it until
// something changes again — or a day passes with nothing tracked.
func TestSnapshotDebounce(t *testing.T) {
	every := 10 * time.Minute
	fx := newLoop(t, every)
	t0 := fx.clock.Now()
	fx.start()
	defer fx.stop(t)
	// Nothing in the bucket: one upload at boot, at once.
	if at := fx.upload(t, "the boot upload"); !at.Equal(t0) {
		t.Fatalf("the boot upload at %s, want %s", at, t0)
	}
	// A burst of ready events right after it.
	for i := 0; i < 50; i++ {
		fx.s.Touch()
	}
	fx.clock.waitDeadline(t, t0.Add(every))
	fx.none(t, "inside the window")
	fx.clock.Advance(every - time.Second)
	fx.none(t, "a second before the window closes")
	fx.clock.Advance(time.Second)
	if at := fx.upload(t, "the trailing write"); !at.Equal(t0.Add(every)) {
		t.Fatalf("the trailing write at %s, want %s", at, t0.Add(every))
	}
	fx.none(t, "a second upload for the same burst")
	// Quiet: nothing for hours.
	fx.clock.Advance(3 * every)
	fx.none(t, "no change, no upload")
	// One change after a long quiet goes at once (the window is long closed).
	fx.s.Touch()
	if at := fx.upload(t, "a lone change after the quiet"); !at.Equal(t0.Add(4 * every)) {
		t.Fatalf("the lone change's upload at %s, want %s", at, t0.Add(4*every))
	}
	// A day with nothing tracked still gets one (a user, a token, a setting).
	// Two day-timers end then: the one set while the lone change waited for
	// its (immediate) turn, and the one set after its upload — which is the
	// one the loop is now waiting on.
	fx.clock.waitDeadlines(t, t0.Add(4*every+snapshotIdle), 2)
	fx.clock.Advance(snapshotIdle)
	fx.upload(t, "the day's untracked changes")
}

// TestSnapshotTouchNeverBlocks: Touch is the request path's and the worker's
// cost of the feature — it must return at once while an upload is stuck, and
// before the loop has even started.
func TestSnapshotTouchNeverBlocks(t *testing.T) {
	fx := newLoop(t, time.Minute)
	for i := 0; i < 1000; i++ {
		fx.s.Touch() // not started: the channel of one is full after the first
	}
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	fx.s.write = func(ctx context.Context) (int64, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return 1, nil
	}
	fx.start()
	<-entered // the boot upload is in flight and stuck
	start := time.Now()
	for i := 0; i < 100000; i++ {
		fx.s.Touch()
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("100000 Touches during a stuck upload took %s", d)
	}
	close(release)
	fx.stop(t)
}

// TestSnapshotFailureIsAWarnAndARetry: a failed upload is recorded for doctor
// and retried one interval on; the next success clears the record.
func TestSnapshotFailureIsAWarnAndARetry(t *testing.T) {
	every := 10 * time.Minute
	fx := newLoop(t, every)
	t0 := fx.clock.Now()
	var calls int
	fx.s.write = func(context.Context) (int64, error) {
		calls++
		fx.uploads <- fx.clock.Now()
		if calls == 1 {
			return 0, errors.New("the bucket said no")
		}
		return 7, nil
	}
	fx.start()
	defer fx.stop(t)
	fx.upload(t, "the boot attempt")
	fx.clock.waitDeadline(t, t0.Add(every))
	st, ok := readSnapshotStatus(fx.s.cfg.DataDir)
	if !ok || st.Reason != "the bucket said no" || st.FailedAt != rfcTime(t0) || st.LastOK != "" {
		t.Fatalf("the failure was not recorded: %+v %v", st, ok)
	}
	fx.clock.Advance(every)
	fx.upload(t, "the retry")
	for i := 0; i < 200; i++ {
		if st, _ = readSnapshotStatus(fx.s.cfg.DataDir); st.LastOK != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st.LastOK != rfcTime(t0.Add(every)) || st.Reason != "" || st.Bytes != 7 || fx.s.Bytes() != 7 {
		t.Fatalf("after the retry: %+v bytes=%d", st, fx.s.Bytes())
	}
}

// TestSnapshotBootRule: a recent copy in the bucket is not rewritten at boot;
// an old one is; a `pending` left by the last process is written whatever the
// copy's age; and a stop with a change pending records one.
func TestSnapshotBootRule(t *testing.T) {
	cfg := snapshotCfg(t, nil)
	fake := store.NewFake()
	fake.Put(store.SnapshotKey(""), []byte("sqlite"), "")
	fake.Put(store.SnapshotSumKey(""), []byte("x"), "")
	s := newDBSnapshotter(cfg, nil, fake, quietLog())
	if _, dirty := s.boot(context.Background()); dirty {
		t.Fatal("a copy a moment old was due at boot")
	}
	if s.Bytes() != int64(len("sqlite")) {
		t.Fatalf("boot did not read the copy's size: %d", s.Bytes())
	}
	s.now = func() time.Time { return time.Now().Add(cfg.DBSnapshotEvery + time.Minute) }
	if _, dirty := s.boot(context.Background()); !dirty {
		t.Fatal("a copy older than the debounce was not due at boot")
	}
	if err := writeJSONAtomic(cfg.DataDir, snapshotStatusName, snapshotStatus{Pending: true}); err != nil {
		t.Fatal(err)
	}
	s = newDBSnapshotter(cfg, nil, fake, quietLog())
	if _, dirty := s.boot(context.Background()); !dirty {
		t.Fatal("a pending change was not due at boot")
	}
	// A stop with a change not yet uploaded leaves `pending` for the next boot.
	fx := newLoop(t, time.Hour)
	fx.start()
	fx.upload(t, "boot")
	fx.s.Touch()
	fx.clock.waitDeadline(t, fx.clock.Now().Add(time.Hour))
	fx.stop(t)
	if st, _ := readSnapshotStatus(fx.s.cfg.DataDir); !st.Pending {
		t.Fatalf("a stop with a change pending did not record it: %+v", st)
	}
}

// TestSnapshotWriteOnce: the real upload — the meta, then the database, then
// the sha256 sidecar, in that order; the sidecar is the file's hash; the meta
// names the clips and the database's birth; no scratch file is left.
func TestSnapshotWriteOnce(t *testing.T) {
	cfg := snapshotCfg(t, nil)
	d := migratedDB(t, cfg, 2)
	fake := store.NewFake()
	s := newDBSnapshotter(cfg, d, fake, quietLog())
	n, err := s.writeOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := fake.Get(store.SnapshotKey(""))
	if !ok || int64(len(raw)) != n {
		t.Fatalf("the snapshot object: present=%v len=%d want %d", ok, len(raw), n)
	}
	h := sha256.Sum256(raw)
	sum, _ := fake.Get(store.SnapshotSumKey(""))
	if trimHex(sum) != hex.EncodeToString(h[:]) || !strings.HasSuffix(string(sum), "\n") {
		t.Fatalf("the sidecar %q is not the object's sha256 %x", sum, h)
	}
	var meta snapshotMeta
	mraw, _ := fake.Get(store.SnapshotMetaKey(""))
	if err := json.Unmarshal(mraw, &meta); err != nil {
		t.Fatal(err)
	}
	born, _ := d.Born(context.Background())
	if meta.Clips != 2 || meta.Bytes != n || meta.SHA256 != hex.EncodeToString(h[:]) || meta.Born != born || meta.WrittenAt == "" {
		t.Fatalf("meta = %+v (born %d)", meta, born)
	}
	var puts []string
	for _, c := range fake.CallsSnapshot() {
		if strings.HasPrefix(c, "PutFile ") {
			puts = append(puts, strings.TrimPrefix(c, "PutFile "))
		}
	}
	want := []string{store.SnapshotMetaKey(""), store.SnapshotKey(""), store.SnapshotSumKey("")}
	if strings.Join(puts, ",") != strings.Join(want, ",") {
		t.Fatalf("the writes were %v, want %v (the sidecar LAST)", puts, want)
	}
	// It is a database a restore-check accepts.
	tmp := filepath.Join(t.TempDir(), "x.db")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if insp, err := db.Inspect(context.Background(), tmp); err != nil || !insp.OK() || insp.Clips != 2 {
		t.Fatalf("the object is not a sound database: %+v %v", insp, err)
	}
	if left, _ := os.ReadDir(cfg.ScratchDir); len(left) != 0 {
		t.Fatalf("scratch kept %d file(s): %v", len(left), left)
	}
	// A key prefix is honoured.
	cfg.S3.KeyPrefix = "inst/"
	s = newDBSnapshotter(cfg, d, fake, quietLog())
	if _, err := s.writeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Get("inst/_clippet/db.sqlite.sha256"); !ok {
		t.Fatalf("no snapshot under the key prefix: %v", fake.Keys())
	}
}

// TestSnapshotNeverOverwritesAnotherDatabase: the bucket's meta names another
// database's birth — a fresh PC before its restore, a probe on the same prefix
// — and nothing is written over it.
func TestSnapshotNeverOverwritesAnotherDatabase(t *testing.T) {
	cfg := snapshotCfg(t, nil)
	d := migratedDB(t, cfg, 1)
	fake := store.NewFake()
	fake.Put(store.SnapshotMetaKey(""), mustJSON(snapshotMeta{Born: 1, WrittenAt: "2026-01-01T00:00:00Z"}), "application/json")
	fake.Put(store.SnapshotKey(""), []byte("the precious one"), "")
	s := newDBSnapshotter(cfg, d, fake, quietLog())
	_, err := s.writeOnce(context.Background())
	if !errors.Is(err, errForeignSnapshot) || !strings.Contains(err.Error(), "S3_KEY_PREFIX") {
		t.Fatalf("writeOnce over another database's snapshot: %v", err)
	}
	if raw, _ := fake.Get(store.SnapshotKey("")); string(raw) != "the precious one" {
		t.Fatal("the other database's snapshot was overwritten")
	}
}

// TestDoctorSnapshotRow: the row's four shapes — on with age and size, off
// (folder home / DB_SNAPSHOT=off), never, and FAIL with the server's reason.
func TestDoctorSnapshotRow(t *testing.T) {
	cfg := snapshotCfg(t, nil)
	fake := store.NewFake()
	var out bytes.Buffer
	now := time.Now().Add(2*time.Minute + 5*time.Second)
	r := &doctorRun{env: adminEnv{stdout: &out, now: func() time.Time { return now },
		openStore: func(config.Config) (store.Store, error) { return fake, nil }}}
	row := func(c config.Config) (string, string) {
		out.Reset()
		v := r.checkSnapshot(context.Background(), c)
		return v, out.String()
	}
	if v, s := row(cfg); v != vOK || !strings.Contains(s, "snapshot ok   never:") {
		t.Fatalf("never: %s %q", v, s)
	}
	fake.Put(store.SnapshotKey(""), bytes.Repeat([]byte("x"), 1363149), "")
	fake.Put(store.SnapshotSumKey(""), []byte("ab\n"), "")
	if v, s := row(cfg); v != vOK || !strings.Contains(s, "snapshot ok   on · 2 min ago · 1.3 MiB") {
		t.Fatalf("on: %s %q", v, s)
	}
	// Off with timed backups on: ok, and it says what the store alone cannot do.
	off := cfg
	off.DBSnapshot = false
	off.BackupEvery, off.BackupDir = time.Hour, t.TempDir()
	if v, s := row(off); v != vOK || !strings.Contains(s, "snapshot ok   off (DB_SNAPSHOT=off): the bucket alone cannot") {
		t.Fatalf("off: %s %q", v, s)
	}
	// Off with timed backups off too: NO backup of any kind is a WARN naming
	// both knobs (§20 erratum 348), on either backend.
	bare := off
	bare.BackupEvery, bare.BackupDir = 0, ""
	for _, backend := range []string{config.BackendS3, config.BackendFS} {
		bare.StorageBackend = backend
		if v, s := row(bare); v != vWarn || !strings.Contains(s, "has NO backup of any kind") ||
			!strings.Contains(s, "DB_SNAPSHOT") || !strings.Contains(s, "BACKUP_EVERY and BACKUP_DIR") {
			t.Fatalf("no backup at all on %s: %s %q", backend, v, s)
		}
	}
	// A folder home keeps its snapshot too (erratum 348): the same row, read
	// from the media folder.
	fsOn := cfg
	fsOn.StorageBackend = config.BackendFS
	empty := &doctorRun{env: adminEnv{stdout: &out, now: r.env.now,
		openStore: func(config.Config) (store.Store, error) { return store.NewFake(), nil }}}
	out.Reset()
	if v := empty.checkSnapshot(context.Background(), fsOn); v != vOK || !strings.Contains(out.String(), "snapshot ok   never: the media folder holds no _clippet/db.sqlite") {
		t.Fatalf("fs on, nothing yet: %s %q", v, out.String())
	}
	if v, s := row(fsOn); v != vOK || !strings.Contains(s, "snapshot ok   on · 2 min ago · 1.3 MiB") {
		t.Fatalf("fs on: %s %q", v, s)
	}
	if err := writeJSONAtomic(cfg.DataDir, snapshotStatusName, snapshotStatus{LastOK: "2026-10-19T11:00:00Z",
		FailedAt: "2026-10-19T12:00:00Z", Reason: "Access Denied"}); err != nil {
		t.Fatal(err)
	}
	if v, s := row(cfg); v != vFail || !strings.Contains(s, "snapshot FAIL Access Denied") {
		t.Fatalf("fail: %s %q", v, s)
	}
	// A bucket that cannot be read is a FAIL with the store's words.
	_ = os.Remove(filepath.Join(cfg.DataDir, snapshotStatusName))
	fake.FailNext("Stat", errors.New("bucket down"))
	if v, s := row(cfg); v != vFail || !strings.Contains(s, "bucket down") {
		t.Fatalf("stat failure: %s %q", v, s)
	}
}

// TestReservedPrefixInObjectsAndOrphans: `objects _clippet` lists the prefix
// under its own heading; `orphans --delete` never names or removes it.
func TestReservedPrefixInObjectsAndOrphans(t *testing.T) {
	fx := newAdminFixture(t)
	reserved := []string{store.SnapshotKey(""), store.SnapshotMetaKey(""), store.SnapshotSumKey("")}
	for _, k := range reserved {
		fx.fake.Put(k, []byte("instance"), "")
	}
	code, out := fx.run(t, "objects", "_clippet")
	if code != 0 || !strings.HasPrefix(out, "_clippet/ (this instance's own") || !strings.Contains(out, "_clippet/db.sqlite 8 ") ||
		!strings.Contains(out, "3 object(s)") {
		t.Fatalf("objects _clippet: %d\n%s", code, out)
	}
	if code, out := fx.run(t, "objects", "_clippet", "--count"); code != 0 || strings.TrimSpace(out) != "3" {
		t.Fatalf("objects _clippet --count: %d %q", code, out)
	}
	fx.now = time.Now().Add(2 * time.Hour)
	code, out = fx.run(t, "orphans", "--delete")
	if code != 0 || strings.Contains(out, "_clippet") {
		t.Fatalf("orphans --delete named the reserved prefix: %d\n%s", code, out)
	}
	for _, k := range reserved {
		if _, ok := fx.fake.Get(k); !ok {
			t.Fatalf("orphans --delete removed %s", k)
		}
	}
	if !store.IsReserved("", store.SnapshotKey("")) || store.IsReserved("", "clips/x/_clippet/db.sqlite") {
		t.Fatal("IsReserved")
	}
}

// restoreRun drives `admin restore` against fake for cfg.
func restoreRun(t *testing.T, cfg config.Config, fake *store.Fake, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runAdminWith(append([]string{"restore"}, args...), cfg, adminEnv{stdout: &out, stderr: &errOut,
		openStore: func(config.Config) (store.Store, error) { return fake, nil },
		now:       func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }})
	return code, out.String() + errOut.String()
}

// TestRestoreFromStore: a snapshot one instance wrote brings another up — a
// dry run without --yes, a refusal while a server holds the lock, a refusal
// of a sha that does not match, and with --yes the snapshot in place and the
// database that was there kept aside.
func TestRestoreFromStore(t *testing.T) {
	src := snapshotCfg(t, nil)
	fake := store.NewFake()
	if _, err := newDBSnapshotter(src, migratedDB(t, src, 3), fake, quietLog()).writeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A new PC: no database at all.
	dst := snapshotCfg(t, nil)
	code, out := restoreRun(t, dst, fake, "--from-store")
	if code != 0 || !strings.Contains(out, "sha256 ok") || !strings.Contains(out, "rows:      users=1 clips=3") ||
		!strings.Contains(out, "dry run:") || !strings.Contains(out, "(none: this machine has no database yet)") {
		t.Fatalf("dry run: %d\n%s", code, out)
	}
	if _, err := os.Stat(dbPath(dst)); err == nil {
		t.Fatal("the dry run created a database")
	}
	// A running server holds the lock shared: refused, nothing changed.
	release, err := lockDataDir(dst.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	code, out = restoreRun(t, dst, fake, "--from-store", "--yes")
	release()
	if code != 1 || !strings.Contains(out, "the server is running on this database") {
		t.Fatalf("restore under a running server: %d\n%s", code, out)
	}
	if _, err := os.Stat(dbPath(dst)); err == nil {
		t.Fatal("a refused restore created a database")
	}
	// --yes: in place, and a sound database with the source's rows.
	code, out = restoreRun(t, dst, fake, "--from-store", "--yes")
	if code != 0 || !strings.Contains(out, "restored:") || strings.Contains(out, "aside:") {
		t.Fatalf("restore --yes: %d\n%s", code, out)
	}
	if insp, err := db.Inspect(context.Background(), dbPath(dst)); err != nil || !insp.OK() || insp.Clips != 3 {
		t.Fatalf("the restored database: %+v %v", insp, err)
	}
	if fi, err := os.Stat(dbPath(dst)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the restored database's mode: %v %v", fi, err)
	}
	// Again over it: the database that was there is kept aside, whole.
	code, out = restoreRun(t, dst, fake, "--from-store", "--yes")
	aside := dbPath(dst) + ".before-restore-20261019T120000Z"
	if code != 0 || !strings.Contains(out, "aside:     "+aside) {
		t.Fatalf("restore over a database: %d\n%s", code, out)
	}
	if insp, err := db.Inspect(context.Background(), aside); err != nil || !insp.OK() {
		t.Fatalf("the aside copy: %+v %v", insp, err)
	}
	// A sidecar that does not match: refused, nothing changed.
	fake.Put(store.SnapshotSumKey(""), []byte(strings.Repeat("0", 64)+"\n"), "text/plain")
	before, _ := os.ReadFile(dbPath(dst))
	code, out = restoreRun(t, dst, fake, "--from-store", "--yes")
	if code != 1 || !strings.Contains(out, "does not match its sha256") {
		t.Fatalf("a bad sha: %d\n%s", code, out)
	}
	if after, _ := os.ReadFile(dbPath(dst)); !bytes.Equal(before, after) {
		t.Fatal("a refused restore changed the database")
	}
	// No snapshot at all, the usage, and a folder home.
	if code, out := restoreRun(t, dst, store.NewFake(), "--from-store"); code != 1 || !strings.Contains(out, "holds no snapshot") {
		t.Fatalf("no snapshot: %d\n%s", code, out)
	}
	if code, out := restoreRun(t, dst, fake); code != 2 || !strings.Contains(out, "usage: clippet admin restore --from-store") {
		t.Fatalf("no --from-store: %d\n%s", code, out)
	}
	// The scratch area is left as it was found.
	if left, _ := os.ReadDir(dst.ScratchDir); len(left) != 0 {
		t.Fatalf("restore left %d file(s) in scratch", len(left))
	}
}

// TestDataLock: shared locks coexist (two servers over one database); an
// exclusive one is refused while any is held, and a shared one while it is.
func TestDataLock(t *testing.T) {
	dir := t.TempDir()
	a, err := lockDataDir(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := lockDataDir(dir, false)
	if err != nil {
		t.Fatalf("a second shared lock: %v", err)
	}
	if _, err := lockDataDir(dir, true); !errors.Is(err, errDataLockHeld) {
		t.Fatalf("exclusive under two shared: %v", err)
	}
	a()
	b()
	x, err := lockDataDir(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockDataDir(dir, false); !errors.Is(err, errDataLockHeld) {
		t.Fatalf("shared under an exclusive: %v", err)
	}
	x()
}

// TestFolderHomeKeepsASnapshot: on `fs` DB_SNAPSHOT is honoured like on a
// bucket — on when empty, off only when it says off (§20 erratum 348) — so
// serve builds the loop for a folder home too.
func TestFolderHomeKeepsASnapshot(t *testing.T) {
	for v, want := range map[string]bool{"": true, "on": true, "off": false} {
		m := map[string]string{"PUBLIC_BASE_URL": "http://localhost:8481", "STORAGE_BACKEND": "fs", "STORAGE_DIR": "/media",
			"DB_SNAPSHOT": v, "DATA_DIR": t.TempDir()}
		cfg, err := config.Load(func(k string) string { return m[k] })
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DBSnapshot != want {
			t.Fatalf("a folder home with DB_SNAPSHOT=%q: snapshot=%v, want %v", v, cfg.DBSnapshot, want)
		}
	}
}

// TestFolderSnapshotRoundTrip is §20 erratum 348 on a REAL folder store: the
// snapshot lands under the media folder's own `_clippet/`, beside — never on
// — the store marker at its root, and `admin restore --from-store` brings the
// database back on a new data directory from the folder alone. Backing up the
// folder is the whole backup.
func TestFolderSnapshotRoundTrip(t *testing.T) {
	media := t.TempDir()
	src := snapshotCfg(t, nil)
	src.StorageBackend, src.StorageDir = config.BackendFS, media
	src.S3 = config.S3Config{OpTimeout: 30 * time.Second}
	st, err := store.NewFS(media, src.S3.OpTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMarker(media); err != nil {
		t.Fatal(err)
	}
	marker, _ := os.ReadFile(filepath.Join(media, store.MarkerName))
	if _, err := newDBSnapshotter(src, migratedDB(t, src, 2), st, quietLog()).writeOnce(context.Background()); err != nil {
		t.Fatalf("writeOnce on a folder store: %v", err)
	}
	for _, k := range []string{store.SnapshotKey(""), store.SnapshotSumKey(""), store.SnapshotMetaKey("")} {
		if fi, err := os.Stat(filepath.Join(media, filepath.FromSlash(k))); err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("%s is not a file under the media folder: %v", k, err)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(media, store.MarkerName)); !bytes.Equal(marker, after) {
		t.Fatal("the snapshot touched the store marker")
	}
	if ok, _ := store.HasMarker(filepath.Join(media, "_clippet")); ok {
		t.Fatal("a marker was written under _clippet/")
	}

	// A new machine: a fresh data directory, the same folder.
	dst := snapshotCfg(t, nil)
	dst.StorageBackend, dst.StorageDir, dst.S3 = config.BackendFS, media, src.S3
	var out, errOut bytes.Buffer
	code := runAdminWith([]string{"restore", "--from-store", "--yes"}, dst, adminEnv{stdout: &out, stderr: &errOut,
		openStore: func(config.Config) (store.Store, error) { return st, nil },
		now:       func() time.Time { return time.Date(2026, 10, 27, 6, 0, 0, 0, time.UTC) }})
	if code != 0 || !strings.Contains(out.String(), "sha256 ok") || !strings.Contains(out.String(), "restored:") {
		t.Fatalf("restore from a folder store: %d\n%s%s", code, out.String(), errOut.String())
	}
	if insp, err := db.Inspect(context.Background(), dbPath(dst)); err != nil || !insp.OK() || insp.Clips != 2 {
		t.Fatalf("the database restored from the folder: %+v %v", insp, err)
	}
}
