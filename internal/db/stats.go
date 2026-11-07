package db

import "context"

// AccountStats is one owner's own tally (ROADMAP #23, §20 erratum 156): what
// THEY have on this instance, for THEM. It is not a leaderboard and there is
// no route that returns anybody else's — the anti-feature line is "private
// view counts are the ceiling", and an aggregate of your own clips is inside
// it precisely because it never leaves the session that owns the rows.
//
// Every number is DERIVED at read time from columns that already exist. There
// is no counter table, for the reason every derived number in this codebase
// is derived: a clip leaves by a TTL sweep or an owner delete without anything
// asking a counter to move, and a counter nobody updates on that path lies.
type AccountStats struct {
	Clips       int   // every state except the `deleting` tombstone
	Ready       int   // …of which are playable
	BytesSource int64 // the untouched uploads
	// BytesRenditions is everything the transcode produced: the rendition, the
	// poster, the preview GIF, the filmstrip and the LIGHT copy. That last one
	// joined the sum with erratum 209's reasoning applied here (N102/N148): the
	// card file is a real object under the clip's own prefix, each clip's own
	// "Stored" line counts it (internal/web storedBytes) and StorageUsed counts
	// it, so an owner's account-wide total was short by every card file they own.
	BytesRenditions  int64
	ViewsTotal       int64
	Expiring7d       int // clips whose expires_at falls inside the next seven days
	CollectionsCount int
}

// accountStatsSQL is ONE pass over `clips_owner_created` for the six clip
// numbers, with the collections tally riding along as a scalar subquery. The
// sums COALESCE because every produced column is NULL until the clip is ready,
// and a NULL in SUM would make the whole sum NULL rather than the zero it
// means.
//
// The collections count is folded in here rather than fetched separately
// (N065): it is a constant subquery over an owner-indexed table, evaluated
// once, and folding it means the library page — which already makes several
// aggregates and DISCARDS this particular number, since its collection list
// comes from ListCollections — pays one statement fewer instead of one whole
// extra round trip for a value it never renders. The subquery's `?` binds
// after the two window bounds and before the outer `owner_id`: SQLite numbers
// anonymous parameters in the order they are WRITTEN.
const accountStatsSQL = `
SELECT COUNT(*),
       COALESCE(SUM(state = 'ready'), 0),
       COALESCE(SUM(source_size), 0),
       COALESCE(SUM(COALESCE(rendition_size, 0) + COALESCE(poster_size, 0) + COALESCE(gif_size, 0)
                    + COALESCE(filmstrip_size, 0) + COALESCE(social_size, 0)), 0),
       COALESCE(SUM(views), 0),
       COALESCE(SUM(expires_at IS NOT NULL AND expires_at > ? AND expires_at <= ?), 0),
       (SELECT COUNT(*) FROM collections WHERE owner_id = ?)
FROM clips WHERE owner_id = ? AND state <> 'deleting'`

// AccountStatsOf returns the caller's own tally in ONE round trip. `now` is
// the handler clock, passed in rather than read here so the seven-day window
// is the same instant the rest of the request works with.
func (d *DB) AccountStatsOf(ctx context.Context, ownerID string, now int64) (AccountStats, error) {
	var s AccountStats
	err := d.ro.QueryRowContext(ctx, accountStatsSQL, now, now+7*86400, ownerID, ownerID).Scan(
		&s.Clips, &s.Ready, &s.BytesSource, &s.BytesRenditions, &s.ViewsTotal, &s.Expiring7d,
		&s.CollectionsCount)
	if err != nil {
		return AccountStats{}, err
	}
	return s, nil
}

// StorageUsed is the instance's KNOWN object total in bytes: every byte this
// database says is in the bucket, across every account and every clip that is
// not already a tombstone (ROADMAP #24, §20 erratum 157).
//
// "Known" is the whole claim, and the word is load-bearing. This is a sum of
// row columns, not a bucket listing: it costs one indexed scan instead of a
// paginated LIST against S3, it is available to a process that cannot reach
// the bucket at all, and it is what `STORAGE_BUDGET_BYTES` is compared
// against. It therefore does NOT count an object no row references any more —
// `clippet admin orphans` is the report for those, and the DEPLOY section
// says so rather than letting an operator read this number as a bill.
//
// The LIGHT copy (`social_size`) is in the sum since the quality tiers, and its
// absence before them was an understatement rather than a policy: that file has
// been a real object under the clip's own prefix since erratum 121, `admin
// orphans` has always counted it, and an operator comparing this line with a
// bucket total was short by every card file on the instance (§20 erratum 209).
func (d *DB) StorageUsed(ctx context.Context) (int64, error) {
	var n int64
	err := d.ro.QueryRowContext(ctx, `
SELECT COALESCE(SUM(source_size + COALESCE(rendition_size, 0) + COALESCE(poster_size, 0)
                    + COALESCE(gif_size, 0) + COALESCE(filmstrip_size, 0) + COALESCE(social_size, 0)), 0)
FROM clips WHERE state <> 'deleting'`).Scan(&n)
	return n, err
}

// ClipCensus is the instance-wide pair the setup page needs in one round trip
// (ROADMAP #45, §20 erratum 297): how many clips this instance holds, and how
// many bytes they account for.
//
// It is StorageUsed's own sum with a COUNT beside it, over the same rows and
// the same tombstone rule, because the two numbers are only useful together:
// one decides whether the STOP row renders at all, and their RATIO is what
// turns "10 GB free" into a number of clips on this instance. Splitting them
// into two statements would let a page print an average of a count and a sum
// taken a moment apart.
func (d *DB) ClipCensus(ctx context.Context) (clips int, bytes int64, err error) {
	err = d.ro.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(source_size + COALESCE(rendition_size, 0) + COALESCE(poster_size, 0)
                    + COALESCE(gif_size, 0) + COALESCE(filmstrip_size, 0) + COALESCE(social_size, 0)), 0)
FROM clips WHERE state <> 'deleting'`).Scan(&clips, &bytes)
	if err != nil {
		return 0, 0, err
	}
	return clips, bytes, nil
}
