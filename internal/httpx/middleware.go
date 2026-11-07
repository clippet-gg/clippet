package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyLogFields
)

// Middleware is the shape every middleware constructor returns.
type Middleware = func(http.Handler) http.Handler

// RequestID assigns a fresh 16-hex-char id to every request, exposes it as
// the X-Request-ID response header and via RequestIDFrom. Client-supplied
// ids are never trusted.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := newRequestID()
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyRequestID, id)))
		})
	}
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// RequestIDFrom returns the request id installed by RequestID ("" if absent).
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(keyRequestID).(string)
	return id
}

// logFields is the per-request annotation bag AccessLog installs.
type logFields struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

func (f *logFields) set(key string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.attrs {
		if a.Key == key {
			f.attrs[i] = slog.Any(key, value)
			return
		}
	}
	f.attrs = append(f.attrs, slog.Any(key, value))
}

func (f *logFields) snapshot() []slog.Attr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]slog.Attr(nil), f.attrs...)
}

// SetLogField annotates THIS request's access-log line with key=value
// (e.g. storage_read_error, reject reason). It is a no-op when AccessLog is
// not installed, so handlers can call it unconditionally in tests.
func SetLogField(r *http.Request, key string, value any) {
	if f, ok := r.Context().Value(keyLogFields).(*logFields); ok {
		f.set(key, value)
	}
}

// recorder captures status and size for the access log and lets Recover
// know whether headers were already sent.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer so streaming handlers keep working.
func (r *recorder) Flush() {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Written reports whether headers have been sent.
func (r *recorder) Written() bool { return r.wroteHeader }

// AccessLog emits one structured line per request: method, path, status,
// bytes, duration, remote address, request id and every SetLogField
// annotation. Health probes with a good status log at DEBUG; 5xx lines are
// WARN (the §12 doctrine reserves ERROR for invariant violations, which the
// handler itself reports via Recover or SetLogField).
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			fields := &logFields{}
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			ctx := context.WithValue(r.Context(), keyLogFields, fields)
			defer func() {
				attrs := []slog.Attr{
					slog.String("method", r.Method),
					slog.String("path", LogPath(r.URL.Path)),
					slog.Int("status", rec.status),
					slog.Int64("bytes", rec.bytes),
					slog.Duration("dur", time.Since(start)),
					slog.String("remote", r.RemoteAddr),
				}
				if id := RequestIDFrom(ctx); id != "" {
					attrs = append(attrs, slog.String("request_id", id))
				}
				attrs = append(attrs, fields.snapshot()...)
				level := slog.LevelInfo
				switch {
				case rec.status >= 500:
					level = slog.LevelWarn
				case isHealthPath(r.URL.Path) && rec.status < 400:
					level = slog.LevelDebug
				}
				log.LogAttrs(ctx, level, "http", attrs...)
			}()
			next.ServeHTTP(rec, r.WithContext(ctx))
		})
	}
}

func isHealthPath(p string) bool { return p == "/healthz" || p == "/readyz" }

// Recover turns a handler panic into a 500 envelope (when nothing has been
// written yet) and an ERROR log line with the stack. http.ErrAbortHandler is
// re-panicked as net/http expects.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				if p == http.ErrAbortHandler {
					panic(p)
				}
				log.LogAttrs(r.Context(), slog.LevelError, "panic",
					slog.Any("err", p),
					slog.String("method", r.Method),
					slog.String("path", LogPath(r.URL.Path)),
					slog.String("request_id", RequestIDFrom(r.Context())),
					slog.String("stack", string(debug.Stack())))
				SetLogField(r, "panic", fmt.Sprint(p))
				if rec, ok := w.(interface{ Written() bool }); ok && rec.Written() {
					return
				}
				WriteError(w, http.StatusInternalServerError, "internal", "internal server error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Content-Security-Policy values (§9.1, §9.2). No 'unsafe-inline' anywhere.
//
// The last three directives are the ones default-src does NOT cover.
// `base-uri` and `form-action` have no fallback at all in CSP3, so leaving
// them out left them unset: a single injected `<base href="//evil">` would
// repoint every relative URL on the page — app.js's own src included — and a
// rewritten form action would post the login credentials off-origin.
// `object-src` does fall back to default-src, but 'self' is the wrong answer
// for a plugin document on a site that has none; 'none' is (§20 erratum 94).
//
// The two media directives are the only ones that are not constant, and they
// are built from CONFIGURATION rather than from a literal (§20 erratum 278).
// CSP re-checks a redirect TARGET by host, so with MEDIA_DELIVERY=redirect a
// `<video src="/api/clips/x/video.mp4">` that 302s to the bucket is blocked on
// our own watch and embed pages — silently, with nothing in the response to
// explain it — unless the bucket's origin is named here. mediaOrigin is the
// resolved presign origin (config.Config.MediaOrigin) and is "" in `proxy`
// mode, where these two directives are byte-for-byte what they always were.
func cspBase(mediaOrigin string) string {
	img, media := "'self' data:", "'self'"
	if mediaOrigin != "" {
		img += " " + mediaOrigin
		media += " " + mediaOrigin
	}
	return "default-src 'self'; img-src " + img + "; media-src " + media +
		"; script-src 'self'; style-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; "
}

// CSPPage is the policy of every page except /embed/{id}; CSPEmbed is the
// embed's. Both take the media origin cspBase explains.
func CSPPage(mediaOrigin string) string  { return cspBase(mediaOrigin) + "frame-ancestors 'none'" }
func CSPEmbed(mediaOrigin string) string { return cspBase(mediaOrigin) + "frame-ancestors *" }

// HSTS is the Strict-Transport-Security value for an https origin: one year,
// subdomains included, and deliberately NO `preload` — preloading is a
// hostname-wide, hard-to-undo commitment that belongs to whoever owns the
// domain, not to an app that happens to be deployed under it.
const HSTS = "max-age=31536000; includeSubDomains"

// SecurityHeaders sets the §15 headers. embed=false: X-Frame-Options DENY
// and frame-ancestors 'none'; embed=true (the /embed/{id} page): no XFO and
// frame-ancestors *. Both set nosniff and the referrer policy.
//
// https says the PUBLIC origin is https (urls.Builder.IsHTTPS — the same fact
// that decides the Secure cookie flag, passed in rather than sniffed from the
// request, because TLS is terminated upstream and the listener never sees it).
// It adds HSTS, so a user who types the bare hostname is upgraded before the
// plaintext hop that carries the login form. A local http dev stack sends no
// HSTS at all: pinning https on `localhost` would lock the developer out of
// their own machine for a year.
//
// mediaOrigin is the bucket origin the media routes redirect to, or "" in the
// shipped `proxy` mode — see cspBase. The policy string is built ONCE here,
// when the middleware is composed, and not per request.
func SecurityHeaders(embed, https bool, mediaOrigin string) Middleware {
	csp := CSPPage(mediaOrigin)
	if embed {
		csp = CSPEmbed(mediaOrigin)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			if https {
				h.Set("Strict-Transport-Security", HSTS)
			}
			if embed {
				h.Del("X-Frame-Options")
			} else {
				h.Set("X-Frame-Options", "DENY")
			}
			h.Set("Content-Security-Policy", csp)
			next.ServeHTTP(w, r)
		})
	}
}
