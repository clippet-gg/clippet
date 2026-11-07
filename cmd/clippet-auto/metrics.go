package main

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"clippet/internal/autobot"
)

// Observability (§20 erratum 334): Prometheus text on /metrics
// (Fly scrapes `[metrics]` into its Grafana), /healthz, and ONE JSON log line
// per decision — ts, level, event, guild, channel, msg_id, outcome, ms; never
// a token, a message's text, a file name or an unsealed URL.

// ackGrace is how long a heartbeat may wait for its ACK before /healthz says
// the session is not healthy (Discord's interval is ~41 s; a beat unanswered
// by the next one is closed as a zombie by internal/autobot anyway).
const ackGrace = 10 * time.Second

// dispatchBuckets are auto_dispatch_ms's upper bounds: a box holds its answer
// up to 100 s.
var dispatchBuckets = []float64{250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 110000}

type metrics struct {
	log *slog.Logger
	now func() time.Time

	mu          sync.Mutex
	up          bool
	beatAt      time.Time
	ackAt       time.Time
	identifies  []time.Time
	resumes     uint64
	reconnects  uint64
	latencyMs   float64
	guilds      int
	videos      map[string]uint64
	uploads     map[string]uint64
	dispatch    []uint64 // per bucket, then +Inf
	dispatchSum float64
	dispatchN   uint64
	queue       int
	rest429     uint64
	planeErrs   uint64
}

func newMetrics(log *slog.Logger) *metrics {
	return &metrics{log: log, now: time.Now, videos: map[string]uint64{}, uploads: map[string]uint64{},
		dispatch: make([]uint64, len(dispatchBuckets)+1)}
}

// Gateway is autobot.Observer.
func (m *metrics) Gateway(e autobot.GatewayEvent) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	switch e.Kind {
	case "identify":
		m.identifies = append(m.identifies, now)
		m.pruneLocked(now)
	case "resume":
		m.resumes++
	case "reconnect":
		m.reconnects++
	case "up":
		m.up = true
		m.beatAt, m.ackAt = time.Time{}, time.Time{}
	case "down":
		m.up = false
	case "beat":
		m.beatAt = now
	case "ack":
		m.ackAt = now
		m.latencyMs = float64(e.RTT) / float64(time.Millisecond)
	}
}

func (m *metrics) pruneLocked(now time.Time) {
	keep := m.identifies[:0]
	for _, t := range m.identifies {
		if now.Sub(t) < 24*time.Hour {
			keep = append(keep, t)
		}
	}
	m.identifies = keep
}

// Decide is autobot.Observer: the counters, and the decision's log line. A
// skipped poster's video (d.Skip) counts nothing and is the same line at
// debug level with one field more, `skip` — never the poster's id.
func (m *metrics) Decide(d autobot.Decision) {
	if d.Skip != "" {
		m.log.Debug("decision", "event", d.Event, "guild", d.Guild, "channel", d.Channel, "msg_id", d.Message,
			"mode", d.Mode, "outcome", d.Outcome, "ms", d.Took.Milliseconds(), "skip", d.Skip)
		return
	}
	m.mu.Lock()
	switch d.Event {
	case "video":
		m.videos[label(d.Mode)]++
	case "link":
		m.uploads[label(d.Outcome)]++
		ms := float64(d.Took) / float64(time.Millisecond)
		i := sort.SearchFloat64s(dispatchBuckets, ms)
		m.dispatch[i]++
		m.dispatchSum += ms
		m.dispatchN++
	}
	m.mu.Unlock()
	m.log.Info("decision", "event", d.Event, "guild", d.Guild, "channel", d.Channel, "msg_id", d.Message,
		"mode", d.Mode, "outcome", d.Outcome, "ms", d.Took.Milliseconds())
}

// RateLimited is autobot.Observer: one 429 from Discord's REST.
func (m *metrics) RateLimited() {
	m.mu.Lock()
	m.rest429++
	m.mu.Unlock()
}

// QueueDepth is autobot.Observer.
func (m *metrics) QueueDepth(n int) {
	m.mu.Lock()
	m.queue = n
	m.mu.Unlock()
}

func (m *metrics) planeError() {
	m.mu.Lock()
	m.planeErrs++
	m.mu.Unlock()
}

func (m *metrics) setGuilds(n int) {
	m.mu.Lock()
	m.guilds = n
	m.mu.Unlock()
}

// healthy: the session is READY or RESUMED, and no heartbeat has waited
// longer than ackGrace for its ACK.
func (m *metrics) healthy() bool {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.up {
		return false
	}
	return !(m.beatAt.After(m.ackAt) && now.Sub(m.beatAt) > ackGrace)
}

// label keeps a label value to the words this process writes.
func label(s string) string {
	if s == "" {
		return "none"
	}
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			b[i] = '_'
		}
	}
	if len(b) > 40 {
		b = b[:40]
	}
	return string(b)
}

// write is the Prometheus text format, version 0.0.4.
func (m *metrics) write(w io.Writer) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	var b strings.Builder
	head := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	one := func(name, typ, help string, v float64) {
		head(name, typ, help)
		fmt.Fprintf(&b, "%s %g\n", name, v)
	}
	up := 0.0
	if m.up {
		up = 1
	}
	one("auto_gateway_up", "gauge", "1 while the session is READY or RESUMED.", up)
	one("auto_identifies_24h", "gauge", "IDENTIFYs sent in the last 24 hours (Discord resets the token past 1000).", float64(len(m.identifies)))
	one("auto_resumes_total", "counter", "RESUMEs sent.", float64(m.resumes))
	one("auto_reconnects_total", "counter", "Sockets opened after the first.", float64(m.reconnects))
	one("auto_heartbeat_latency_ms", "gauge", "The last heartbeat's round trip.", m.latencyMs)
	one("auto_guilds_bound", "gauge", "Servers with Clippet Auto on, as the plane lists them.", float64(m.guilds))
	labelled := func(name, help, key string, vals map[string]uint64) {
		head(name, "counter", help)
		keys := make([]string, 0, len(vals))
		for k := range vals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s{%s=%q} %d\n", name, key, k, vals[k])
		}
	}
	labelled("auto_videos_seen_total", "Applicable videos seen in a server with Auto on.", "mode", m.videos)
	labelled("auto_uploads_total", "Uploads dispatched to a box, by outcome.", "outcome", m.uploads)
	head("auto_dispatch_ms", "histogram", "An upload's dispatch to its box, in milliseconds.")
	var cum uint64
	for i, le := range dispatchBuckets {
		cum += m.dispatch[i]
		fmt.Fprintf(&b, "auto_dispatch_ms_bucket{le=\"%g\"} %d\n", le, cum)
	}
	cum += m.dispatch[len(dispatchBuckets)]
	fmt.Fprintf(&b, "auto_dispatch_ms_bucket{le=\"+Inf\"} %d\nauto_dispatch_ms_sum %g\nauto_dispatch_ms_count %d\n", cum, m.dispatchSum, m.dispatchN)
	one("auto_queue_depth", "gauge", "Uploads waiting, every server.", float64(m.queue))
	one("auto_rest_429_total", "counter", "429s from Discord's REST.", float64(m.rest429))
	one("auto_plane_errors_total", "counter", "Plane calls that failed.", float64(m.planeErrs))
	_, _ = io.WriteString(w, b.String())
}
