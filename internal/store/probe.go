package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"clippet/internal/ids"
)

// ProbeKeyFolder is the key space a probe object lives in, under whatever
// S3_KEY_PREFIX the instance carries. It is deliberately NOT under `clips/`,
// so nothing that walks the clip prefixes — the sweeper, `admin orphans`
// (which lists ClipPrefix) — can ever see a probe object, and a probe that
// died between the put and the delete leaves its litter somewhere an operator
// recognises rather than somewhere a cleanup verb would report as an orphan.
const ProbeKeyFolder = "doctor/"

// The four steps a Probe takes, in order. They are exported because the ONE
// caller that renders a different sentence per step — `admin doctor`'s
// `bucket` row, whose exact lines doctor_test.go pins — has to tell them
// apart, and because "reachable but not writable" and "writable but not
// cleanable" are genuinely different operator problems.
const (
	ProbeStepPing    = "ping"    // the store did not answer at all
	ProbeStepPrepare = "prepare" // the local temp file could not be made (nothing to do with the store)
	ProbeStepPut     = "put"     // reachable, not writable
	ProbeStepDelete  = "delete"  // writable, the probe object could not be removed
)

// ProbeError says which step of a Probe failed. Unwrap keeps every
// errors.Is/As the callers already make working through it, so a PingError or
// a StorageError underneath is still reachable — which is what ProbeClass and
// the doctor's own storeClass read.
type ProbeError struct {
	Step string
	Err  error
}

func (e *ProbeError) Error() string { return e.Err.Error() }
func (e *ProbeError) Unwrap() error { return e.Err }

// ProbeStep names the step that failed, or "" when err is nil or is not a
// probe failure at all.
func ProbeStep(err error) string {
	var pe *ProbeError
	if errors.As(err, &pe) {
		return pe.Step
	}
	return ""
}

// Probe answers the one question a store's caller actually has: is it there,
// and can THIS process write to it (§20 erratum 297). It is what `admin
// doctor`'s bucket check has always done, extracted so the CLI and the setup
// page run the identical check rather than two implementations that drift.
//
// Three store calls and no fourth: Ping, put one small object under
// `{keyPrefix}doctor/`, delete it again. There is deliberately **no
// read-back**. Adding a GET would change what the `bucket` row proves — and
// what it has always printed — and would cost a Class B operation on every
// press of a button a person may press a dozen times while they are fixing
// their `.env`.
//
// Every failure is a *ProbeError naming the step, wrapping the store's own
// error. ProbeClass renders the one bounded word a non-operator surface may
// print about any of them.
func Probe(ctx context.Context, s Store, keyPrefix string) error {
	if s == nil {
		return &ProbeError{Step: ProbeStepPing,
			Err: &PingError{Class: PingUnavailable, Err: errors.New("store: probe: no store")}}
	}
	if err := s.Ping(ctx); err != nil {
		return &ProbeError{Step: ProbeStepPing, Err: err}
	}
	key := keyPrefix + ProbeKeyFolder + "probe-" + ids.New()
	tmp, err := os.CreateTemp("", "clippet-probe-*")
	if err != nil {
		return &ProbeError{Step: ProbeStepPrepare, Err: fmt.Errorf("store: probe: temp file: %w", err)}
	}
	name := tmp.Name()
	defer os.Remove(name)
	_, werr := io.WriteString(tmp, "clippet store write probe\n")
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return &ProbeError{Step: ProbeStepPrepare, Err: fmt.Errorf("store: probe: temp file: %w", werr)}
	}
	if _, err := s.PutFile(ctx, key, name, "application/octet-stream", nil); err != nil {
		return &ProbeError{Step: ProbeStepPut, Err: err}
	}
	if _, err := s.DeletePrefix(ctx, key); err != nil {
		return &ProbeError{Step: ProbeStepDelete, Err: err}
	}
	return nil
}

// ProbeClass names the bounded failure class of a Probe error, in the CLOSED
// §20 erratum 31 vocabulary and nothing else — `unreachable`, `timeout`,
// `denied`, `bucket missing`, `unavailable`.
//
// A Ping failure already carries one. A put or a delete failure carries the
// TRANSFER vocabulary instead (PublicClass), so the two are mapped onto the
// one set here rather than letting a second vocabulary reach a page: a reader
// learns that storage was the problem and nothing about where the storage is.
// Anything unclassified is `unavailable` — the leak is closed by default,
// never by enumerating the error shapes that would have leaked.
func ProbeClass(err error) string {
	if err == nil {
		return ""
	}
	var pe *PingError
	if errors.As(err, &pe) {
		return pe.HealthClass()
	}
	if PublicClass(err) == PublicTimeout {
		return PingTimeout
	}
	return PingUnavailable
}
