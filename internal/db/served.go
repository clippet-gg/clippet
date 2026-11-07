package db

// served.go — the per-day served-bytes counter (ROADMAP #39, DESIGN.md §20
// erratum 270; migration 0017).
//
// Three statements, none of them on a hot path. The MEDIA path never comes
// here: it adds to a number in memory, and internal/cost flushes that number
// on a window with one UPSERT. What is here is that flush, the read the three
// surfaces make, and the sweeper's prune.
//
// Everything is aggregate. There is no clip, no account and no viewer in any
// signature below, because there is no such column (0017 says why).

import "context"

// AddBytesServed adds n to one UTC day's total, creating the row if it is the
// day's first flush. `day` is `YYYY-MM-DD`.
//
// It INCREMENTS and never sets, which is what makes the flush window safe: the
// caller holds an unflushed delta, not a running total, so a flush that lands
// after another process (or another window) wrote the same day adds to it
// instead of overwriting it. n <= 0 writes nothing — a flush with nothing in it
// costs no statement at all.
func (d *DB) AddBytesServed(ctx context.Context, day string, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := d.rw.ExecContext(ctx, `
INSERT INTO bytes_served (day, bytes) VALUES (?, ?)
ON CONFLICT(day) DO UPDATE SET bytes = bytes + excluded.bytes`, day, n)
	return err
}

// BytesServed reads one day's total and one month's in ONE round trip: `day`
// (`YYYY-MM-DD`) and the half-open month range [monthStart, monthEnd).
//
// One statement rather than two because /healthz calls it, and the rule that
// route has kept since §20 erratum 157 is that a liveness probe compose polls
// every ten seconds grows no scan. This is an index range over at most 31 rows
// of a table 0017 bounds at 400 — not a scan of `clips`, which is why the
// budget's `used` is still a cached reading and this is not.
//
// The day is expected to fall inside the month range (every caller passes
// today and today's month); nothing here requires it, and a caller that passes
// a day outside simply gets that day's total beside a month that does not
// contain it.
func (d *DB) BytesServed(ctx context.Context, day, monthStart, monthEnd string) (today, month int64, err error) {
	err = d.ro.QueryRowContext(ctx, `
SELECT COALESCE(SUM(CASE WHEN day = ? THEN bytes ELSE 0 END), 0), COALESCE(SUM(bytes), 0)
FROM bytes_served WHERE day >= ? AND day < ?`, day, monthStart, monthEnd).Scan(&today, &month)
	if err != nil {
		return 0, 0, err
	}
	return today, month, nil
}

// PruneBytesServed deletes every day strictly before `day` and returns how many
// rows went. The sweeper calls it once a tick with today minus the retention
// window; it is one DELETE over the primary key's own ordering, and on a
// healthy instance it deletes nothing.
func (d *DB) PruneBytesServed(ctx context.Context, day string) (int64, error) {
	return d.exec(ctx, "DELETE FROM bytes_served WHERE day < ?", day)
}
