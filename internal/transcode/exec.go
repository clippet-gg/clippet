package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// killDelay is cmd.WaitDelay: SIGKILL this long after SIGTERM, so a
	// cancelled ffmpeg can never linger as a zombie (§10.3).
	killDelay = 5 * time.Second
	// stderrRingSize bounds captured stderr; failureTail is how much of it
	// becomes the failure reason on a non-zero exit. They are not the same
	// number and must not be confused: the reason is a LOG LINE (the last
	// kilobyte, which may open mid-word), while anything that CLASSIFIES a
	// failure reads the whole ring — the tail can open past the line that named
	// the cause, and that cost a clip (§20 erratum 289).
	//
	// 8 KiB is sized against what an ffmpeg of ours can print. Every argv here
	// that encodes carries `-hide_banner -nostdin -loglevel error -nostats`,
	// and no argv here runs above error level at all, so nothing below
	// AV_LOG_ERROR is written: the longest failure this package has measured —
	// a hardware decoder refusing a source, whose shutdown then reports every
	// thread and every unopened encoder — is ≈1.2 KB, and the
	// two-output argv's extra streams could not take it past ≈2 KB. One ring
	// lives per RUNNING ffmpeg and APP_CPUS bounds those, so the whole cost is
	// tens of kilobytes.
	stderrRingSize = 8 << 10
	failureTail    = 1000
	// maxLineLen bounds one -progress line; longer input is discarded.
	maxLineLen = 64 << 10
)

// protocolWhitelist is the ONLY protocol ffmpeg or ffprobe may open for an
// input of ours: the pipeline hands them a path in the scratch directory and
// never a crypto: or data: URL, so `file` is the whole of it (§20 erratum 91).
//
// It is stated because a demuxer opens what its INPUT tells it to. A list
// format — HLS, DASH, an ffconcat script, an SDP — is a text file naming
// further resources, and without a whitelist the demuxer fetches them: that
// is the CVE-2016-1897/1898 SSRF class, and it would be the one outbound
// request this binary makes that §15's outbound-client tripwire cannot see
// (naming that type here in full would trip the grep itself — check.sh's
// expression does not know a comment from code).
// ffmpeg's own built-in default (`file,crypto,data`) happens to refuse
// http today, but a vendor default is the vendor's to change on the next
// bump; ours is ours. (The ingest gate refuses those containers outright —
// see the allowlist in ParseProbe — so this is the second lock, not the
// first.)
const protocolWhitelist = "file"

// inputArgs renders one ffmpeg input: the protocol whitelist immediately in
// front of the `-i` it governs. ffmpeg applies input options to the input
// that FOLLOWS them, so the pairing is positional and every `-i` in a
// multi-input argv needs its own copy. Every ffmpeg argv this package builds
// goes through here (scripts/check.sh has a tripwire for a sixth call site
// that spells `-i` by hand).
func inputArgs(path string) []string {
	return []string{"-protocol_whitelist", protocolWhitelist, "-i", path}
}

// probeInputArgs renders ffprobe's input, whose path is positional: the same
// whitelist, then the path it governs.
func probeInputArgs(path string) []string {
	return []string{"-protocol_whitelist", protocolWhitelist, path}
}

// newCommand builds an argv-based exec.Cmd bound to ctx: no shell, stdin
// from the null device, SIGTERM on cancellation with a SIGKILL backstop.
func newCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Cancel = func() error {
		err := cmd.Process.Signal(syscall.SIGTERM)
		if err == nil || errors.Is(err, os.ErrProcessDone) {
			return err
		}
		// A platform that cannot deliver SIGTERM (Windows) falls back to
		// Kill; WaitDelay would do the same 5s later.
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = killDelay
	return cmd
}

// isExit reports whether err carries an exit status (the process ran and
// finished) as opposed to a start failure.
func isExit(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

// stderrCapture is what one run printed on stderr, in the TWO readings §20
// erratum 289 made distinct — and it is ONE value so a caller cannot get one
// of them by accident.
//
// Reason is the last failureTail bytes, which is what becomes a failure reason
// because a reason is a log line. All is the WHOLE retained ring, which is what
// anything that CLASSIFIES a failure has to read: ffmpeg's shutdown after a
// refused hardware decode runs past a kilobyte, so the tail can open PAST the
// line that named the cause. That cost a clip once (erratum 289); the shape it
// cost it in was a function that returned the tail alone, and this is the fix
// for the shape rather than for the one caller. runCapture used to be exactly
// that function: nothing classifies an ffprobe failure today, so it was not a
// bug — it was the same trap waiting for the next classifier (a nit from lane
// hwtail).
type stderrCapture struct {
	reason string // the last failureTail bytes
	all    string // everything the ring still holds
}

// Reason is the log line: the last failureTail bytes, which may open mid-word.
func (c stderrCapture) Reason() string { return c.reason }

// All is the whole ring, for anything that asks a QUESTION of this stderr
// rather than printing it.
func (c stderrCapture) All() string { return c.all }

// captureOf takes both readings of a settled ring. The caller must have
// Wait()ed: os/exec closes its copiers there, and only then is the ring safe to
// read from another goroutine.
func captureOf(r *ring) stderrCapture {
	return stderrCapture{reason: r.Tail(failureTail), all: r.Tail(stderrRingSize)}
}

// runCapture runs bin with args and returns its stdout, its stderr (both
// readings) and the Wait error. A cancelled ctx is returned as ctx.Err().
func runCapture(ctx context.Context, bin string, args ...string) (stdout []byte, stderr stderrCapture, err error) {
	cmd := newCommand(ctx, bin, args...)
	var out bytes.Buffer
	ring := newRing(stderrRingSize)
	cmd.Stdout, cmd.Stderr = &out, ring
	err = cmd.Run()
	if cerr := ctx.Err(); cerr != nil {
		return nil, captureOf(ring), cerr
	}
	return out.Bytes(), captureOf(ring), err
}

// exitReason renders a Wait error as "NAME exited N: <reason>" (or the raw
// error when there is no exit code, e.g. the binary could not start). It takes
// the whole capture and spends its Reason, so a caller cannot hand the log line
// the wrong reading.
func exitReason(name string, err error, se stderrCapture) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		msg := fmt.Sprintf("%s exited %d", name, ee.ExitCode())
		if ee.ExitCode() == -1 && ee.ProcessState != nil {
			msg = fmt.Sprintf("%s killed: %s", name, ee.ProcessState.String())
		}
		if se.Reason() != "" {
			msg += ": " + se.Reason()
		}
		return msg
	}
	return name + ": " + err.Error()
}

// ring is a fixed-capacity tail buffer: Write keeps only the last cap bytes.
type ring struct {
	buf  []byte
	w    int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]byte, n)} }

func (r *ring) Write(p []byte) (int, error) {
	n := len(p)
	if n >= len(r.buf) {
		copy(r.buf, p[n-len(r.buf):])
		r.w, r.full = 0, true
		return n, nil
	}
	first := copy(r.buf[r.w:], p)
	if first < n {
		copy(r.buf, p[first:])
		r.full = true
	}
	r.w = (r.w + n) % len(r.buf)
	if r.w < first {
		r.full = true
	}
	return n, nil
}

// String returns the retained bytes in write order.
func (r *ring) String() string {
	if !r.full {
		return string(r.buf[:r.w])
	}
	return string(r.buf[r.w:]) + string(r.buf[:r.w])
}

// Tail returns the last n bytes, trimmed to a UTF-8 boundary and whitespace.
func (r *ring) Tail(n int) string {
	s := r.String()
	if len(s) > n {
		s = s[len(s)-n:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
	}
	return strings.TrimSpace(s)
}

// lineWriter is an io.Writer that hands complete lines (without the
// trailing newline or carriage return) to fn as they arrive.
type lineWriter struct {
	fn  func(line string)
	buf []byte
}

func newLineWriter(fn func(string)) *lineWriter { return &lineWriter{fn: fn} }

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(l.buf[:i]), "\r")
		l.buf = l.buf[i+1:]
		l.fn(line)
	}
	if len(l.buf) > maxLineLen {
		l.buf = l.buf[:0]
	}
	return len(p), nil
}
