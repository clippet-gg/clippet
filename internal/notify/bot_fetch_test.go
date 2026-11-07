package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Bot.Fetch, the Clippet Auto URL's job (§20 erratum 324): the same fetcher,
// no interaction, one outcome on a buffered channel.

func fetchAtt(f *botFixture) Attachment {
	return Attachment{URL: f.attachmentURL(), Filename: "secret-name.mp4", ContentType: "video/mp4",
		Size: 11, Tag: "ROWid123"}
}

// One fetch, the ingest handed the bytes, the outcome on the channel, and no
// Discord API call at all (there is no interaction to answer). The log names
// the row, the clip and the outcome — never the URL or the file name.
func TestFetchFetchesIngestsAndReportsTheOutcome(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	var body []byte
	ch, err := f.bot.Fetch(fetchAtt(f), func(ctx context.Context, src IngestSource) (IngestResult, error) {
		body, _ = io.ReadAll(src.Body)
		return IngestResult{ClipID: "abc123XYZ789", CardReady: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-ch:
		if out.Err != nil || out.Result.ClipID != "abc123XYZ789" {
			t.Fatalf("outcome %+v", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no outcome")
	}
	if string(body) != "video bytes" || f.fetches() != 1 {
		t.Fatalf("ingest got %q, %d CDN hit(s)", body, f.fetches())
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if calls != 0 {
		t.Fatalf("Fetch called Discord's API %d time(s)", calls)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "autofetch=ROWid123") || !strings.Contains(logs, "outcome=ok") ||
		strings.Contains(logs, "secret-name") || strings.Contains(logs, f.cdn.URL) || strings.Contains(logs, "attachments/") {
		t.Fatalf("the log line: %s", logs)
	}
}

// A CDN refusal is the fetch sentence; the failure line carries an outcome
// word and still no URL or file name.
func TestFetchFailureIsOneWordOnTheLog(t *testing.T) {
	f := newBotFixture(t, http.StatusForbidden, "no")
	ch, err := f.bot.Fetch(fetchAtt(f), func(context.Context, IngestSource) (IngestResult, error) {
		t.Error("the ingest ran after a CDN refusal")
		return IngestResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := <-ch
	var ie *IngestError
	if !errors.As(out.Err, &ie) || ie.Sentence != SentenceFetch {
		t.Fatalf("outcome %+v", out)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "outcome=fetch") || strings.Contains(logs, "secret-name") ||
		strings.Contains(logs, f.cdn.URL) {
		t.Fatalf("the failure line: %s", logs)
	}
	// The job's own line carries a word, never the status; the status is the
	// `discord fetch` line's, the measurement (§20 erratum 340).
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "autofetch failed") && strings.Contains(withoutTimes(l), "403") {
			t.Fatalf("the failure line carries the status: %s", l)
		}
	}
	if fl := fetchLines(logs); len(fl) != 1 || !strings.Contains(fl[0], "status=403") {
		t.Fatalf("the fetch lines: %q", fl)
	}
}

// logTime matches the text handler's `time=` field. A millisecond of 403
// (…15.403Z) is not the CDN's status leaking onto the line, so the "no status
// number" assert reads the line without it.
var logTime = regexp.MustCompile(`time=\S+`)

func withoutTimes(logs string) string { return logTime.ReplaceAllString(logs, "time=") }

// The pin for the above: a timestamp's 403 is not a finding; a status number is.
func TestWithoutTimesKeepsTheStatusAssertHonest(t *testing.T) {
	stamp := "time=2026-10-08T02:58:15.403Z level=INFO msg=autofetch outcome=fetch"
	if strings.Contains(withoutTimes(stamp), "403") {
		t.Fatalf("a timestamp's millisecond still trips the assert: %s", withoutTimes(stamp))
	}
	if !strings.Contains(withoutTimes(stamp+" status=403"), "403") {
		t.Fatal("a real status=403 no longer trips the assert")
	}
}

// The channel is buffered: a job whose caller never reads ends anyway, and
// Shutdown — which waits for every counted job — returns.
func TestFetchNeverBlocksOnAnUnreadOutcome(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	if _, err := f.bot.Fetch(fetchAtt(f), func(context.Context, IngestSource) (IngestResult, error) {
		return IngestResult{ClipID: "x"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.bot.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with an unread outcome: %v", err)
	}
}

// Shutdown waits for a running Fetch: while the ingest holds, Shutdown with a
// short budget reports its deadline; the job sees the Bot's context cancelled
// (never a request's), ends, and a second Shutdown returns.
func TestShutdownWaitsForARunningFetch(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	started, release := make(chan struct{}), make(chan struct{})
	ch, err := f.bot.Fetch(fetchAtt(f), func(ctx context.Context, src IngestSource) (IngestResult, error) {
		close(started)
		<-release
		return IngestResult{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	short, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := f.bot.Shutdown(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown returned %v while a Fetch was running, want it to wait", err)
	}
	close(release)
	out := <-ch
	if !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("the job did not run under the Bot's context: %+v", out)
	}
	long, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if err := f.bot.Shutdown(long); err != nil {
		t.Fatalf("Shutdown after the job ended: %v", err)
	}
	if !strings.Contains(f.logs.String(), "outcome=restart") {
		t.Fatalf("the cancelled job's line: %s", f.logs.String())
	}
}

// Once Shutdown has begun, Fetch starts nothing and says so.
func TestFetchAfterShutdownIsRefused(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.bot.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	ch, err := f.bot.Fetch(fetchAtt(f), func(context.Context, IngestSource) (IngestResult, error) {
		t.Error("a job started after the shutdown")
		return IngestResult{}, nil
	})
	if !errors.Is(err, ErrClosed) || ch != nil || f.fetches() != 0 {
		t.Fatalf("Fetch after Shutdown: %v %v, %d hit(s)", ch, err, f.fetches())
	}
}

// A panic in the job is recovered: one outcome, one log line with no URL,
// file name or panic value, and the process lives.
func TestFetchRecoversAPanic(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	ch, err := f.bot.Fetch(fetchAtt(f), func(context.Context, IngestSource) (IngestResult, error) {
		panic("boom " + f.attachmentURL())
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-ch:
		if out.Err == nil {
			t.Fatal("a panicked job reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no outcome after a panic")
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "outcome=panic") || strings.Contains(logs, "boom") ||
		strings.Contains(logs, f.cdn.URL) || strings.Contains(logs, "secret-name") {
		t.Fatalf("the panic line: %s", logs)
	}
	// The value's Go type, and only that (§20 erratum 327).
	if !strings.Contains(logs, "panic_type=string") {
		t.Fatalf("the panic line does not name the value's type: %s", logs)
	}
}

// heldCDN answers nothing until the test ends: a CDN GET in flight.
func heldCDN(t *testing.T) (srv *httptest.Server, hit <-chan struct{}) {
	t.Helper()
	release, h := make(chan struct{}), make(chan struct{}, 8)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv, h
}

// A restart during the CDN GET is a restart, not Discord's fault (§20 erratum
// 327): the Fetch outcome is a bare cancellation (the route's 503) and its log
// word `restart`; a deferred /upload's edit is the restart sentence.
func TestARestartDuringTheCDNGetIsARestart(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	cdn, hit := heldCDN(t)
	att := fetchAtt(f)
	att.URL = cdn.URL + "/" + testAttachment
	ch, err := f.bot.Fetch(att, func(context.Context, IngestSource) (IngestResult, error) {
		t.Error("the ingest ran for a fetch that never answered")
		return IngestResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	job := okJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		t.Error("the ingest ran for a fetch that never answered")
		return IngestResult{}, nil
	})
	job.URL = cdn.URL + "/" + testAttachment
	f.bot.Clip(job)
	<-hit
	<-hit
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.bot.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	out := <-ch
	var ie *IngestError
	if !errors.Is(out.Err, context.Canceled) || errors.As(out.Err, &ie) {
		t.Fatalf("a restart mid-GET reads as %v, want a bare cancellation", out.Err)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "outcome=restart") || strings.Contains(logs, "outcome=fetch") {
		t.Fatalf("the Fetch line: %s", logs)
	}
	edit := f.waitEdited(t)
	if !strings.Contains(string(edit.body), SentenceRestart) {
		t.Fatalf("the /upload edit is %s, want the restart sentence", edit.body)
	}
}

// A job's log lines name its command (run 8): Enrich's say Enrich, a job with
// none says /upload — never /clip for a job that was not one.
func TestJobLinesNameTheCommand(t *testing.T) {
	f := newBotFixture(t, http.StatusForbidden, "no")
	job := okJob(f, func(context.Context, IngestSource) (IngestResult, error) { return IngestResult{}, nil })
	job.Command = "Enrich"
	f.bot.Clip(job)
	f.waitEdited(t)
	if logs := f.logs.String(); !strings.Contains(logs, "command=Enrich") || strings.Contains(logs, "command=/upload") {
		t.Fatalf("an Enrich job's line: %s", logs)
	}
	g := newBotFixture(t, http.StatusForbidden, "no")
	g.bot.Clip(okJob(g, func(context.Context, IngestSource) (IngestResult, error) { return IngestResult{}, nil }))
	g.waitEdited(t)
	if logs := g.logs.String(); !strings.Contains(logs, "command=/upload") {
		t.Fatalf("a job with no command's line: %s", logs)
	}
}
