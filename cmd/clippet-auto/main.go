// Command clippet-auto is the HOSTED Clippet Auto (DESIGN.md §20 erratum 334;
// clippet-pass autohost/ builds and deploys it): ONE process holding the
// `Clippet Auto` application's one gateway session for every server that
// turned Auto on in the official bot's `/settings`. For a video posted there
// it does what Clippet Auto on the box does (internal/autobot: Prompt or Auto,
// the timer, the notice, the delete, the permission lines) — except that the
// upload is DISPATCHED to that server's own Clippet through its sealed Clippet
// Auto URL (`POST /in/auto/{token}`, erratum 324): the box fetches the file
// from Discord's CDN itself, and this process never holds a video.
//
// Configuration is the environment and nothing else (no file, no volume):
//
//	DISCORD_AUTO_TOKEN  the Auto application's bot token (SECRET)
//	AUTO_PLANE_URL      the claim Worker's base (its /claim URL), https
//	AUTO_PLANE_SECRET   the bearer for the plane's /auto routes (SECRET)
//	AUTO_SEAL_PRIVATE   the P-256 private JWK that opens the sealed URLs (SECRET)
//	AUTO_LISTEN         the internal listener for /metrics and /healthz (:9091)
//	AUTO_DEADMAN_URL    optional: a GET every 60 s while the session is healthy
//	                    and Discord's REST reads answer (withheld while the last
//	                    read of the minute failed; §20 erratum 334 (i))
//	AUTO_LOG            json (default) | text
//	AUTO_TEST_ENDPOINTS TEST-ONLY, never production (§20 erratum 347): 1 lets a
//	                    name under the reserved .test domain reach a private
//	                    address — the end-to-end bundle's own network. Refused
//	                    unless DISCORD_AUTO_TOKEN is not a Discord token, the
//	                    plane is a .test name and no dead-man is set
//
// Exit: 0 on SIGINT/SIGTERM after a clean stop (the session saved, the socket
// closed RESUMABLE so the next start RESUMEs), 2 on a configuration error. A
// halt (a refused token, a disallowed intent) does NOT exit: a restart would
// IDENTIFY into the same refusal and spend Discord's daily budget, so the
// process stays up with /healthz at 503 and says why in its log.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"clippet/internal/autobot"
	"clippet/internal/config"
	"clippet/internal/notify"
)

// envTestEndpoints is the one test-only knob (§20 erratum 347).
const envTestEndpoints = "AUTO_TEST_ENDPOINTS"

// version is the clippet tag the image was built from (clippet-pass
// autohost/Dockerfile stamps it with -ldflags); "dev" otherwise.
var version = "dev"

// envConfig is the whole configuration.
type envConfig struct {
	token, planeURL, planeSecret, sealPrivate, listen, deadman, logFormat string
	// testEndpoints is AUTO_TEST_ENDPOINTS=1, checked by testEndpointsOK.
	testEndpoints bool
}

// String never prints a secret.
func (c envConfig) String() string {
	return fmt.Sprintf("listen=%s log=%s deadman=%t plane_set=%t test_endpoints=%t", c.listen, c.logFormat, c.deadman != "", c.planeURL != "",
		c.testEndpoints)
}

// loadEnv reads and checks the configuration; the error names the variable.
func loadEnv(get func(string) string) (envConfig, error) {
	c := envConfig{
		token:       strings.TrimSpace(get("DISCORD_AUTO_TOKEN")),
		planeURL:    strings.TrimSpace(get("AUTO_PLANE_URL")),
		planeSecret: strings.TrimSpace(get("AUTO_PLANE_SECRET")),
		sealPrivate: strings.TrimSpace(get("AUTO_SEAL_PRIVATE")),
		listen:      strings.TrimSpace(get("AUTO_LISTEN")),
		deadman:     strings.TrimSpace(get("AUTO_DEADMAN_URL")),
		logFormat:   strings.TrimSpace(get("AUTO_LOG")),
	}
	switch strings.TrimSpace(get(envTestEndpoints)) {
	case "":
	case "1":
		c.testEndpoints = true
	default:
		return c, errors.New(envTestEndpoints + " must be 1 or unset")
	}
	if c.listen == "" {
		c.listen = ":9091"
	}
	if c.logFormat == "" {
		c.logFormat = "json"
	}
	switch {
	case c.token == "":
		return c, errors.New("DISCORD_AUTO_TOKEN is required")
	case c.planeURL == "":
		return c, errors.New("AUTO_PLANE_URL is required")
	case c.planeSecret == "":
		return c, errors.New("AUTO_PLANE_SECRET is required")
	case c.sealPrivate == "":
		return c, errors.New("AUTO_SEAL_PRIVATE is required")
	case c.logFormat != "json" && c.logFormat != "text":
		return c, errors.New("AUTO_LOG must be json or text")
	}
	if _, _, err := net.SplitHostPort(c.listen); err != nil {
		return c, errors.New("AUTO_LISTEN must be host:port or :port")
	}
	if c.deadman != "" {
		u, err := url.Parse(c.deadman)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return c, errors.New("AUTO_DEADMAN_URL must be an https URL")
		}
	}
	if c.testEndpoints {
		if err := testEndpointsOK(c); err != nil {
			return c, err
		}
	}
	return c, nil
}

// testEndpointsOK is the knob's guard (§20 erratum 347). AUTO_TEST_ENDPOINTS=1
// only ever lets a `.test` name reach a private address, and it is refused
// outright unless all three hold — so a production process can never run with
// it, and a test process can never act on production:
//
//   - the token is NOT shaped like a Discord token: a real one's first segment
//     is the bot's user id, base64 of 17-20 ASCII digits; anything else is a
//     token Discord never issued, which its gateway closes with 4004 — so a
//     process with the knob on holds no real session anywhere
//   - the plane is a name under `.test` (RFC 6761: no public resolver answers
//     for it) — never production's plane, whose bearer and records stay out
//   - no dead-man is set: a test pings nothing real
func testEndpointsOK(c envConfig) error {
	if discordTokenShape(c.token) {
		return errors.New(envTestEndpoints + " needs a DISCORD_AUTO_TOKEN that is not a Discord token (a test's mock gateway; never production)")
	}
	u, err := url.Parse(c.planeURL)
	if err != nil || !notify.TestName(u.Hostname()) {
		return errors.New(envTestEndpoints + " needs an AUTO_PLANE_URL under the reserved .test domain (a test's mock plane; never production's)")
	}
	if c.deadman != "" {
		return errors.New(envTestEndpoints + " refuses AUTO_DEADMAN_URL: a test pings nothing real")
	}
	return nil
}

// discordTokenShape reports whether t could be a token Discord issued: three
// dot-separated segments, the first the base64 (standard or URL alphabet,
// padded or not) of a snowflake — 17 to 20 ASCII digits, the bot's user id.
func discordTokenShape(t string) bool {
	parts := strings.Split(t, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	first := strings.TrimRight(parts[0], "=")
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding} {
		b, err := enc.DecodeString(first)
		if err != nil || len(b) < 17 || len(b) > 20 {
			continue
		}
		digits := true
		for _, c := range b {
			if c < '0' || c > '9' {
				digits = false
				break
			}
		}
		if digits {
			return true
		}
	}
	return false
}

// testEndpointsWarn is the one line a process with the knob on says at start.
const testEndpointsWarn = envTestEndpoints + "=1: a name under .test may reach a private address. This process is a test's, never production's"

// announce is the start's lines: the knob's WARN first when it is on.
func announce(log *slog.Logger, cfg envConfig) {
	if cfg.testEndpoints {
		log.Warn(testEndpointsWarn, "event", "test_endpoints", "outcome", "on")
	}
	log.Info("clippet-auto starting", "version", version, "config", cfg.String(), "gateway_host", config.DiscordGatewayHost())
}

// newLogger is the one logger: JSON on stdout (or text), `ts` for the time.
func newLogger(w io.Writer, format string) *slog.Logger {
	opts := &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			a.Key = "ts"
		}
		return a
	}}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func main() {
	os.Exit(run(os.Getenv, os.Stdout))
}

func run(get func(string) string, out io.Writer) int {
	cfg, err := loadEnv(get)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clippet-auto:", err)
		return 2
	}
	log := newLogger(out, cfg.logFormat)
	// 256 MB Machine: keep the heap well under it unless the operator said
	// otherwise (the burst of GUILD_CREATEs at IDENTIFY is the peak).
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(200 << 20)
	}
	open, err := newOpener(cfg.sealPrivate)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clippet-auto:", err)
		return 2
	}
	hc, err := notify.NewHostedClient(notify.HostedDeps{PlaneURL: cfg.planeURL, PlaneSecret: cfg.planeSecret,
		AllowTestNames: cfg.testEndpoints})
	if err != nil {
		fmt.Fprintln(os.Stderr, "clippet-auto:", err)
		return 2
	}
	dc := notify.NewAutoClient(notify.AutoDeps{Token: cfg.token, APIHost: config.DiscordAPIHost(), AllowGateway: config.ValidGatewayURL})
	if dc == nil {
		fmt.Fprintln(os.Stderr, "clippet-auto: the Discord client could not be built")
		return 2
	}
	m := newMetrics(log)
	host := newPlaneHost(hc, hc, open, m, log)
	reads := &restReads{}
	auto := autobot.New(autobot.Config{
		Mode: autobot.ModeGuild, Log: log, Dial: dc.Dial, Call: reads.wrap(dc.Call), AllowGateway: config.ValidGatewayURL,
		Sessions: planeSessions{plane: hc, m: m}, Observer: m, Backfill: true,
	})
	auto.SetHost(host)
	announce(log, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: cfg.listen, Handler: handler(m), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listener", "event", "listen", "outcome", "failed")
		}
	}()
	// The list first, so the IDENTIFY's burst of GUILD_CREATEs is filtered by
	// it rather than asked server by server; then every 5 min.
	host.refreshList(ctx)
	go func() {
		for sleepCtx(ctx, 5*time.Minute) {
			host.refreshList(ctx)
		}
	}()
	if cfg.deadman != "" {
		go every(ctx, time.Minute, func() { deadman(ctx, hc, m, reads, cfg.deadman, log) })
	}

	done := make(chan struct{})
	go func() { defer close(done); auto.Run(ctx, cfg.token) }()
	select {
	case <-ctx.Done():
	case <-done:
		if why := auto.Halted(); why != "" {
			log.Error("halted; /healthz stays 503 until the cause is fixed and the Machine restarted", "event", "halt", "outcome", why)
		}
		<-ctx.Done()
	}
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = auto.Shutdown(sctx)
	select {
	case <-done:
	case <-sctx.Done():
	}
	_ = srv.Shutdown(sctx)
	log.Info("clippet-auto stopped", "event", "stop", "outcome", "clean")
	return 0
}

// handler serves the two internal routes. Fly's private network reaches it;
// fly.toml publishes no service.
func handler(m *metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.write(w)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if m.healthy() {
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "not ready\n")
	})
	return mux
}

// every runs f now and then every d until ctx ends.
func every(ctx context.Context, d time.Duration, f func()) {
	for {
		f()
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// sleepCtx waits d and reports true, or false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pinger is notify.HostedClient's dead-man call.
type pinger interface {
	Ping(ctx context.Context, rawURL string) bool
}

// deadman pings only while the session is healthy AND the process's Discord
// REST reads answer: a silent switch is the alert (Healthchecks.io, or any
// dead-man's service). The session alone is not enough — it stayed healthy
// through fourteen days of a backfill that 404'd on every read (§20 erratum
// 347 (e)) — so the ping is also withheld for a minute whose last read failed
// (restReads, §20 erratum 334 (i)). A minute with no read at all still pings:
// an idle process is a healthy one. One WARN when the pings stop, one INFO
// when they start again; never a line per minute.
func deadman(ctx context.Context, p pinger, m *metrics, reads *restReads, target string, log *slog.Logger) {
	if !m.healthy() {
		return
	}
	ping, change := reads.tick()
	switch change {
	case readsFailing:
		log.Warn("dead-man ping withheld: the last Discord REST read failed", "event", "deadman", "outcome", "withheld")
	case readsRecovered:
		log.Info("dead-man ping resumed", "event", "deadman", "outcome", "resumed")
	}
	if !ping {
		return
	}
	if !p.Ping(ctx, target) {
		log.Warn("dead-man ping", "event", "deadman", "outcome", "failed")
	}
}

// restReads watches the outcome of Discord's REST READS — every GET this
// process makes: the backfill's channel reads, /gateway/bot before a socket,
// the identity fetch on a RESUME — for the dead-man (§20 erratum 334 (i)).
// The writes (a notice, a delete, an interaction's answer) are left out: their
// refusals are facts about one server (a deleted message, a missing
// permission) that the decision lines already carry, not about this process.
//
// A read FAILED when it did not complete (a transport error) or answered
// anything but 2xx — except a 429, which is the REST manners' own wait
// (autobot's rest.do waits it out and tries again) and counts as neither.
type restReads struct {
	mu         sync.Mutex
	calls      int  // reads with an outcome since the last tick
	lastFailed bool // the latest of them failed
	withheld   bool // the last tick withheld the ping
}

// The transitions tick reports, for the two log lines.
const (
	readsSteady = iota
	readsFailing
	readsRecovered
)

// wrap is call with every GET's outcome recorded.
func (r *restReads) wrap(call autobot.Caller) autobot.Caller {
	return func(ctx context.Context, method, path string, body []byte, auth bool) (autobot.Response, error) {
		resp, err := call(ctx, method, path, body, auth)
		if method == http.MethodGet {
			switch {
			case err != nil:
				r.record(false)
			case resp.Status == http.StatusTooManyRequests:
			default:
				r.record(resp.Status/100 == 2)
			}
		}
		return resp, err
	}
}

func (r *restReads) record(ok bool) {
	r.mu.Lock()
	r.calls++
	r.lastFailed = !ok
	r.mu.Unlock()
}

// tick closes one dead-man interval: ping unless the interval held a read and
// its last one failed; change names the transition, if this tick is one.
func (r *restReads) tick() (ping bool, change int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	failing := r.calls > 0 && r.lastFailed
	r.calls = 0
	switch {
	case failing && !r.withheld:
		r.withheld = true
		return false, readsFailing
	case failing:
		return false, readsSteady
	case r.withheld:
		r.withheld = false
		return true, readsRecovered
	}
	return true, readsSteady
}
