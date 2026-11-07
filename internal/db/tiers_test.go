package db

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

// The quality tiers, database half (DESIGN.md §17 #10, §20 errata 200–209).

func fullFields() FullFields {
	return FullFields{Key: "clips/x/renditions/main-full.mp4", Size: 4096, ETag: "fe",
		Width: 2560, Height: 1440, DurationMS: 6000, FPSNum: 60, FPSDen: 1, HasAudio: true, Codec: "h264"}
}

// cardFirstClip is a clip in the state card-first ordering produces: ready on its
// LIGHT copy, with no rendition object and a `full` job queued (§20 erratum 206).
func cardFirstClip(t *testing.T, d *DB) (Clip, Job) {
	t.Helper()
	ctx := context.Background()
	owner := mkUser(t, d, "tierer")
	c, j := mkClip(t, d, owner, 1000, nil, false)
	if _, _, _, err := d.ClaimJob(ctx, 1000); err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	f := ReadyFields{
		PosterKey: "p", PosterSize: 5, PosterETag: "pe", PosterWidth: 1280, PosterHeight: 720,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		Light: SocialFields{Key: "clips/" + c.ID + "/renditions/social-a.mp4", Size: 22, ETag: "le",
			Width: 1280, Height: 720, FPSNum: 30, FPSDen: 1},
		Tiers: TierFields{OriginalWidth: 2560, OriginalHeight: 1440, OriginalFPSNum: 60, OriginalFPSDen: 1,
			OriginalCodec: "hevc", FullState: TierEncoding, LightState: TierReady, QueueFull: true},
	}
	ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, f, false, 1001)
	if err != nil || !ok {
		t.Fatalf("FinalizeTranscode: ok=%v err=%v", ok, err)
	}
	c, err = d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	full, err := d.LatestJobOfKind(ctx, c.ID, KindFull)
	if err != nil {
		t.Fatalf("the ready commit did not queue the full job: %v", err)
	}
	return c, full
}

func TestFinalizeTranscodeWritesTheTiersAndQueuesTheFull(t *testing.T) {
	d := openTest(t)
	c, full := cardFirstClip(t, d)
	switch {
	case c.RenditionKey != nil:
		t.Fatalf("rendition_key = %q; the card-first path publishes none", *c.RenditionKey)
	case c.SocialKey == nil || c.SocialFPSNum == nil || *c.SocialFPSNum != 30:
		t.Fatalf("the light copy's columns are not filled: %+v", c.SocialKey)
	case c.FullState != TierEncoding || c.LightState != TierReady:
		t.Fatalf("tier states = %q/%q, want encoding/ready", c.FullState, c.LightState)
	case c.OriginalWidth == nil || *c.OriginalWidth != 2560 || c.OriginalCodec != "hevc":
		t.Fatalf("the original's own columns are not filled: %v %q", c.OriginalWidth, c.OriginalCodec)
	case full.Kind != KindFull || full.State != JobQueued:
		t.Fatalf("full job = %+v", full)
	}
	// PlayObject falls through to the light copy: it is the only playable file a
	// clip in this state has, and it is what /video.mp4 then serves.
	if key, _, _, ok := PlayObject(c); !ok || key != *c.SocialKey {
		t.Fatalf("PlayObject = %q ok=%v, want the light copy", key, ok)
	}
	// …and the tier states a poller sees say exactly that.
	want := []TierStateRow{{TierNameOriginal, TierReady}, {TierNameFull, TierEncoding}, {TierNameLight, TierReady}}
	got := TierStatesOfClip(c)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tier states = %+v, want %+v", got, want)
		}
	}
}

func TestSetFullPublishesTheFaithfulCopy(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, full := cardFirstClip(t, d)
	if _, _, _, err := d.ClaimMediaJob(ctx, 1002); err != nil {
		t.Fatalf("ClaimMediaJob: %v", err)
	}
	swap, err := d.SetFull(ctx, full.ID, c.ID, fullFields(), c.SourceETag, 1003)
	if err != nil || !swap.OK {
		t.Fatalf("SetFull: %+v %v", swap, err)
	}
	after, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case after.RenditionKey == nil || *after.RenditionKey != fullFields().Key:
		t.Fatalf("rendition_key = %v, want the Full's key", after.RenditionKey)
	case after.Width == nil || *after.Width != 2560 || after.FPSNum == nil || *after.FPSNum != 60:
		t.Fatalf("the clip's measurements are not the Full's: %v x %v @ %v", after.Width, after.Height, after.FPSNum)
	case after.FullState != TierReady || after.FullCodec != "h264":
		t.Fatalf("full_state=%q full_codec=%q, want ready/h264", after.FullState, after.FullCodec)
	case after.SocialKey == nil || *after.SocialKey != *c.SocialKey:
		t.Fatalf("the card file was disturbed: %v -> %v", c.SocialKey, after.SocialKey)
	case after.State != ClipReady:
		t.Fatalf("the clip is %q; SetFull never moves it", after.State)
	}
	// The ORIGINAL's own columns are untouched: a Full landing later must not be
	// able to overwrite the measurements of the file it was made from.
	if after.OriginalWidth == nil || *after.OriginalWidth != 2560 || after.OriginalCodec != "hevc" {
		t.Fatalf("SetFull overwrote the original's columns: %v %q", after.OriginalWidth, after.OriginalCodec)
	}
	if j, err := d.LatestJobOfKind(ctx, c.ID, KindFull); err != nil || j.State != JobDone {
		t.Fatalf("full job = %+v %v, want done", j, err)
	}
}

func TestSetFullRefusesAReplacedOrTrimmedOrUnreadyClip(t *testing.T) {
	ctx := context.Background()
	t.Run("the original changed", func(t *testing.T) {
		d := openTest(t)
		c, full := cardFirstClip(t, d)
		if _, _, _, err := d.ClaimMediaJob(ctx, 1002); err != nil {
			t.Fatal(err)
		}
		swap, err := d.SetFull(ctx, full.ID, c.ID, fullFields(), "some-other-etag", 1003)
		if err != nil {
			t.Fatal(err)
		}
		if swap.OK || swap.Reason != "the original changed" {
			t.Fatalf("SetFull against a different original = %+v", swap)
		}
		after, _ := d.GetClip(ctx, c.ID)
		if after.RenditionKey != nil {
			t.Fatalf("a refused swap published %q", *after.RenditionKey)
		}
	})
	t.Run("the clip was trimmed", func(t *testing.T) {
		d := openTest(t)
		c, full := cardFirstClip(t, d)
		// A trim's own commit: it produces the Full of a WINDOW, so a whole-file
		// Full arriving afterwards would silently un-cut the clip.
		trim, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 3000}), 1002)
		if err != nil {
			t.Fatal(err)
		}
		// BY KIND: the clip already has a queued `full` job, and ClaimMediaJob
		// takes the oldest media job of any kind — which is that one.
		if _, _, ok, err := d.ClaimJobOfKinds(ctx, []string{KindTrim}, 1003); err != nil || !ok {
			t.Fatalf("claim the trim: ok=%v err=%v", ok, err)
		}
		cut := ready()
		cut.Tiers = TrimTierFields(false, 2560, 1440, 60, 1, "hevc", false)
		if _, ok, _, err := d.SwapRendition(ctx, trim.ID, c.ID, cut, TrimParams{StartMS: 0, EndMS: 3000}, 1004); err != nil || !ok {
			t.Fatalf("SwapRendition: ok=%v err=%v", ok, err)
		}
		if _, _, ok, err := d.ClaimJobOfKinds(ctx, []string{KindFull}, 1005); err != nil || !ok {
			t.Fatalf("claim the full job: ok=%v err=%v", ok, err)
		}
		swap, err := d.SetFull(ctx, full.ID, c.ID, fullFields(), c.SourceETag, 1006)
		if err != nil {
			t.Fatal(err)
		}
		if swap.OK || swap.Reason != "the clip was trimmed" {
			t.Fatalf("SetFull over a trimmed clip = %+v", swap)
		}
		after, _ := d.GetClip(ctx, c.ID)
		if after.RenditionKey == nil || *after.RenditionKey != cut.RenditionKey {
			t.Fatalf("the cut was replaced by the whole-file Full: %v", after.RenditionKey)
		}
	})
	t.Run("a tombstoned clip publishes nothing", func(t *testing.T) {
		d := openTest(t)
		c, full := cardFirstClip(t, d)
		if _, _, ok, err := d.ClaimJobOfKinds(ctx, []string{KindFull}, 1002); err != nil || !ok {
			t.Fatalf("claim the full job: ok=%v err=%v", ok, err)
		}
		if err := d.MarkDeleting(ctx, c.ID, 1003); err != nil {
			t.Fatal(err)
		}
		// MarkDeleting cancels the clip's live jobs, so the JOB-state guard is the
		// one this reaches — the clip guard behind it says the same thing. What
		// matters either way is that nothing was published onto a tombstone.
		swap, err := d.SetFull(ctx, full.ID, c.ID, fullFields(), c.SourceETag, 1004)
		if err != nil || swap.OK {
			t.Fatalf("SetFull against a tombstone = %+v %v", swap, err)
		}
		after, err := d.GetClip(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.RenditionKey != nil || after.FullState != TierEncoding {
			t.Fatalf("a refused swap changed the row: rendition=%v full_state=%q", after.RenditionKey, after.FullState)
		}
	})
	t.Run("a transcode job is refused outright", func(t *testing.T) {
		d := openTest(t)
		c, j := readyClip(t, d)
		if _, err := d.SetFull(ctx, j.ID, c.ID, fullFields(), c.SourceETag, 1002); !errors.Is(err, ErrStateMismatch) {
			t.Fatalf("SetFull on a transcode job = %v, want ErrStateMismatch", err)
		}
	})
}

func TestEnqueueFullAdmitsOneAtATime(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	job, err := d.EnqueueFull(ctx, c.ID, 2000)
	if err != nil {
		t.Fatalf("EnqueueFull: %v", err)
	}
	if job.Kind != KindFull || job.State != JobQueued {
		t.Fatalf("queued job = %+v", job)
	}
	if _, err := d.EnqueueFull(ctx, c.ID, 2001); !errors.Is(err, ErrConflict) {
		t.Errorf("second EnqueueFull = %v, want ErrConflict", err)
	}
	// `full` is claimed by the MEDIA loop and by nothing else: it must never be
	// taken by the transcode loop (which would move the clip's state) or by the
	// notify loop (which cannot run ffmpeg). This clip's own transcode job is
	// already `done`, so the only claimable job in the file is the full one.
	if _, _, ok, err := d.ClaimJob(ctx, 2004); err != nil || ok {
		t.Fatalf("the transcode loop claimed a full job: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := d.ClaimNotifyJob(ctx, 2005); err != nil || ok {
		t.Fatalf("the notify loop claimed a full job: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := d.ClaimMediaJob(ctx, 2006); err != nil || !ok {
		t.Fatalf("the media loop did not claim the full job: ok=%v err=%v", ok, err)
	}
	// A clip that is not ready has nothing to add a tier to, and neither has one
	// that does not exist.
	other := mkUser(t, d, "queued-owner")
	q, _ := mkClip(t, d, other, 2000, nil, false)
	if _, err := d.EnqueueFull(ctx, q.ID, 2007); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("EnqueueFull on a queued clip = %v, want ErrStateMismatch", err)
	}
	if _, err := d.EnqueueFull(ctx, "aZ3kQ9xB2mLp", 2008); !errors.Is(err, ErrNotFound) {
		t.Errorf("EnqueueFull on a missing clip = %v, want ErrNotFound", err)
	}
}

func TestSetTierState(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	if err := d.SetTierState(ctx, c.ID, TierNameFull, TierFailed, 2000); err != nil {
		t.Fatalf("SetTierState: %v", err)
	}
	if got, _ := d.GetClip(ctx, c.ID); got.FullState != TierFailed {
		t.Fatalf("full_state = %q, want failed", got.FullState)
	}
	if err := d.SetTierState(ctx, c.ID, TierNameLight, TierSkipped, 2001); err != nil {
		t.Fatalf("SetTierState(light): %v", err)
	}
	if got, _ := d.GetClip(ctx, c.ID); got.LightState != TierSkipped {
		t.Fatalf("light_state = %q, want skipped", got.LightState)
	}
	// It writes nothing else, and it refuses a tier or a state it does not know
	// rather than inventing a column.
	for _, bad := range []struct{ tier, state string }{
		{TierNameOriginal, TierReady}, // the Original has no state column
		{"nonsense", TierReady},
		{TierNameFull, "nonsense"},
	} {
		if err := d.SetTierState(ctx, c.ID, bad.tier, bad.state, 2002); !errors.Is(err, ErrStateMismatch) {
			t.Errorf("SetTierState(%q, %q) = %v, want ErrStateMismatch", bad.tier, bad.state, err)
		}
	}
	// A tombstoned clip is left alone: this is a footnote on a job's ending, and
	// a clip being destroyed has no tiers to describe.
	if err := d.MarkDeleting(ctx, c.ID, 2003); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTierState(ctx, c.ID, TierNameFull, TierReady, 2004); err != nil {
		t.Fatalf("SetTierState on a tombstone: %v", err)
	}
	if got, _ := d.GetClip(ctx, c.ID); got.FullState != TierFailed {
		t.Fatalf("a tombstone's tier state was rewritten to %q", got.FullState)
	}
}

func TestPlayObjectAndOriginalIsTheClip(t *testing.T) {
	rendition, poster := "r", "p"
	base := Clip{ID: "c", State: ClipReady, SourceKey: "s", SourceSize: 100, SourceETag: "se"}
	withRendition := base
	withRendition.RenditionKey, withRendition.RenditionSize, withRendition.RenditionETag = &rendition, i64(20), &poster

	if key, size, etag, ok := PlayObject(withRendition); !ok || key != "r" || size != 20 || etag != "p" {
		t.Fatalf("a rendition wins: %q/%d/%q ok=%v", key, size, etag, ok)
	}
	direct := base
	direct.OriginalPlayable = true
	if key, size, etag, ok := PlayObject(direct); !ok || key != "s" || size != 100 || etag != "se" {
		t.Fatalf("a playable upload is the play object: %q/%d/%q ok=%v", key, size, etag, ok)
	}
	if !OriginalIsTheClip(direct) {
		t.Fatal("OriginalIsTheClip(direct) = false")
	}
	// Each of the three narrowings closes the anonymous door again.
	remuxed := direct
	remuxed.OriginalRemuxed = true
	trimmed := direct
	trimmed.TrimStartMS, trimmed.TrimEndMS = i64(0), i64(1000)
	unready := direct
	unready.State = ClipProcessing
	for name, c := range map[string]Clip{"remuxed": remuxed, "trimmed": trimmed, "not ready": unready} {
		if OriginalIsTheClip(c) {
			t.Errorf("OriginalIsTheClip is true for a %s clip", name)
		}
	}
	// A remuxed clip with no rendition object yet has no playable bytes at all —
	// which on a ready row is an invariant violation and on any other state is
	// simply "not yet".
	if _, _, _, ok := PlayObject(remuxed); ok {
		t.Error("PlayObject found playable bytes for a remuxed clip with no rendition")
	}
	if _, _, _, ok := PlayObject(base); ok {
		t.Error("PlayObject found playable bytes for a clip that has none")
	}
	// The light copy is the last resort: a clip waiting for its Full.
	light, lightETag := "l", "le"
	waiting := base
	waiting.SocialKey, waiting.SocialSize, waiting.SocialETag = &light, i64(9), &lightETag
	if key, _, _, ok := PlayObject(waiting); !ok || key != "l" {
		t.Fatalf("PlayObject = %q ok=%v, want the light copy", key, ok)
	}
}

func TestReconcileTierState(t *testing.T) {
	// The OBJECT wins: a file in the bucket is not "encoding" whatever a column
	// says, which is what makes a row migrated from before 0012 read correctly.
	for _, stored := range []string{TierUnknown, TierSkipped, TierEncoding, TierFailed, TierReady} {
		if got := ReconcileTierState(stored, true); got != TierReady {
			t.Errorf("ReconcileTierState(%q, present) = %q, want ready", stored, got)
		}
	}
	// With no object the stored word stands, and an unknown one reads as skipped:
	// the honest answer to "is a better copy coming" is no.
	for stored, want := range map[string]string{
		TierUnknown: TierSkipped, TierSkipped: TierSkipped,
		TierEncoding: TierEncoding, TierFailed: TierFailed, TierReady: TierSkipped,
	} {
		if got := ReconcileTierState(stored, false); got != want {
			t.Errorf("ReconcileTierState(%q, absent) = %q, want %q", stored, got, want)
		}
	}
}

// ReconcileClipTier adds the difference between "not needed" and "not decided
// yet" (§20 erratum 205, amended): an unknown column on a clip that is still
// queued or processing is `pending`, because `skipped` means "a finished answer,
// not a gap" everywhere else in this API and a client that believed it would stop
// waiting for a tier that is on its way.
func TestReconcileClipTierPendingOnlyWhereNothingIsDecided(t *testing.T) {
	for _, state := range []string{ClipQueued, ClipProcessing} {
		if got := ReconcileClipTier(state, TierUnknown, false); got != TierPending {
			t.Errorf("ReconcileClipTier(%q, unknown, absent) = %q, want pending", state, got)
		}
		// A stored word is a decision, and an OBJECT is a fact: neither becomes
		// pending merely because the clip has not flipped yet (a `full` job's clip is
		// ready, but a re-run transcode's is not).
		if got := ReconcileClipTier(state, TierEncoding, false); got != TierEncoding {
			t.Errorf("ReconcileClipTier(%q, encoding, absent) = %q", state, got)
		}
		if got := ReconcileClipTier(state, TierUnknown, true); got != TierReady {
			t.Errorf("ReconcileClipTier(%q, unknown, present) = %q", state, got)
		}
	}
	// Every other clip state keeps ReconcileTierState's answer. A FAILED clip is
	// deliberately not pending: nothing is coming for it either.
	for _, state := range []string{ClipReady, ClipFailed, ClipDeleting} {
		if got := ReconcileClipTier(state, TierUnknown, false); got != TierSkipped {
			t.Errorf("ReconcileClipTier(%q, unknown, absent) = %q, want skipped", state, got)
		}
	}
	// TierStatesOf carries it through, and the Original is `ready` whatever the
	// clip's state: its bytes were in the bucket before the 201.
	got := TierStatesOf(ClipProcessing, TierUnknown, TierUnknown, false, false)
	want := []TierStateRow{{TierNameOriginal, TierReady}, {TierNameFull, TierPending}, {TierNameLight, TierPending}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TierStatesOf of a processing clip = %+v, want %+v", got, want)
		}
	}
	// …and `pending` is never STORED: SetTierState refuses it, because it is
	// derived from the clip's own state and nothing else.
	d := openTest(t)
	c, _ := readyClip(t, d)
	if err := d.SetTierState(context.Background(), c.ID, TierNameFull, TierPending, 2000); err == nil {
		t.Fatal("SetTierState stored `pending`; it is a derived word, not a column value")
	}
}

func TestMigration0012BackfillsAPopulatedDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "clippet.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	// Migrate to 0011 ONLY — the schema a running instance has before this bundle
	// — and populate it with raw SQL. No Go struct is used for the writes on
	// purpose: scanClip already knows 0012's column list, so anything that went
	// through it would be testing this binary against itself rather than an
	// upgrade.
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	applied := 0
	for _, m := range files {
		if m.version >= 12 {
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		applied++
	}
	if applied == 0 {
		t.Fatal("no pre-0012 migrations were applied; this test would prove nothing")
	}
	if _, err := d.exec(ctx,
		"INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1', 'legacy', 'h', 1000)"); err != nil {
		t.Fatal(err)
	}
	// Three rows, each a different thing 0012 has to say something about: a ready
	// clip WITH a card file, a ready clip without one, and a clip still queued.
	insert := func(id, state string, rendition, social bool) {
		t.Helper()
		cols := "id, owner_id, title, description, unlisted, state, source_key, source_filename, source_size," +
			" source_content_type, source_etag, views, created_at, updated_at"
		vals := "?, 'u1', 't', 'd', 0, ?, 'clips/' || ? || '/original/source.mp4', 'clip.mp4', 123," +
			" 'video/mp4', 'etag-' || ?, 0, 1000, 1000"
		if rendition {
			cols += ", rendition_key, rendition_size, rendition_etag, poster_key, poster_size, poster_etag"
			vals += ", 'r', 10, 're', 'p', 5, 'pe'"
		}
		if social {
			cols += ", social_key, social_size, social_etag"
			vals += ", 'sk', 9, 'se2'"
		}
		if _, err := d.exec(ctx, "INSERT INTO clips ("+cols+") VALUES ("+vals+")", id, state, id, id); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("withcard0001", ClipReady, true, true)
	insert("plainready01", ClipReady, true, false)
	insert("stillqueued1", ClipQueued, false, false)

	// Now migrate, exactly as a running instance would on restart. 0012 is the
	// migration under test and the first one this database has not seen; the
	// numbers other lanes of the same run added (0013's sweep-failure table,
	// 0014's filmstrip columns) ride along behind it, additively, and the
	// backfill asserted below is 0012's alone. So the assertion is that 0012 IS
	// applied — not that it is the highest number in the directory, which every
	// later feature would break.
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	versions, err := d.AppliedVersions(ctx)
	if err != nil || !slices.Contains(versions, 12) {
		t.Fatalf("applied versions = %v (%v), want 0012 among them", versions, err)
	}

	// A ready row's rendition IS its Full tier: it was encoded by libx264 from a
	// source nobody asked about playability.
	gotCard, err := d.GetClip(ctx, "withcard0001")
	if err != nil {
		t.Fatalf("a legacy row does not read back after 0012: %v", err)
	}
	if gotCard.FullState != TierReady || gotCard.FullCodec != "h264" || gotCard.LightState != TierReady {
		t.Fatalf("a legacy ready clip WITH a card file backfilled to full=%q/%q light=%q",
			gotCard.FullState, gotCard.FullCodec, gotCard.LightState)
	}
	gotPlain, err := d.GetClip(ctx, "plainready01")
	if err != nil {
		t.Fatal(err)
	}
	if gotPlain.FullState != TierReady || gotPlain.LightState != TierSkipped {
		t.Fatalf("a legacy ready clip with NO card file backfilled to full=%q light=%q",
			gotPlain.FullState, gotPlain.LightState)
	}
	// A clip that is still queued has no tiers yet: its run decides them.
	gotQueued, err := d.GetClip(ctx, "stillqueued1")
	if err != nil {
		t.Fatal(err)
	}
	if gotQueued.FullState != TierUnknown || gotQueued.LightState != TierUnknown {
		t.Fatalf("a queued clip was given tier states: full=%q light=%q", gotQueued.FullState, gotQueued.LightState)
	}
	// original_playable is NOT guessed: nobody probed these uploads for it, and
	// telling a viewer an old upload is streamable when nobody checked is worse
	// than saying nothing.
	for _, c := range []Clip{gotCard, gotPlain, gotQueued} {
		if c.OriginalPlayable || c.OriginalRemuxed || c.OriginalWidth != nil || c.OriginalCodec != "" {
			t.Fatalf("0012 invented facts about a legacy upload: %+v", c)
		}
	}
	// Every pre-existing column survived, and the tier view of the row is honest.
	if gotCard.RenditionKey == nil || *gotCard.RenditionKey != "r" || gotCard.SourceSize != 123 {
		t.Fatalf("a legacy row lost data across 0012: %+v", gotCard)
	}
	if got := TierStatesOfClip(gotCard); got[1].State != TierReady || got[2].State != TierReady {
		t.Fatalf("tier states of a legacy row = %+v", got)
	}
	// StorageUsed counts the card file since erratum 209: three sources at 123,
	// two rendition+poster pairs at 15, and one 9-byte card copy.
	used, err := d.StorageUsed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(123*3 + 15*2 + 9); used != want {
		t.Fatalf("StorageUsed = %d, want %d (the light copy is in the sum)", used, want)
	}
	// The OWNER's own tally counts it too (N102/N148). The library header's
	// "Stored" and each clip's own "Stored" line are the same arithmetic, or a
	// person reading both is told two different things about one bucket.
	st, err := d.AccountStatsOf(ctx, "u1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if st.BytesSource != 123*3 || st.BytesRenditions != int64(15*2+9) {
		t.Fatalf("AccountStatsOf = source %d / produced %d, want %d / %d (the light copy is in the sum)",
			st.BytesSource, st.BytesRenditions, 123*3, 15*2+9)
	}
	// The migration is idempotent, as Migrate's contract requires.
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate: %v", err)
	}
}

func TestTrimTierFields(t *testing.T) {
	// A cut is the FULL tier whatever the upload was, and it carries the
	// original's own measurements through from the trim's re-probe.
	got := TrimTierFields(true, 2560, 1440, 60, 1, "h264", true)
	switch {
	case got.FullState != TierReady || got.FullCodec != TierCodecH264:
		t.Fatalf("full = %q/%q, want ready/h264", got.FullState, got.FullCodec)
	case got.LightState != TierEncoding:
		t.Fatalf("light_state = %q, want encoding (the commit queued one)", got.LightState)
	case !got.OriginalPlayable || got.OriginalRemuxed:
		t.Fatalf("original playable=%v remuxed=%v, want true/false — a cut supersedes any remux",
			got.OriginalPlayable, got.OriginalRemuxed)
	case got.OriginalWidth != 2560 || got.OriginalFPSNum != 60 || got.OriginalCodec != "h264":
		t.Fatalf("the original's columns did not come through: %+v", got)
	}
	if got := TrimTierFields(false, 1280, 720, 30, 1, "hevc", false); got.LightState != TierSkipped {
		t.Fatalf("light_state = %q with no light queued, want skipped", got.LightState)
	}
}

// TestRepairedTierJobSettlesItsTierState is §20 erratum 236: a `social` or `full`
// job ended by a REPAIR — the automatic JOBS_UNSTICK_AFTER loop, the boot
// reconcile, or `clippet admin jobs unstick` — writes the tier column in the same
// transaction as its own `failed`.
//
// Before it, only the worker's exit path (worker/social.go, worker/full.go) wrote
// it, and none of those three endings runs one: the job stopped, and
// `light_state`/`full_state` read `encoding` for ever. Every reporter of a tier
// state repeated that — the DTO, both progress routes — and web.tiersLive put a
// `data-progress-url` on a READY page, so app.js polled /progress for twenty
// minutes on every single view of that clip, indefinitely.
func TestRepairedTierJobSettlesItsTierState(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		kind, column string
		attempts     int
		read         func(Clip) string
	}{
		{KindSocial, "light_state", SocialMaxAttempts, func(c Clip) string { return c.LightState }},
		{KindFull, "full_state", FullMaxAttempts, func(c Clip) string { return c.FullState }},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			d := openTest(t)
			c, _ := readyClip(t, d)
			// The clip says what a card-first commit says: this tier is coming.
			if err := d.SetTierState(ctx, c.ID, tierOfKind(tc.kind), TierEncoding, 2000); err != nil {
				t.Fatal(err)
			}
			now := int64(100000)
			j := enqueueKind(t, d, c.ID, tc.kind, now-10000)
			// Repair it once per attempt in its budget. Every repair but the last
			// requeues, and a requeued job's tier really is still `encoding`.
			for i := 1; i <= tc.attempts; i++ {
				markRunning(t, d, j.ID, now-9000)
				res, err := d.UnstickJob(ctx, j.ID, now-1000, Limits{}, now)
				if err != nil {
					t.Fatalf("repair %d: %v", i, err)
				}
				got, err := d.GetClip(ctx, c.ID)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case i < tc.attempts:
					if res.Action != UnstickRequeued {
						t.Fatalf("repair %d = %q, want a requeue inside the %d-attempt budget", i, res.Action, tc.attempts)
					}
					if tc.read(got) != TierEncoding {
						t.Fatalf("repair %d set %s = %q; a requeued job's tier IS still encoding", i, tc.column, tc.read(got))
					}
				default:
					if res.Action != UnstickFailed {
						t.Fatalf("repair %d = %q, want the cap to fail the job", i, res.Action)
					}
					if tc.read(got) != TierFailed {
						t.Fatalf("%s = %q after the last repair, want failed — it would otherwise read `encoding` for ever",
							tc.column, tc.read(got))
					}
					// The CLIP is untouched: a lost tier is not a lost clip (§5).
					if got.State != ClipReady || got.FailureReason != nil {
						t.Fatalf("the clip is %q/%v; only a transcode job may move it", got.State, got.FailureReason)
					}
				}
			}
		})
	}
}

// TestRepairedNonTierJobTouchesNoTierColumn is the other half: the mapping is from
// the KIND, so a kind that produces no tier writes nothing — and a `transcode` at
// its cap settles the CLIP (which it always did) and not a tier column, because a
// transcode's tiers are settled by TierFields when it succeeds.
func TestRepairedNonTierJobTouchesNoTierColumn(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	if err := d.SetTierState(ctx, c.ID, TierNameFull, TierEncoding, 2000); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTierState(ctx, c.ID, TierNameLight, TierEncoding, 2000); err != nil {
		t.Fatal(err)
	}
	now := int64(100000)
	for _, kind := range []string{KindTrim, KindGIF, KindPoster, KindFilmstrip, KindReplace, KindNotify} {
		j := enqueueKind(t, d, c.ID, kind, now-10000)
		for i := 0; i < 12; i++ {
			if jobState(t, d, j.ID) == JobFailed {
				break
			}
			markRunning(t, d, j.ID, now-9000)
			if _, err := d.UnstickJob(ctx, j.ID, now-1000, Limits{}, now); err != nil {
				t.Fatalf("%s: repair: %v", kind, err)
			}
		}
		if got := jobState(t, d, j.ID); got != JobFailed {
			t.Fatalf("%s: the job is %q after its budget; this test needs it failed", kind, got)
		}
		got, err := d.GetClip(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.FullState != TierEncoding || got.LightState != TierEncoding {
			t.Fatalf("a failed %s job wrote a tier column: full=%q light=%q", kind, got.FullState, got.LightState)
		}
	}
	if got := tierOfKind(KindTranscode); got != "" {
		t.Fatalf("tierOfKind(transcode) = %q, want no tier", got)
	}
}

// storedTierOf reads the column a job of this kind settles, so the table below can
// name a kind and nothing else.
func storedTierOf(c Clip, kind string) string {
	if tierOfKind(kind) == TierNameFull {
		return c.FullState
	}
	return c.LightState
}

// TestSettleTierEncodingOnlyOverwritesAPromise is §20 erratum 237's guard, as a
// table.
//
// `encoding` is a PROMISE that a job of this kind is coming. The only write
// entitled to replace it is the ending that makes the promise false — and an
// ending is only that ending when nothing of the kind is left to keep it. Every
// row here is a way the unguarded write went wrong: it buried a `ready` column
// whose key is set (a trim published a Full while a stale one was still queued),
// it re-answered a tier that had already been settled, and it told the selector
// that the REPLACEMENT job a trim or a replacement had just queued was not coming
// — which, for the card's own copy, releases erratum 235's `CardReady` belt while
// the file really is still being encoded.
func TestSettleTierEncodingOnlyOverwritesAPromise(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                string
		kind, stored        string
		liveKind, liveState string
		write, want         string
	}{
		{"a promised card copy that stopped", KindSocial, TierEncoding, "", "", TierSkipped, TierSkipped},
		{"a promised Full that failed", KindFull, TierEncoding, "", "", TierFailed, TierFailed},
		{"a published Full is a fact, not a promise", KindFull, TierReady, "", "", TierFailed, TierReady},
		{"a published card copy likewise", KindSocial, TierReady, "", "", TierSkipped, TierReady},
		{"a tier already settled is not re-answered", KindSocial, TierSkipped, "", "", TierFailed, TierSkipped},
		{"…`failed` included", KindFull, TierFailed, "", "", TierSkipped, TierFailed},
		{"a tier nobody promised is not settled either", KindSocial, TierUnknown, "", "", TierSkipped, TierUnknown},
		{"a replacement job is queued: the promise stands", KindSocial, TierEncoding, KindSocial, JobQueued, TierSkipped, TierEncoding},
		{"…or is already running", KindFull, TierEncoding, KindFull, JobRunning, TierSkipped, TierEncoding},
		{"another kind's job is not this tier's promise", KindSocial, TierEncoding, KindGIF, JobQueued, TierSkipped, TierSkipped},
		{"nor is the other tier's", KindFull, TierEncoding, KindSocial, JobQueued, TierSkipped, TierSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTest(t)
			c, _ := readyClip(t, d)
			if tc.stored != TierUnknown {
				if err := d.SetTierState(ctx, c.ID, tierOfKind(tc.kind), tc.stored, 2000); err != nil {
					t.Fatal(err)
				}
			}
			if tc.liveKind != "" {
				j := enqueueKind(t, d, c.ID, tc.liveKind, 2000)
				if tc.liveState == JobRunning {
					markRunning(t, d, j.ID, 2000)
				}
			}
			if err := d.SettleTierEncoding(ctx, c.ID, tc.kind, tc.write, 2001); err != nil {
				t.Fatalf("SettleTierEncoding: %v", err)
			}
			got, err := d.GetClip(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if state := storedTierOf(got, tc.kind); state != tc.want {
				t.Fatalf("the %s column is %q, want %q", tierOfKind(tc.kind), state, tc.want)
			}
			// Whatever it did or refused to do, it wrote nothing else.
			if got.State != ClipReady || got.FailureReason != nil || got.RenditionKey == nil {
				t.Fatalf("a tier footnote changed the clip: state=%q reason=%v rendition=%v",
					got.State, got.FailureReason, got.RenditionKey)
			}
		})
	}

	// It is keyed on the job KIND, and it writes only the two words an ending can
	// mean. A kind that owns no tier and a state that is not an ending are refused
	// rather than mapped to a column or a word this function guessed.
	d := openTest(t)
	c, _ := readyClip(t, d)
	for _, bad := range []struct{ kind, state string }{
		{KindTranscode, TierFailed},
		{KindGIF, TierSkipped},
		{"nonsense", TierSkipped},
		{KindSocial, TierEncoding}, // only a commit promises a tier
		{KindSocial, TierReady},    // only SetSocial/SetFull publish one
		{KindFull, TierPending},    // derived, never stored
		{KindFull, "nonsense"},
	} {
		if err := d.SettleTierEncoding(ctx, c.ID, bad.kind, bad.state, 2002); !errors.Is(err, ErrStateMismatch) {
			t.Errorf("SettleTierEncoding(%q, %q) = %v, want ErrStateMismatch", bad.kind, bad.state, err)
		}
	}
	// A tombstoned clip is left alone, exactly as SetTierState leaves it: a clip
	// being destroyed has no tiers to describe.
	if err := d.SetTierState(ctx, c.ID, TierNameLight, TierEncoding, 2003); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkDeleting(ctx, c.ID, 2004); err != nil {
		t.Fatal(err)
	}
	if err := d.SettleTierEncoding(ctx, c.ID, KindSocial, TierSkipped, 2005); err != nil {
		t.Fatalf("SettleTierEncoding on a tombstone: %v", err)
	}
	if got, _ := d.GetClip(ctx, c.ID); got.LightState != TierEncoding {
		t.Fatalf("a tombstone's tier state was rewritten to %q", got.LightState)
	}
}

// TestRepairedTierJobLeavesAPublishedTierAlone is the same guard on the REPAIR's
// own write (internal/db/jobs.go, erratum 236's statement).
//
// The shape is a stale `full` job — claimed, then its worker lost, so a repair
// ends it — on a clip whose Full was published meanwhile by a TRIM (`full_state =
// ready`, `rendition_key` set to the cut). Unguarded, the repair wrote `failed`
// over that, and the quality selector then refused to offer a file the bucket
// holds, for ever.
func TestRepairedTierJobLeavesAPublishedTierAlone(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	c, _ := readyClip(t, d)
	if err := d.SetTierState(ctx, c.ID, TierNameFull, TierReady, 2000); err != nil {
		t.Fatal(err)
	}
	now := int64(100000)
	j := enqueueKind(t, d, c.ID, KindFull, now-10000)
	for i := 1; i <= FullMaxAttempts; i++ {
		markRunning(t, d, j.ID, now-9000)
		if _, err := d.UnstickJob(ctx, j.ID, now-1000, Limits{}, now); err != nil {
			t.Fatalf("repair %d: %v", i, err)
		}
	}
	if got := jobState(t, d, j.ID); got != JobFailed {
		t.Fatalf("the job is %q after its budget; this test needs it failed", got)
	}
	got, err := d.GetClip(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FullState != TierReady {
		t.Fatalf("full_state = %q; a repair buried a tier the bucket holds", got.FullState)
	}
}

// TestCardReadyWaitsForAnEncodingLightCopy is §20 erratum 235's belt. A clip whose
// card copy is still `encoding` has a card file COMING that is not the one
// CardObject resolves to — the fallback is the Full, or the cut a trim just made,
// which is too big, too smooth or too large a picture for a crawler, which is why
// the tier was planned at all. Answering "card ready" there hands over precisely
// the file the tier exists to replace, and Discord remembers it per URL.
func TestCardReadyWaitsForAnEncodingLightCopy(t *testing.T) {
	poster, rendition, etag := "p", "r", "e"
	base := Clip{ID: "c", State: ClipReady, SourceKey: "s", SourceSize: 100, SourceETag: "se",
		PosterKey: &poster, RenditionKey: &rendition, RenditionSize: i64(20), RenditionETag: &etag}
	if !CardReady(base) {
		t.Fatal("premise: a ready clip with a poster and a rendition is card-ready")
	}
	for _, state := range []string{TierUnknown, TierSkipped, TierReady, TierFailed} {
		c := base
		c.LightState = state
		if !CardReady(c) {
			t.Errorf("light_state = %q must not hold the card back", state)
		}
	}
	encoding := base
	encoding.LightState = TierEncoding
	if CardReady(encoding) {
		t.Fatal("CardReady is true while the card's own copy is still encoding")
	}
	if !CardPending(encoding) {
		t.Fatal("CardPending is false for a clip whose card copy is still coming")
	}
	// A FULL still encoding is NOT a reason to wait: the card is already the Light
	// copy, and the Full is a better tier for the PLAYER (§20 erratum 217's window
	// no longer exists, and erratum 205's state is reported on its own).
	full := base
	full.FullState = TierEncoding
	if !CardReady(full) {
		t.Fatal("an encoding Full held the card back; only the card's own copy does")
	}
	// And a FAILED clip is never pending, whatever its tier columns say: its
	// link-out card is the card it will have for ever (§20 erratum 213).
	failed := encoding
	failed.State = ClipFailed
	if CardPending(failed) {
		t.Fatal("a failed clip was held")
	}
}
