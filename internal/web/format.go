package web

import (
	"strconv"
	"time"

	"clippet/internal/transcode"
)

// This file is the one home for the page formatters: the four functions that
// turn a stored number — a unix second, a millisecond count, a byte count, a
// configured duration — into the words a template prints. They were spread
// across server.go and pages.go, three of them adjacent and the fourth on the
// far side of a file boundary, which is how humanDuration came to round where
// its neighbours floor. Pure functions with no server state, so they sit
// together and are tested together (format_test.go).
//
// What deliberately does NOT live here: the byte formatter is mirrored, on
// purpose and with a comment saying so, in internal/httpapi (the upload
// limit's error) and cmd/clippet (`admin stats` and `admin backup`). Those
// copies exist so an owner comparing "Download original (3.4 MiB)" against
// MAX_UPLOAD_BYTES and against what the CLI prints is comparing three numbers
// in the same currency; merging them across packages is not the same job as
// gathering these four in one place, and is not done here.

func formatDate(ts int64) string { return time.Unix(ts, 0).UTC().Format(dateLayout) }

// expiryText describes a clip's lifetime: never, a future expiry, or an
// expiry the sweeper has not acted on yet (rows outlive expires_at by up to
// one tick, longer for processing clips under SWEEP_GRACE).
func expiryText(expiresAt *int64, now time.Time) string {
	switch {
	case expiresAt == nil:
		return "kept forever"
	case *expiresAt <= now.Unix():
		return "expired " + formatDate(*expiresAt) + " (pending deletion)"
	default:
		return "expires " + formatDate(*expiresAt)
	}
}

// formatDuration renders milliseconds as m:ss or h:mm:ss.
//
// Since §20 erratum 287 it is a one-line delegation rather than a copy. The
// duration is now drawn in three places a viewer can see at once — under the
// player, on the card's own description, and burned into the top-right of the
// poster — and the third of those is rendered by ffmpeg inside
// internal/transcode, which DESIGN §3 puts at the bottom of the import graph
// with no edge out of it. This package already imports that one (QualityShort
// builds the card's quality clause), so the shared spelling lives there and this
// is the caller. internal/notify keeps its own copy, pinned against the same
// table (testdata/duration.tsv), because the outbound-call package has no such
// edge; format_test.go pins THIS function against that table too, so the
// delegation is proven and not assumed.
func formatDuration(ms int64) string { return transcode.FormatDuration(ms) }

// humanDuration renders a whole number of days, hours or minutes — what
// SESSION_MAX_TTL is set in. It exists so the settings page can say "90 days"
// where the config says "2160h".
//
// Every branch FLOORS (§20 erratum 177's sibling nit). The sentence this feeds
// describes a CEILING — "a session never lives longer than X from the moment
// it started" — so a value rounded up would promise an operator's 30-minute
// setting an hour it does not have. Rounding down can only ever understate
// the ceiling, which is the harmless direction; the exception is a setting
// under a minute, which floors to zero and is therefore named rather than
// printed ("under a minute" is true and "0 minutes" is not).
func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		days := int64(d / (24 * time.Hour))
		return strconv.FormatInt(days, 10) + plural(" day", days)
	case d >= time.Hour:
		hours := int64(d / time.Hour)
		return strconv.FormatInt(hours, 10) + plural(" hour", hours)
	case d >= time.Minute:
		mins := int64(d / time.Minute)
		return strconv.FormatInt(mins, 10) + plural(" minute", mins)
	default:
		return "under a minute"
	}
}

// plural appends the "s" — one place rather than three.
func plural(unit string, n int64) string {
	if n == 1 {
		return unit
	}
	return unit + "s"
}

// byteUnits are the binary units humanBytes renders — the same ones the
// upload limit's error message and `admin stats` use, so an owner comparing
// "Download original (3.4 MiB)" against MAX_UPLOAD_BYTES is comparing two
// numbers in the same currency.
var byteUnits = [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// humanBytes renders a byte count with one decimal in the largest unit that
// leaves it under 1024.
func humanBytes(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " bytes"
	}
	div, exp := int64(1024), 0
	for m := n / 1024; m >= 1024 && exp < len(byteUnits)-1; m /= 1024 {
		div *= 1024
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + byteUnits[exp]
}
