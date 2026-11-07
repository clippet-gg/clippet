package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// The fetch measures itself, and the person sees the bytes land (§20 erratum
// 340, lane `fetchline`).
//
// A 263 MB `/upload` spent ≈55 s of its 64 s on ONE GET from Discord's CDN and
// said nothing while it ran. Two things change, both around the one GET
// fetchAndIngest makes: the body is counted, and that count feeds (1) ONE log
// line per fetch — the measurement that decides whether a parallel ranged fetch
// is worth building — and (2) every five seconds, the line the person who ran
// the command is looking at: `Uploading… 120 of 263 MB`.

// botProgressEvery is the cadence of the count's edit: the first edit at five
// seconds, never sooner, and one per five seconds after. With no retry, that is
// an order of magnitude under any per-route bucket this bot has met.
const botProgressEvery = 5 * time.Second

// mb is a megabyte as the line counts it: 10⁶ bytes, the decimal unit the
// product's storage line counts GB in (internal/cost, GB = 10⁹), and the unit
// the sentence's own "MB" names.
const mb = 1_000_000

// progressSentence is the line, exactly (the owner's words, signed off): the two numbers
// live, whole megabytes, no rate, no time, no percentage.
func progressSentence(got, total int64) string {
	if got > total {
		got = total // a declared size that lied: the stream stops at the cap anyway
	}
	return fmt.Sprintf("%s %d of %d MB", SentenceUploading, got/mb, total/mb)
}

// newTicker is time.NewTicker as the watcher needs it; a test swaps Bot.tick
// for a channel it feeds, so the cadence is pinned without waiting for it.
func newTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// fetchMeter counts the bytes the ingest reads off the response and notes when
// the body ended (EOF or an error), so the log line's seconds are the
// transfer's, not the ingest's: the spool, the probe and the card wait run
// after the last byte and are not Discord's.
type fetchMeter struct {
	r     io.Reader
	n     atomic.Int64
	once  sync.Once
	ended chan struct{}
	at    atomic.Int64 // UnixNano of the end; 0 while the body runs
}

func newFetchMeter(r io.Reader) *fetchMeter {
	return &fetchMeter{r: r, ended: make(chan struct{})}
}

func (m *fetchMeter) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	m.n.Add(int64(n))
	if err != nil {
		m.end()
	}
	return n, err
}

func (m *fetchMeter) end() {
	m.once.Do(func() {
		m.at.Store(time.Now().UnixNano())
		close(m.ended)
	})
}

// fetchLine is the ONE log line a fetch ends in, success or not. It names the
// transfer and nothing about the request: never the URL (a signed CDN link is
// a bearer link to the file), never a header that could carry a signature.
type fetchLine struct {
	started      time.Time
	ttfb         time.Duration // to the response's headers; 0 when none came
	status       int           // 0 when no response came
	proto        string
	acceptRanges string
	meter        *fetchMeter // nil when no body was read
	clipID       string
}

func (b *Bot) logFetch(l fetchLine) {
	var bytes int64
	end := time.Now()
	if l.meter != nil {
		bytes = l.meter.n.Load()
		if at := l.meter.at.Load(); at != 0 {
			end = time.Unix(0, at)
		}
	}
	secs := end.Sub(l.started).Seconds()
	rate := 0.0
	if secs > 0 {
		rate = float64(bytes) / mb / secs
	}
	attrs := []any{"bytes", bytes, "seconds", round1(secs), "mb_s", round1(rate),
		"proto", l.proto, "status", l.status, "accept_ranges", l.acceptRanges,
		"ttfb_ms", l.ttfb.Milliseconds()}
	if l.clipID != "" {
		attrs = append(attrs, "clip", l.clipID)
	}
	b.log.Info("discord fetch", attrs...)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// progressEdit is one edit of the line the person is waiting on. An error makes
// the watcher skip its next tick; it is never retried.
type progressEdit func(ctx context.Context, content string) error

// watchFetch edits the line every botProgressEvery while the body runs, from a
// goroutine of its own fed by the meter — the fetch never waits on Discord. It
// stops when the body ends, the context ends, or the returned stop is called;
// stop cancels an edit in flight and returns once the goroutine is gone, so no
// count can land after the message the job ends in.
func (b *Bot) watchFetch(ctx context.Context, m *fetchMeter, total int64, edit progressEdit) (stop func()) {
	ectx, cancel := context.WithCancel(ctx)
	ticks, stopTicks := b.tick(botProgressEvery)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer stopTicks()
		defer func() {
			if p := recover(); p != nil {
				b.log.Error("discord bot: the uploading count panicked", "panic_type", fmt.Sprintf("%T", p))
			}
		}()
		skip := false
		for {
			select {
			case <-ectx.Done():
				return
			case <-m.ended:
				return
			case <-ticks:
			}
			if skip { // the last edit was refused (a 429 among them): this tick rests
				skip = false
				continue
			}
			skip = edit(ectx, progressSentence(m.n.Load(), total)) != nil
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// progressFor is how a job's count reaches the person (nil: nobody waits on a
// line, as on a form job without its `Uploading…`, or Clippet Auto, which has no
// interaction at all). The deferred path edits its "thinking…" reply; the form's
// path edits its private `Uploading…` follow-up once Discord has named its id —
// before that, a tick has nothing to edit and passes.
func (b *Bot) progressFor(job ClipJob, line *heldLine) progressEdit {
	switch {
	case !job.Followup:
		path := b.followupPath(job.Token) + "/messages/@original"
		return func(ctx context.Context, content string) error {
			return b.patchCount(ctx, path, content)
		}
	case line != nil:
		return func(ctx context.Context, content string) error {
			id, ok := line.peek()
			if !ok || id == "" {
				return nil
			}
			return b.patchCount(ctx, b.followupPath(job.Token)+"/messages/"+id, content)
		}
	}
	return nil
}

// patchCount is ONE attempt: the count is a courtesy, and the next tick carries
// a newer number than any retry would.
func (b *Bot) patchCount(ctx context.Context, path, content string) error {
	body, _ := json.Marshal(newPayload(content)) // plain strings: cannot fail
	return b.call(ctx, http.MethodPatch, path, body, false)
}

// heldLine is the `Uploading…` follow-up's id, read twice: by the count (never
// waiting — a tick before Discord answered has nothing to edit) and by the
// delete at the job's end (waiting).
type heldLine struct {
	mu  sync.Mutex
	ch  <-chan string
	id  string
	got bool
}

func newHeldLine(ch <-chan string) *heldLine { return &heldLine{ch: ch} }

// peek is the id if postUploading has answered, without waiting.
func (h *heldLine) peek() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.got {
		select {
		case h.id = <-h.ch:
			h.got = true
		default:
		}
	}
	return h.id, h.got
}

// wait is the id once postUploading has answered ("" when nothing was posted).
func (h *heldLine) wait() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.got {
		h.id, h.got = <-h.ch, true
	}
	return h.id
}
