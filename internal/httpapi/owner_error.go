package httpapi

import "clippet/internal/worker"

// ownerJobError maps a FAILED job's last_error onto that kind's own fixed
// vocabulary before an owner reads it (§20 erratum 165, closing the gap
// erratum 80 left open on the restart path). The raw text stays in the
// operator log, where it belongs; what GET …/poster, GET …/trim and
// GET …/replace put in `error` is one of the sentences internal/worker words
// that kind's endings with, and an unrecognised text — in practice one of the
// two internal/db writes when it repairs a job nobody is running — becomes the
// kind's generic sentence instead of an operator's.
//
// It lives in a file of its own so the four routes that need it share one
// spelling; the vocabulary itself stays in internal/worker, which owns it.
//
// nil in, nil out: a failed job with no text says nothing extra, as before.
func ownerJobError(kind string, lastError *string) *string {
	if lastError == nil {
		return nil
	}
	s := worker.OwnerError(kind, *lastError)
	if s == "" {
		return nil
	}
	return &s
}
