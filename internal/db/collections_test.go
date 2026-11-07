package db

import (
	"context"
	"errors"
	"testing"

	"clippet/internal/ids"
)

func mkCollection(t *testing.T, d *DB, owner User, title string, now int64) Collection {
	t.Helper()
	c, err := d.CreateCollection(context.Background(), ids.New(), owner.ID, title, now)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	return c
}

func memberIDs(t *testing.T, d *DB, colID string) []string {
	t.Helper()
	clips, err := d.CollectionClips(context.Background(), colID)
	if err != nil {
		t.Fatalf("CollectionClips: %v", err)
	}
	out := make([]string, len(clips))
	for i, c := range clips {
		out[i] = c.ID
	}
	return out
}

func TestCollectionCRUD(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	me := mkUser(t, d, "carol")
	other := mkUser(t, d, "friend")

	col := mkCollection(t, d, me, "Best of", 1000)
	if col.Title != "Best of" || col.CreatedAt != 1000 || col.UpdatedAt != 1000 || col.ClipCount != 0 {
		t.Fatalf("created = %+v", col)
	}
	got, err := d.GetCollection(ctx, col.ID)
	if err != nil || got.ID != col.ID || got.ClipCount != 0 {
		t.Fatalf("GetCollection = %+v, %v", got, err)
	}
	// The public read is for ANY caller: the link is the auth.
	if _, err := d.GetCollection(ctx, "AAAAAAAAAAAA"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetCollection(missing) = %v", err)
	}

	// Listing is the OWNER's own, newest first, and nobody else's.
	mkCollection(t, d, me, "Older", 900)
	mkCollection(t, d, other, "Theirs", 1100)
	list, err := d.ListCollections(ctx, me.ID)
	if err != nil || len(list) != 2 || list[0].Title != "Best of" || list[1].Title != "Older" {
		t.Fatalf("ListCollections = %+v, %v", list, err)
	}
	if list, _ := d.ListCollections(ctx, "nobody"); len(list) != 0 {
		t.Fatalf("ListCollections for a stranger = %+v", list)
	}

	// Rename is owner-scoped: a stranger's rename is a 404, not a 403.
	renamed, err := d.UpdateCollectionTitle(ctx, col.ID, me.ID, "Best of 2026", 2000)
	if err != nil || renamed.Title != "Best of 2026" || renamed.UpdatedAt != 2000 {
		t.Fatalf("UpdateCollectionTitle = %+v, %v", renamed, err)
	}
	if _, err := d.UpdateCollectionTitle(ctx, col.ID, other.ID, "mine now", 2001); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename by a stranger = %v, want ErrNotFound", err)
	}
	if got, _ := d.GetCollection(ctx, col.ID); got.Title != "Best of 2026" {
		t.Fatalf("a stranger's rename landed: %q", got.Title)
	}

	// Delete, likewise.
	if err := d.DeleteCollection(ctx, col.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete by a stranger = %v, want ErrNotFound", err)
	}
	if err := d.DeleteCollection(ctx, col.ID, me.ID); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}
	if _, err := d.GetCollection(ctx, col.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
	if err := d.DeleteCollection(ctx, col.ID, me.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestCollectionCap(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	for i := 0; i < MaxCollectionsPerUser; i++ {
		if _, err := d.CreateCollection(ctx, ids.New(), u.ID, "c", 1000); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := d.CreateCollection(ctx, ids.New(), u.ID, "one too many", 1000); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("create past the cap = %v, want ErrLimitReached", err)
	}
	// The cap is per user, not global.
	other := mkUser(t, d, "friend")
	if _, err := d.CreateCollection(ctx, ids.New(), other.ID, "fine", 1000); err != nil {
		t.Fatalf("another user's first collection: %v", err)
	}
}

func TestCollectionCreateDuplicateID(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	id := ids.New()
	if _, err := d.CreateCollection(ctx, id, u.ID, "a", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateCollection(ctx, id, u.ID, "b", 1000); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate id = %v, want ErrConflict", err)
	}
}

// TestSetCollectionClipsOrderAndGuards is the whole membership contract:
// order is what the caller sent, duplicates collapse keeping the first
// position, a stranger's clip and an unknown id are the SAME 400, the cap is
// enforced, and every rejection leaves the previous membership untouched.
func TestSetCollectionClipsOrderAndGuards(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	me := mkUser(t, d, "carol")
	other := mkUser(t, d, "friend")
	col := mkCollection(t, d, me, "Best of", 1000)

	a, _ := mkClip(t, d, me, 1001, nil, false)
	b, _ := mkClip(t, d, me, 1002, nil, false)
	c, _ := mkClip(t, d, me, 1003, nil, true) // unlisted clips belong too
	theirs, _ := mkClip(t, d, other, 1004, nil, false)

	if err := d.SetCollectionClips(ctx, col.ID, me.ID, []string{c.ID, a.ID, b.ID}, 2000); err != nil {
		t.Fatalf("SetCollectionClips: %v", err)
	}
	if got := memberIDs(t, d, col.ID); len(got) != 3 || got[0] != c.ID || got[1] != a.ID || got[2] != b.ID {
		t.Fatalf("order = %v, want [%s %s %s]", got, c.ID, a.ID, b.ID)
	}
	if col2, _ := d.GetCollection(ctx, col.ID); col2.ClipCount != 3 || col2.UpdatedAt != 2000 {
		t.Fatalf("after PUT = %+v", col2)
	}

	// Duplicates collapse keeping the FIRST position.
	if err := d.SetCollectionClips(ctx, col.ID, me.ID, []string{b.ID, a.ID, b.ID}, 2001); err != nil {
		t.Fatalf("SetCollectionClips dupes: %v", err)
	}
	if got := memberIDs(t, d, col.ID); len(got) != 2 || got[0] != b.ID || got[1] != a.ID {
		t.Fatalf("after dupes = %v", got)
	}

	// A stranger's clip, an unknown id and a tombstone are one answer.
	gone, _ := mkClip(t, d, me, 1005, nil, false)
	if err := d.MarkDeleting(ctx, gone.ID, 2002); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{theirs.ID, "AAAAAAAAAAAA", gone.ID} {
		err := d.SetCollectionClips(ctx, col.ID, me.ID, []string{a.ID, bad}, 2003)
		var nyc *NotYourClipError
		if !errors.As(err, &nyc) || nyc.ClipID != bad {
			t.Fatalf("SetCollectionClips with %s = %v, want NotYourClipError naming it", bad, err)
		}
	}
	if got := memberIDs(t, d, col.ID); len(got) != 2 || got[0] != b.ID {
		t.Fatalf("a rejected PUT changed the membership: %v", got)
	}

	// Not the caller's collection.
	if err := d.SetCollectionClips(ctx, col.ID, other.ID, nil, 2004); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PUT by a stranger = %v, want ErrNotFound", err)
	}

	// The cap.
	tooMany := make([]string, MaxClipsPerCollection+1)
	for i := range tooMany {
		tooMany[i] = a.ID
	}
	if err := d.SetCollectionClips(ctx, col.ID, me.ID, tooMany, 2005); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("PUT past the cap = %v, want ErrLimitReached", err)
	}

	// Emptying.
	if err := d.SetCollectionClips(ctx, col.ID, me.ID, nil, 2006); err != nil {
		t.Fatalf("empty the collection: %v", err)
	}
	if got := memberIDs(t, d, col.ID); len(got) != 0 {
		t.Fatalf("after emptying = %v", got)
	}
}

// TestCollectionClipsIncludeEveryStateButDeleting: a clip that is still
// encoding shows up as processing, like it does in the library — it does not
// vanish from the list its owner put it in and reappear minutes later.
func TestCollectionClipsIncludeEveryStateButDeleting(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	col := mkCollection(t, d, u, "Mixed", 1000)

	queued, _ := mkClip(t, d, u, 1001, nil, false)
	readyClip, job := mkClip(t, d, u, 1002, nil, false)
	if _, got, ok, err := d.ClaimJob(ctx, 1100); err != nil || !ok || got.ID != queued.ID {
		t.Fatalf("ClaimJob: %v %v %s", err, ok, got.ID)
	}
	if _, got, ok, err := d.ClaimJob(ctx, 1100); err != nil || !ok || got.ID != readyClip.ID {
		t.Fatalf("ClaimJob 2: %v %v %s", err, ok, got.ID)
	}
	if ok, _, err := d.FinalizeTranscode(ctx, job.ID, readyClip.ID, ready(), false, 1200); err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	doomed, _ := mkClip(t, d, u, 1003, nil, false)

	if err := d.SetCollectionClips(ctx, col.ID, u.ID, []string{queued.ID, readyClip.ID, doomed.ID}, 2000); err != nil {
		t.Fatal(err)
	}
	if got := memberIDs(t, d, col.ID); len(got) != 3 {
		t.Fatalf("members = %v", got)
	}
	// A tombstone drops out of the list AND out of the count immediately,
	// before the sweeper has removed anything.
	if err := d.MarkDeleting(ctx, doomed.ID, 2100); err != nil {
		t.Fatal(err)
	}
	got := memberIDs(t, d, col.ID)
	if len(got) != 2 || got[0] != queued.ID || got[1] != readyClip.ID {
		t.Fatalf("after the tombstone = %v", got)
	}
	if col2, _ := d.GetCollection(ctx, col.ID); col2.ClipCount != 2 {
		t.Fatalf("clip_count with a tombstone = %d, want 2", col2.ClipCount)
	}
}

// TestClipDeleteCascadesMembership is the §12 promise of migration 0004:
// destroying a clip removes it from every collection, with no sweeper step.
func TestClipDeleteCascadesMembership(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	one := mkCollection(t, d, u, "One", 1000)
	two := mkCollection(t, d, u, "Two", 1001)
	a, _ := mkClip(t, d, u, 1002, nil, false)
	b, _ := mkClip(t, d, u, 1003, nil, false)
	if err := d.SetCollectionClips(ctx, one.ID, u.ID, []string{a.ID, b.ID}, 2000); err != nil {
		t.Fatal(err)
	}
	if err := d.SetCollectionClips(ctx, two.ID, u.ID, []string{a.ID}, 2000); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkDeleting(ctx, a.ID, 2100); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteClipRow(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := memberIDs(t, d, one.ID); len(got) != 1 || got[0] != b.ID {
		t.Fatalf("collection one after the delete = %v", got)
	}
	if got := memberIDs(t, d, two.ID); len(got) != 0 {
		t.Fatalf("collection two after the delete = %v", got)
	}
	var n int
	if err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM collection_clips WHERE clip_id = ?", a.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d collection_clips rows survived the clip (PRAGMA foreign_keys is not on?)", n)
	}
	// And the collections themselves are untouched: a list is not its links.
	if _, err := d.GetCollection(ctx, one.ID); err != nil {
		t.Fatalf("collection one is gone: %v", err)
	}
}

// TestDeleteCollectionKeepsItsClips: deleting the list deletes links, never
// media.
func TestDeleteCollectionKeepsItsClips(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	col := mkCollection(t, d, u, "Doomed", 1000)
	a, _ := mkClip(t, d, u, 1001, nil, false)
	if err := d.SetCollectionClips(ctx, col.ID, u.ID, []string{a.ID}, 2000); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteCollection(ctx, col.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetClip(ctx, a.ID); err != nil {
		t.Fatalf("the clip went with the collection: %v", err)
	}
	var n int
	if err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM collection_clips WHERE collection_id = ?", col.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d membership rows survived the collection", n)
	}
}

// TestListOwnerClips is the membership picker's source: the caller's own
// clips only, in every state but deleting — the library's visibility set (a
// stranger's listed clips) is the wrong set, because a collection may only
// hold clips the caller owns.
func TestListOwnerClips(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	me := mkUser(t, d, "carol")
	other := mkUser(t, d, "friend")
	a, _ := mkClip(t, d, me, 1001, nil, false)
	b, _ := mkClip(t, d, me, 1002, nil, true)
	mkClip(t, d, other, 1003, nil, false)
	gone, _ := mkClip(t, d, me, 1004, nil, false)
	if err := d.MarkDeleting(ctx, gone.ID, 2100); err != nil {
		t.Fatal(err)
	}
	own, err := d.ListOwnerClips(ctx, me.ID, 0)
	if err != nil || len(own) != 2 || own[0].ID != b.ID || own[1].ID != a.ID {
		t.Fatalf("ListOwnerClips = %v, %v", own, err)
	}
}
