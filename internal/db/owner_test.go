package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// --- per-account upload defaults (ROADMAP #22, migration 0011) --------------

// TestUserDefaultsThreeStates: the column keeps "no preference", "keep
// forever" and "N days" apart, which is the whole reason it stores wire days
// and not seconds.
func TestUserDefaultsThreeStates(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "prefs")

	got, err := d.GetUserDefaults(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserDefaults: %v", err)
	}
	if got.TTLDays != nil || got.Unlisted {
		t.Fatalf("a fresh account already has preferences: %+v", got)
	}
	// The link grammar has no absent state: migration 0015's column is NOT NULL
	// DEFAULT 'compact', so a fresh account reads back the shipped default.
	if got.LinkStyle != LinkStyleCompact || got.LinkStyleOr() != LinkStyleCompact {
		t.Fatalf("a fresh account's link style is %q, want %q", got.LinkStyle, LinkStyleCompact)
	}

	for _, want := range []UserDefaults{
		{TTLDays: intp(0), Unlisted: true, LinkStyle: LinkStyleMemorable}, // chosen keep-forever
		{TTLDays: intp(7), LinkStyle: LinkStyleCompact},                   // seven days
		{TTLDays: nil, Unlisted: true, LinkStyle: LinkStyleMemorable},     // back to "follow the instance"
		{TTLDays: intp(90), Unlisted: false, LinkStyle: LinkStyleCompact},
		{}, // everything cleared: the style falls back to compact, never to ""
	} {
		if err := d.SetUserDefaults(ctx, u.ID, want); err != nil {
			t.Fatalf("SetUserDefaults(%+v): %v", want, err)
		}
		got, err := d.GetUserDefaults(ctx, u.ID)
		if err != nil {
			t.Fatalf("GetUserDefaults: %v", err)
		}
		switch {
		case (want.TTLDays == nil) != (got.TTLDays == nil):
			t.Fatalf("ttl nil-ness lost: wrote %+v read %+v", want, got)
		case want.TTLDays != nil && *want.TTLDays != *got.TTLDays:
			t.Fatalf("ttl = %d, want %d", *got.TTLDays, *want.TTLDays)
		case want.Unlisted != got.Unlisted:
			t.Fatalf("unlisted = %v, want %v", got.Unlisted, want.Unlisted)
		case got.LinkStyle != want.LinkStyleOr():
			t.Fatalf("link style = %q, want %q", got.LinkStyle, want.LinkStyleOr())
		}
	}
}

// TestValidLinkStyle: the closed set, in the one place both the handler and
// the minter read it from (§20 erratum 250).
func TestValidLinkStyle(t *testing.T) {
	for _, s := range []string{LinkStyleCompact, LinkStyleMemorable} {
		if !ValidLinkStyle(s) {
			t.Errorf("ValidLinkStyle(%q) = false", s)
		}
	}
	for _, s := range []string{"", "Compact", "MEMORABLE", "words", "compact ", "memorable\n"} {
		if ValidLinkStyle(s) {
			t.Errorf("ValidLinkStyle(%q) = true", s)
		}
	}
	if got := (UserDefaults{LinkStyle: "words"}).LinkStyleOr(); got != LinkStyleCompact {
		t.Errorf("LinkStyleOr() of a hand-edited column = %q, want %q", got, LinkStyleCompact)
	}
}

// TestMigrate0015UpgradesAPopulatedDatabase is the upgrade path a running
// install takes: a database that has never seen 0015 — with an account in it —
// gains the column, and that account reads back as `compact`, which is the
// grammar every clip it already has was minted in.
func TestMigrate0015UpgradesAPopulatedDatabase(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	// Everything EXCEPT 0015, found by version rather than by position (the
	// convention every migration test here follows: a later file lands in that
	// directory every time a feature needs a column).
	const linkStyleVersion = 15
	skipped := 0
	for _, m := range files {
		if m.version == linkStyleVersion {
			skipped++
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if skipped != 1 {
		t.Fatalf("skipped %d migrations, want exactly 0015", skipped)
	}
	if _, err := d.rw.ExecContext(ctx,
		"INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1', 'upgrader', 'h', 1000)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetUserDefaults(ctx, "u1"); err == nil {
		t.Fatal("GetUserDefaults succeeded before 0015; the column is not in its SELECT")
	}
	applySkipped(t, d, 15)
	got, err := d.GetUserDefaults(ctx, "u1")
	if err != nil {
		t.Fatalf("GetUserDefaults after the upgrade: %v", err)
	}
	if got.LinkStyle != LinkStyleCompact {
		t.Errorf("an upgraded account's link style is %q, want %q", got.LinkStyle, LinkStyleCompact)
	}
	if got.TTLDays != nil || got.Unlisted {
		t.Errorf("the upgrade disturbed the other defaults: %+v", got)
	}
}

// TestUserDefaultsOfAMissingUser: never the zero value by accident.
func TestUserDefaultsOfAMissingUser(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	if _, err := d.GetUserDefaults(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUserDefaults(unknown) = %v, want ErrNotFound", err)
	}
	if err := d.SetUserDefaults(ctx, "nobody", UserDefaults{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetUserDefaults(unknown) = %v, want ErrNotFound", err)
	}
}

func intp(n int) *int { return &n }

// --- the owner's own tally and the instance total (ROADMAP #23, #24) --------

// TestAccountStatsCountTheCallersOwnRows: one account's numbers never include
// another's, a tombstone counts nowhere, and the derived bytes are the ones
// the clip actually has.
func TestAccountStatsAreTheCallersOwn(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	mine := mkUser(t, d, "mine")
	theirs := mkUser(t, d, "theirs")

	// One ready clip of mine (10+5 derived bytes on top of the 123-byte
	// source that mkClip writes), one queued, one expiring inside the window,
	// one tombstone, and one of somebody else's.
	c1, j1 := mkClip(t, d, mine, 1000, nil, false)
	if _, _, _, err := d.ClaimJob(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := d.FinalizeTranscode(ctx, j1.ID, c1.ID, ready(), false, 1001); err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	mkClip(t, d, mine, 1000, nil, false)                     // queued
	soon, _ := mkClip(t, d, mine, 1000, i64(3*86400), false) // expires in three days
	gone, _ := mkClip(t, d, mine, 1000, nil, false)
	if err := d.MarkDeleting(ctx, gone.ID, 1002); err != nil {
		t.Fatal(err)
	}
	mkClip(t, d, theirs, 1000, nil, false)
	if err := d.IncrementViews(ctx, c1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateCollection(ctx, "CoLLeCtIoN1", mine.ID, "mine", 1000); err != nil {
		t.Fatal(err)
	}

	st, err := d.AccountStatsOf(ctx, mine.ID, 1000)
	if err != nil {
		t.Fatalf("AccountStatsOf: %v", err)
	}
	if st.Clips != 3 {
		t.Errorf("clips = %d, want 3 (the tombstone and the stranger's are not mine)", st.Clips)
	}
	if st.Ready != 1 {
		t.Errorf("ready = %d, want 1", st.Ready)
	}
	if st.BytesSource != 3*123 {
		t.Errorf("bytes_source = %d, want %d", st.BytesSource, 3*123)
	}
	if st.BytesRenditions != 15 {
		t.Errorf("bytes_renditions = %d, want 15 (rendition 10 + poster 5)", st.BytesRenditions)
	}
	if st.ViewsTotal != 1 {
		t.Errorf("views_total = %d, want 1", st.ViewsTotal)
	}
	if st.Expiring7d != 1 {
		t.Errorf("expiring_7d = %d, want 1 (%s)", st.Expiring7d, soon.ID)
	}
	if st.CollectionsCount != 1 {
		t.Errorf("collections = %d, want 1", st.CollectionsCount)
	}

	// The other account sees only its own, and an account with nothing sees
	// zeros rather than an error.
	other, err := d.AccountStatsOf(ctx, theirs.ID, 1000)
	if err != nil || other.Clips != 1 || other.ViewsTotal != 0 {
		t.Errorf("the stranger's tally = %+v (%v)", other, err)
	}
	empty, err := d.AccountStatsOf(ctx, "nobody", 1000)
	if err != nil || empty.Clips != 0 || empty.BytesSource != 0 {
		t.Errorf("an unknown account's tally = %+v (%v)", empty, err)
	}
	if empty.CollectionsCount != 0 {
		t.Errorf("an unknown account has %d collections", empty.CollectionsCount)
	}
	if other.CollectionsCount != 0 {
		t.Errorf("the stranger was given somebody else's collections: %d", other.CollectionsCount)
	}
}

// The collections tally rides inside the clip aggregate as a scalar subquery
// (N065). The case that shape could get wrong is an account with COLLECTIONS
// and no clips: the outer query matches no rows, and a count folded into the
// aggregate rather than fetched beside it still has to come back. It does — an
// aggregate with no GROUP BY returns exactly one row whatever the WHERE
// matched — and this pins it, together with the reverse.
func TestAccountStatsCountCollectionsWithoutClips(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	lister := mkUser(t, d, "lister")
	uploader := mkUser(t, d, "uploader")

	for _, id := range []string{"CoLLeCtIoNa", "CoLLeCtIoNb", "CoLLeCtIoNc"} {
		if _, err := d.CreateCollection(ctx, id, lister.ID, "a list", 1000); err != nil {
			t.Fatal(err)
		}
	}
	mkClip(t, d, uploader, 1000, nil, false)

	st, err := d.AccountStatsOf(ctx, lister.ID, 1000)
	if err != nil {
		t.Fatalf("AccountStatsOf: %v", err)
	}
	if st.Clips != 0 || st.CollectionsCount != 3 {
		t.Errorf("collections without clips: clips=%d collections=%d, want 0 and 3", st.Clips, st.CollectionsCount)
	}
	st, err = d.AccountStatsOf(ctx, uploader.ID, 1000)
	if err != nil {
		t.Fatalf("AccountStatsOf: %v", err)
	}
	if st.Clips != 1 || st.CollectionsCount != 0 {
		t.Errorf("clips without collections: clips=%d collections=%d, want 1 and 0", st.Clips, st.CollectionsCount)
	}
}

// TestStorageUsedIsEverybodysBytesMinusTheTombstones.
func TestStorageUsedIsEverybodysBytes(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	a := mkUser(t, d, "a")
	b := mkUser(t, d, "b")

	used, err := d.StorageUsed(ctx)
	if err != nil || used != 0 {
		t.Fatalf("an empty instance stores %d bytes (%v)", used, err)
	}
	c1, j1 := mkClip(t, d, a, 1000, nil, false)
	if _, _, _, err := d.ClaimJob(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := d.FinalizeTranscode(ctx, j1.ID, c1.ID, ready(), false, 1001); err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	mkClip(t, d, b, 1000, nil, false)
	used, err = d.StorageUsed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Both sources (123 each) plus the ready one's rendition and poster.
	if want := int64(2*123 + 15); used != want {
		t.Errorf("StorageUsed = %d, want %d", used, want)
	}
	// A tombstone is already gone everywhere else, so it is not counted here.
	if err := d.MarkDeleting(ctx, c1.ID, 1002); err != nil {
		t.Fatal(err)
	}
	used, err = d.StorageUsed(ctx)
	if err != nil || used != 123 {
		t.Errorf("StorageUsed after a tombstone = %d (%v), want 123", used, err)
	}
}

// --- one clip at a time (ROADMAP #17) --------------------------------------

// TestRemoveClipFromCollection: idempotent in the CLIP, never in the
// COLLECTION, and it touches no clip.
func TestRemoveClipFromCollection(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	owner := mkUser(t, d, "curator")
	stranger := mkUser(t, d, "stranger")
	c1, _ := mkClip(t, d, owner, 1000, nil, false)
	c2, _ := mkClip(t, d, owner, 1000, nil, false)
	col, err := d.CreateCollection(ctx, "CoLLeCtIoN1", owner.ID, "best of", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetCollectionClips(ctx, col.ID, owner.ID, []string{c1.ID, c2.ID}, 1000); err != nil {
		t.Fatal(err)
	}

	if err := d.RemoveClipFromCollection(ctx, col.ID, owner.ID, c1.ID, 1001); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	members, err := d.CollectionClips(ctx, col.ID)
	if err != nil || len(members) != 1 || members[0].ID != c2.ID {
		t.Fatalf("members after the remove = %+v (%v)", members, err)
	}
	// The clip itself is untouched.
	if _, err := d.GetClip(ctx, c1.ID); err != nil {
		t.Errorf("removing a clip from a collection deleted it: %v", err)
	}
	// Idempotent in the clip.
	if err := d.RemoveClipFromCollection(ctx, col.ID, owner.ID, c1.ID, 1002); err != nil {
		t.Errorf("removing a non-member is an error: %v", err)
	}
	if err := d.RemoveClipFromCollection(ctx, col.ID, owner.ID, "nosuchclipid", 1002); err != nil {
		t.Errorf("removing an unknown id is an error: %v", err)
	}
	// Never idempotent in the collection.
	if err := d.RemoveClipFromCollection(ctx, col.ID, stranger.ID, c2.ID, 1003); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger's remove = %v, want ErrNotFound", err)
	}
	if err := d.RemoveClipFromCollection(ctx, "NoSuchCoLLid", owner.ID, c2.ID, 1003); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown collection = %v, want ErrNotFound", err)
	}
	if members, _ := d.CollectionClips(ctx, col.ID); len(members) != 1 {
		t.Errorf("a refused remove changed the membership: %+v", members)
	}
}

// TestAddRemoveAddKeepsPositionsUsable: a hole left by a removal is never
// re-used by an append, so the second add cannot collide with a live row.
func TestAddAfterRemoveAppendsPastTheHole(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	owner := mkUser(t, d, "curator")
	c1, _ := mkClip(t, d, owner, 1000, nil, false)
	c2, _ := mkClip(t, d, owner, 1000, nil, false)
	c3, _ := mkClip(t, d, owner, 1000, nil, false)
	col, err := d.CreateCollection(ctx, "CoLLeCtIoN1", owner.ID, "best of", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetCollectionClips(ctx, col.ID, owner.ID, []string{c1.ID, c2.ID}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveClipFromCollection(ctx, col.ID, owner.ID, c1.ID, 1001); err != nil {
		t.Fatal(err)
	}
	if err := d.AddClipToCollection(ctx, col.ID, owner.ID, c3.ID, 1002); err != nil {
		t.Fatalf("Add after a remove: %v", err)
	}
	members, err := d.CollectionClips(ctx, col.ID)
	if err != nil || len(members) != 2 || members[0].ID != c2.ID || members[1].ID != c3.ID {
		t.Fatalf("members = %+v (%v); want %s then %s", members, err, c2.ID, c3.ID)
	}
}

// TestCollectionsOfClip returns the CALLER's collections holding the clip and
// never anybody else's, however the membership was made.
func TestCollectionsOfClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	owner := mkUser(t, d, "curator")
	c, _ := mkClip(t, d, owner, 1000, nil, false)
	in, err := d.CreateCollection(ctx, "CoLLeCtIoN1", owner.ID, "in", 1000)
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.CreateCollection(ctx, "CoLLeCtIoN2", owner.ID, "out", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddClipToCollection(ctx, in.ID, owner.ID, c.ID, 1001); err != nil {
		t.Fatal(err)
	}
	got, err := d.CollectionsOfClip(ctx, c.ID, owner.ID)
	if err != nil {
		t.Fatalf("CollectionsOfClip: %v", err)
	}
	if !got[in.ID] || got[out.ID] || len(got) != 1 {
		t.Fatalf("CollectionsOfClip = %v; want only %s", got, in.ID)
	}
	// Asked as somebody else, it is empty: the map is scoped by owner, so a
	// page can never render a stranger's collection as a ticked box.
	stranger := mkUser(t, d, "stranger")
	if got, err := d.CollectionsOfClip(ctx, c.ID, stranger.ID); err != nil || len(got) != 0 {
		t.Fatalf("a stranger sees %v (%v)", got, err)
	}
}

// --- per-tag notify routing at enqueue time (ROADMAP #26) -------------------

// routedFinalize drives one clip to ready with a routing rule set and returns
// the notify job it queued (or false when it queued none).
func routedFinalize(t *testing.T, d *DB, tags []string, routing NotifyRouting) (Job, bool) {
	t.Helper()
	ctx := context.Background()
	owner := mkUser(t, d, "router"+ids12(t))
	c, j := mkClip(t, d, owner, 1000, nil, false)
	if len(tags) > 0 {
		if err := d.SetClipTags(ctx, c.ID, tags); err != nil {
			t.Fatalf("SetClipTags: %v", err)
		}
	}
	if _, _, _, err := d.ClaimJob(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), true, 1001, routing)
	if err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	jobs := notifyJobs(t, d, c.ID)
	if len(jobs) == 0 {
		return Job{}, false
	}
	if len(jobs) != 1 {
		t.Fatalf("%d notify jobs, want at most 1", len(jobs))
	}
	return jobs[0], true
}

var routerSeq int

func ids12(t *testing.T) string {
	t.Helper()
	routerSeq++
	return string(rune('a'+routerSeq%26)) + string(rune('a'+(routerSeq/26)%26))
}

// TestNotifyRouteIsChosenAtEnqueue: the FIRST rule whose tag the clip carries
// wins, in the operator's configured order and not the alphabet's, and the
// answer is stored on the job rather than re-derived at delivery.
func TestNotifyRouteIsChosenAtEnqueue(t *testing.T) {
	rules := []NotifyRule{{Tag: "clutch", Name: "clutch"}, {Tag: "fails", Name: "fails"}}
	cases := []struct {
		name  string
		tags  []string
		want  string
		queue bool
	}{
		{"the only matching tag", []string{"clutch"}, `{"route":"clutch"}`, true},
		{"the FIRST rule wins, not the first tag", []string{"fails", "clutch"}, `{"route":"clutch"}`, true},
		{"the second rule when the first does not match", []string{"fails"}, `{"route":"fails"}`, true},
		{"no match falls back to the default", []string{"gaming"}, "{}", true},
		{"no tags at all falls back to the default", nil, "{}", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := openTest(t)
			job, queued := routedFinalize(t, d, c.tags, NotifyRouting{Rules: rules, DefaultConfigured: true})
			if queued != c.queue {
				t.Fatalf("queued = %v, want %v", queued, c.queue)
			}
			if job.ParamsJSON != c.want {
				t.Errorf("params_json = %s, want %s", job.ParamsJSON, c.want)
			}
			if got := NotifyRouteOf(job.ParamsJSON); got != routeOf(c.want) {
				t.Errorf("NotifyRouteOf(%s) = %q", job.ParamsJSON, got)
			}
		})
	}
}

func routeOf(params string) string {
	if params == "{}" {
		return ""
	}
	return NotifyRouteOf(params)
}

// TestUnmatchedWithNoDefaultQueuesNothing is the DISCORD_WEBHOOK_UNMATCHED=skip
// half (and the "routes but no DISCORD_WEBHOOK_URL" deployment): a clip no rule
// matched has nowhere to go, so no job is written at all — which is what keeps
// "no webhook, no outbound request" true per clip and not just per instance.
func TestUnmatchedWithNoDefaultQueuesNothing(t *testing.T) {
	rules := []NotifyRule{{Tag: "clutch", Name: "clutch"}}
	d := openTest(t)
	if _, queued := routedFinalize(t, d, []string{"gaming"}, NotifyRouting{Rules: rules}); queued {
		t.Error("an unmatched clip queued a notify job with no default webhook")
	}
	d2 := openTest(t)
	job, queued := routedFinalize(t, d2, []string{"clutch"}, NotifyRouting{Rules: rules})
	if !queued || job.ParamsJSON != `{"route":"clutch"}` {
		t.Errorf("a matched clip = %v %s", queued, job.ParamsJSON)
	}
}

// TestNoRoutingIsTheOldBehaviourByteForByte: a caller that passes no routing —
// every existing one — writes `{}` exactly as it always did.
func TestNoRoutingIsTheOldBehaviour(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	owner := mkUser(t, d, "plain")
	c, j := mkClip(t, d, owner, 1000, nil, false)
	if _, _, _, err := d.ClaimJob(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), true, 1001); err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	jobs := notifyJobs(t, d, c.ID)
	if len(jobs) != 1 || jobs[0].ParamsJSON != "{}" {
		t.Fatalf("notify jobs = %+v", jobs)
	}
	if NotifyRouteOf(jobs[0].ParamsJSON) != "" {
		t.Error("an unrouted job named a route")
	}
}

// TestNotifyRouteOfIsTotal: a job row is not a place to fail over.
func TestNotifyRouteOfIsTotal(t *testing.T) {
	for _, in := range []string{"", "{}", "null", "not json", `{"route":123}`, `{"other":"x"}`} {
		if got := NotifyRouteOf(in); got != "" {
			t.Errorf("NotifyRouteOf(%q) = %q, want the default", in, got)
		}
	}
	if got := NotifyRouteOf(`{"route":"clutch"}`); got != "clutch" {
		t.Errorf("NotifyRouteOf = %q", got)
	}
}

// TestUnlistedStillOptsOutUnderRouting: routing changes WHERE a post goes and
// nothing about WHETHER an unlisted clip is announced.
func TestUnlistedStillOptsOutUnderRouting(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	owner := mkUser(t, d, "quiet")
	c, j := mkClip(t, d, owner, 1000, nil, true)
	if err := d.SetClipTags(ctx, c.ID, []string{"clutch"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.ClaimJob(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), true, 1001,
		NotifyRouting{Rules: []NotifyRule{{Tag: "clutch", Name: "clutch"}}, DefaultConfigured: true})
	if err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	if jobs := notifyJobs(t, d, c.ID); len(jobs) != 0 {
		t.Fatalf("an UNLISTED clip queued %d notify job(s) under routing", len(jobs))
	}
}

// TestUnmatchedSkipWithNoRulesQueuesNothing is the degenerate configuration
// DEPLOY §25.5 promises: DISCORD_WEBHOOK_UNMATCHED=skip with no rules at all
// means every clip is unmatched, so nothing is announced — the same thing
// unsetting the webhook does, said the other way round.
func TestUnmatchedSkipWithNoRulesQueuesNothing(t *testing.T) {
	d := openTest(t)
	if _, queued := routedFinalize(t, d, []string{"clutch"}, NotifyRouting{}); queued {
		t.Error("a clip was announced although there is nowhere for an unmatched one to go")
	}
	d2 := openTest(t)
	job, queued := routedFinalize(t, d2, []string{"clutch"}, NotifyRouting{DefaultConfigured: true})
	if !queued || job.ParamsJSON != "{}" {
		t.Errorf("with a default and no rules a clip goes to it: %v %s", queued, job.ParamsJSON)
	}
}
