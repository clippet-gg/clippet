package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"clippet/internal/cost"
	"clippet/internal/httpx"
)

// newBudget is a storage-budget holder that has already taken a reading — the
// state the sweeper leaves it in after its first tick.
func newBudget(used, limit int64) *httpx.StorageBudget {
	b := &httpx.StorageBudget{}
	b.Set(used, limit)
	return b
}

// The served-bytes counter's measuring point (ROADMAP #39, §20 erratum 270):
// one wrapper around the writer `http.ServeContent` streams through, counting
// the bytes of a 200 or a 206 body and nothing else.
//
// Every assertion below reads `Pending("")` — the whole unflushed window,
// whatever day its entries are keyed by. The counter keys its window by UTC day
// and a test that ran across midnight would otherwise be looking in the wrong
// row; the number these tests are about is the bytes, not the key.

// servedEnv is an env whose handler counts, plus the counter itself.
func servedEnv(t *testing.T) (*env, *cost.Counter) {
	t.Helper()
	var c *cost.Counter
	e := newEnv(t, func(d *Deps) {
		c = cost.New(d.DB, d.Log)
		d.Served = c
	})
	return e, c
}

func TestServedBytesCountsWhatTheViewerReceived(t *testing.T) {
	e, counter := servedEnv(t)
	owner, _ := e.newUser("owner")
	c, rendition, poster := e.readyClip(owner, clipOpts{})
	base := "/api/clips/" + c.ID
	size := int64(len(rendition))

	// A whole body: exactly its length.
	if r := e.do("GET", base+"/video.mp4", nil); r.code != 200 {
		t.Fatalf("video.mp4: %d", r.code)
	}
	if got := counter.Pending(""); got != size {
		t.Fatalf("a full GET counted %d, want %d", got, size)
	}

	// A RANGE: the hundred bytes it asked for, not the file.
	if r := e.do("GET", base+"/video.mp4", nil, withHeader("Range", "bytes=100-199")); r.code != 206 {
		t.Fatalf("ranged GET: %d", r.code)
	}
	if got := counter.Pending(""); got != size+100 {
		t.Fatalf("after a 100-byte range the counter is at %d, want %d", got, size+100)
	}

	// The other objects a viewer's page pulls are counted too — the poster here,
	// and `social.mp4`, which on this clip falls back to the rendition.
	if r := e.do("GET", base+"/poster.jpg", nil); r.code != 200 {
		t.Fatalf("poster.jpg: %d", r.code)
	}
	if got := counter.Pending(""); got != size+100+int64(len(poster)) {
		t.Fatalf("the poster was not counted: %d", got)
	}
	before := counter.Pending("")
	if r := e.do("GET", base+"/social.mp4", nil); r.code != 200 {
		t.Fatalf("social.mp4: %d", r.code)
	}
	if got := counter.Pending("") - before; got != size {
		t.Fatalf("the card copy counted %d, want %d", got, size)
	}

	// And nothing that is NOT a body: a HEAD, a 304 and a 416 all write either
	// no body or an error envelope, and an error envelope is not serving.
	before = counter.Pending("")
	if r := e.do("HEAD", base+"/video.mp4", nil); r.code != 200 {
		t.Fatalf("HEAD: %d", r.code)
	}
	etag := `"` + sha256hex(rendition) + `"`
	if r := e.do("GET", base+"/video.mp4", nil, withHeader("If-None-Match", etag)); r.code != 304 {
		t.Fatalf("304: %d", r.code)
	}
	if r := e.do("GET", base+"/video.mp4", nil, withHeader("Range", "bytes="+strconv.FormatInt(size+10, 10)+"-")); r.code != 416 {
		t.Fatalf("416: %d", r.code)
	}
	if r := e.do("GET", base+"/video.mp4", nil, withHeader("If-Match", `"nope"`)); r.code != 412 {
		t.Fatalf("412: %d", r.code)
	}
	if got := counter.Pending(""); got != before {
		t.Fatalf("a HEAD, a 304, a 416 and a 412 added %d bytes", got-before)
	}
}

// A transfer that ends early is charged what it delivered: the row says the
// object is bigger than it is, so the stream stops short of the Content-Length
// and the counter records the bytes that actually went out.
func TestServedBytesCountsAShortTransferForWhatItWrote(t *testing.T) {
	e, counter := servedEnv(t)
	owner, _ := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	e.setClipColumn(c.ID, "rendition_size", int64(len(rendition))+4096)

	r := e.do("GET", "/api/clips/"+c.ID+"/video.mp4", nil)
	if r.code != 200 || len(r.body) != len(rendition) {
		t.Fatalf("short object: %d len=%d, want 200 and %d bytes", r.code, len(r.body), len(rendition))
	}
	if got := counter.Pending(""); got != int64(len(rendition)) {
		t.Fatalf("a truncated stream counted %d, want the %d bytes it wrote", got, len(rendition))
	}

	// A stream that fails before its first byte costs nothing at all.
	before := counter.Pending("")
	e.fake.FailNext("GetRange", io.ErrUnexpectedEOF)
	if r := e.do("GET", "/api/clips/"+c.ID+"/video.mp4", nil); len(r.body) != 0 {
		t.Fatalf("the failed stream wrote %d bytes", len(r.body))
	}
	if got := counter.Pending(""); got != before {
		t.Fatalf("a stream that never delivered a byte counted %d", got-before)
	}
}

// servedWriter itself, at the two edges the routes above cannot reach through a
// recorder: a Write that is accepted only in part, and an error status written
// through the same writer.
func TestServedWriterCountsAcceptedBytesOnly(t *testing.T) {
	full := &servedWriter{ResponseWriter: httptest.NewRecorder()}
	n, err := full.Write([]byte("hello"))
	if n != 5 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if full.n != 5 || full.status != http.StatusOK {
		t.Fatalf("an implicit 200 counted %d at status %d", full.n, full.status)
	}

	partial := &servedWriter{ResponseWriter: &shortWriter{ResponseWriter: httptest.NewRecorder(), accept: 3}}
	partial.WriteHeader(http.StatusPartialContent)
	n, _ = partial.Write([]byte("0123456789"))
	if n != 3 || partial.n != 3 {
		t.Fatalf("a short write counted %d (Write returned %d), want 3", partial.n, n)
	}

	errored := &servedWriter{ResponseWriter: httptest.NewRecorder()}
	errored.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	if _, err := errored.Write([]byte(`{"error":"range_not_satisfiable"}`)); err != nil {
		t.Fatal(err)
	}
	if errored.n != 0 {
		t.Fatalf("an error envelope counted %d bytes", errored.n)
	}
	if errored.Unwrap() == nil {
		t.Fatal("Unwrap must hand back the wrapped writer")
	}
}

// shortWriter accepts only the first `accept` bytes of any Write, as a socket
// with a full buffer and a dying peer does.
type shortWriter struct {
	http.ResponseWriter
	accept int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.accept {
		p = p[:w.accept]
	}
	n, _ := w.ResponseWriter.Write(p)
	return n, io.ErrClosedPipe
}

// /healthz' storage line: three byte counts as JSON numbers, absent keys where
// a number would be a lie, and no cost of any kind (§20 erratum 270).
func TestHealthzStorageUsage(t *testing.T) {
	e, counter := servedEnv(t)
	owner, _ := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	if r := e.do("GET", "/api/clips/"+c.ID+"/video.mp4", nil); r.code != 200 {
		t.Fatalf("video.mp4: %d", r.code)
	}
	if got := counter.Pending(""); got != int64(len(rendition)) {
		t.Fatalf("the counter is at %d before the probe is read", got)
	}

	// No budget holder is wired, so there is no stored reading to publish: the
	// stored keys are ABSENT rather than zero, and the served ones are there.
	body := string(e.do("GET", "/healthz", nil).body)
	if !strings.Contains(body, `"storage_usage":{"served_today_bytes":`+strconv.Itoa(len(rendition))) {
		t.Fatalf("/healthz did not carry the served bytes: %s", body)
	}
	if strings.Contains(body, "stored_bytes") || strings.Contains(body, "cost") {
		t.Fatalf("/healthz invented a stored figure or a cost: %s", body)
	}
	// The ping-health string keeps its own key: `storage` is a string and
	// `storage_usage` is the object beside it.
	if !strings.Contains(body, `"storage":"`) {
		t.Fatalf("/healthz' storage health string is gone: %s", body)
	}
}

// A PRICED instance publishes the same three byte counts as an unpriced one and
// NOT ONE AMOUNT (§20 erratum 270(e)): /healthz is anonymous and reachable
// through the tunnel, and a cost beside the bytes it came from is the
// operator's own per-GB price one division away.
func TestHealthzStorageUsageNeverCarriesAPrice(t *testing.T) {
	// 200 GB stored at 0.015 a GB-month would be 3.00, and a GB served at 0.01
	// would be 0.01. Neither may appear.
	const stored = 200 * cost.GB
	storage, egress := int64(15_000), int64(10_000)
	var counter *cost.Counter
	e := newEnv(t, func(d *Deps) {
		counter = cost.New(d.DB, d.Log)
		d.Served = counter
		d.Budget = newBudget(stored, 0)
		d.Cfg.StoragePriceMicros, d.Cfg.EgressPriceMicros = &storage, &egress
	})
	// Through the counter, which is the production path: the window is keyed by
	// the counter's own clock and /healthz adds it back under the same one, so
	// exactly one clock decides the day (a row seeded for a day a SECOND clock
	// computed is how a test like this goes flaky at a UTC midnight).
	counter.Add(cost.GB)

	body := string(e.do("GET", "/healthz", nil).body)
	for _, want := range []string{
		`"stored_bytes":200000000000`,
		`"served_today_bytes":1000000000`,
		`"served_month_bytes":1000000000`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/healthz is missing %s:\n%s", want, body)
		}
	}
	// The scaled prices themselves (15000, 10000 micros) are deliberately NOT in
	// this list: they are substrings of legitimate byte counts. What must not
	// appear is the vocabulary and the rendered amounts — and the real proof is
	// the byte-for-byte comparison with the unpriced instance below.
	for _, forbidden := range []string{"cost", "price", "3.00", "0.01"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("/healthz carries %q — an anonymous route must publish no price and nothing derived from one:\n%s",
				forbidden, body)
		}
	}
	// The unpriced instance answers byte-identically on that object, which is
	// the point: a stranger cannot tell a priced instance from an unpriced one.
	e2 := newEnv(t, func(d *Deps) {
		c := cost.New(d.DB, d.Log)
		c.Add(cost.GB)
		d.Served = c
		d.Budget = newBudget(stored, 0)
	})
	body2 := string(e2.do("GET", "/healthz", nil).body)
	if usage(t, body) != usage(t, body2) {
		t.Errorf("a priced instance's storage_usage differs from an unpriced one's:\n%s\n%s", body, body2)
	}
}

// usage cuts the storage_usage object out of a /healthz body.
func usage(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `"storage_usage":`)
	if i < 0 {
		t.Fatalf("no storage_usage object in %s", body)
	}
	j := strings.Index(body[i:], "}")
	if j < 0 {
		t.Fatalf("unterminated storage_usage object in %s", body)
	}
	return body[i : i+j+1]
}
