// Command mockdiscord is scripts/e2e.d/110-autohost.sh's ONE fake (DESIGN.md
// §20 erratum 347): a test-only program, built by the bundle in the harness's
// Go image and copied into a throwaway container — never part of the product
// image (the Dockerfile's build context excludes scripts/, and nothing under
// cmd/ or internal/ imports it).
//
// On a private docker network it answers for every host the hosted Clippet
// Auto talks to, under each host's real name (network aliases) and over TLS
// with a certificate from its own CA (the processes trust it by SSL_CERT_FILE):
//
//	discord.com            Discord's REST: records every call, answers what the
//	                       process needs (GET /gateway/bot, messages, callbacks)
//	gateway.discord.gg     the gateway: HELLO, READY, two GUILD_CREATEs, a video
//	gateway-e2e.discord.gg MESSAGE_CREATE per server, a press, op 7; then RESUME
//	                       is expected on the regional resume host
//	cdn.discordapp.com     the attachment the box fetches
//	plane.clippet.test     the claim plane: /auto/guilds, /auto/guild/{id} with a
//	                       sealed Clippet Auto URL, /auto/session load and save
//	box.clippet.test       the box's edge (a tunnel's part): proxies to the box
//
// Subcommands: `serve` (the default), `state` (prints the record), `wait
// <phase> <seconds>` (exit 0 once `phase <name>` is recorded, 1 on time out).
// The record is plain lines on 127.0.0.1:8090, which nothing off the container
// reaches; the bundle reads it with `docker exec`.
//
// scripts/e2e.d/111-autobot.sh (Clippet Auto ON THE BOX, erratum 333) runs the
// same program with four settings whose defaults are bundle 110's run:
//
//	MOCK_TOKEN         the token the process must present (required)
//	MOCK_MODES         each scripted server's mode, in order: server A, then
//	                   B (default `auto,prompt`; `auto` or `prompt` alone
//	                   scripts server A only)
//	MOCK_SCRIPT        `hosted` (default) or `box`: the box keeps no session
//	                   and is never restarted, so `box` skips the save wait,
//	                   the duplicate event after RESUME and the third socket
//	MOCK_PLANE_SECRET  the plane's bearer (required by `hosted`; without it
//	                   every plane call is refused)
//	MOCK_BOX           the box's plain listener behind box.clippet.test
//	                   (required by `hosted`; without it the name is unknown)
package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The fixed ids (snowflakes) of the script.
const (
	guildA   = "1430000000000000001" // MOCK_MODES' first: Auto, notice on, by default
	guildB   = "1430000000000000002" // MOCK_MODES' second: Prompt, notice off, by default
	chanA    = "1430000000000000011"
	chanB    = "1430000000000000012"
	selfID   = "1430000000000000021"
	appID    = "1430000000000000022"
	posterID = "1430000000000000031"
	msgA     = "1430000000000000101"
	msgB     = "1430000000000000102"
	attA     = "1430000000000000111"
	attB     = "1430000000000000112"
	replyA   = "1430000000000000202" // the offer's id in server A's channel
	replyB   = "1430000000000000201" // …and in server B's
	pressID  = "1430000000000000301"
	pressTok = "mock-itok-1"
	session1 = "mock-session-1"

	boxHost    = "box.clippet.test"
	planeHost  = "plane.clippet.test"
	resumeHost = "gateway-e2e.discord.gg"
	intents    = 1<<0 | 1<<9 | 1<<15
)

var hosts = []string{"discord.com", "gateway.discord.gg", resumeHost, "cdn.discordapp.com", planeHost, boxHost}

// server is one scripted server: its ids, its video's file and its mode (from
// MOCK_MODES; the plane answers notice on for Auto, off for Prompt).
type server struct {
	name, guild, channel, msg, att, reply, file, mode string
}

// allServers are the two the script can hold, in MOCK_MODES' order.
var allServers = []server{
	{name: "A", guild: guildA, channel: chanA, msg: msgA, att: attA, reply: replyA, file: "e2e-auto.mp4"},
	{name: "B", guild: guildB, channel: chanB, msg: msgB, att: attB, reply: replyB, file: "e2e-press.mp4"},
}

// servers is the script's: MOCK_MODES' entries over allServers.
var servers []server

// boxScript is MOCK_SCRIPT=box.
var boxScript bool

func serverOf(guild string) (server, bool) {
	for _, s := range servers {
		if s.guild == guild {
			return s, true
		}
	}
	return server{}, false
}

// replyFor is the offer's id in a channel (replyB for a channel not scripted).
func replyFor(channel string) string {
	for _, s := range allServers {
		if s.channel == channel {
			return s.reply
		}
	}
	return replyB
}

// rec is the record: lines, appended under mu.
type rec struct {
	mu    sync.Mutex
	lines []string
	sess  []byte // the plane's saved session
}

var R rec

func (r *rec) add(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
	log.Print(line)
}

func (r *rec) has(pred func(string) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines {
		if pred(l) {
			return true
		}
	}
	return false
}

func (r *rec) waitFor(d time.Duration, pred func(string) bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if r.has(pred) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func prefixed(p string) func(string) bool {
	return func(l string) bool { return strings.HasPrefix(l, p) }
}

var (
	dir         string // MOCK_DIR: ca.pem, seal.jwk written; box-url read
	token       string // MOCK_TOKEN: the token the process must present
	planeSecret string // MOCK_PLANE_SECRET
	sealPub     *ecdh.PublicKey
	clipDir     string // MOCK_CLIPS: e2e-auto.mp4 and e2e-press.mp4, one file per video
)

func main() {
	log.SetFlags(0)
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		serve()
	case "state":
		os.Exit(state())
	case "wait":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: mockdiscord wait <phase> <seconds>")
			os.Exit(2)
		}
		secs, _ := strconv.Atoi(os.Args[3])
		os.Exit(wait(os.Args[2], secs))
	default:
		fmt.Fprintln(os.Stderr, "usage: mockdiscord serve|state|wait <phase> <seconds>")
		os.Exit(2)
	}
}

func fetchState() (string, error) {
	resp, err := http.Get("http://127.0.0.1:8090/state")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func state() int {
	s, err := fetchState()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mockdiscord: the record could not be read")
		return 1
	}
	fmt.Print(s)
	return 0
}

func wait(phase string, secs int) int {
	end := time.Now().Add(time.Duration(secs) * time.Second)
	for time.Now().Before(end) {
		if s, err := fetchState(); err == nil {
			for _, l := range strings.Split(s, "\n") {
				if l == "phase "+phase {
					return 0
				}
				if strings.HasPrefix(l, "phase FAILED") {
					fmt.Fprintln(os.Stderr, l)
					return 1
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return 1
}

func serve() {
	dir = envOr("MOCK_DIR", "/tmp/mock")
	token = os.Getenv("MOCK_TOKEN")
	planeSecret = os.Getenv("MOCK_PLANE_SECRET")
	clipDir = envOr("MOCK_CLIPS", "/tmp")
	upstream := os.Getenv("MOCK_BOX")
	switch envOr("MOCK_SCRIPT", "hosted") {
	case "hosted":
	case "box":
		boxScript = true
	default:
		log.Fatal("mockdiscord: MOCK_SCRIPT is hosted or box")
	}
	modes := strings.Split(envOr("MOCK_MODES", "auto,prompt"), ",")
	if len(modes) > len(allServers) {
		log.Fatal("mockdiscord: MOCK_MODES names at most two servers")
	}
	for i, m := range modes {
		if m != "auto" && m != "prompt" {
			log.Fatal("mockdiscord: each MOCK_MODES entry is auto or prompt")
		}
		s := allServers[i]
		s.mode = m
		servers = append(servers, s)
	}
	if token == "" || (!boxScript && (planeSecret == "" || upstream == "")) {
		log.Fatal("mockdiscord: MOCK_TOKEN is required, and MOCK_PLANE_SECRET and MOCK_BOX with MOCK_SCRIPT=hosted")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	cert, err := makeCA()
	if err != nil {
		log.Fatal(err)
	}
	if err := makeSealKey(); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
		R.mu.Lock()
		defer R.mu.Unlock()
		_, _ = io.WriteString(w, strings.Join(R.lines, "\n")+"\n")
	})
	go func() { log.Fatal(http.ListenAndServe("127.0.0.1:8090", mux)) }()

	var box http.Handler
	if upstream != "" {
		box = boxProxy(upstream)
	}
	srv := &http.Server{
		Addr:              ":443",
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}},
		// HTTP/1.1 only: a WebSocket upgrade is an HTTP/1.1 request.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := strings.ToLower(r.Host)
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			switch host {
			case "discord.com":
				restHandler(w, r)
			case "gateway.discord.gg", resumeHost:
				gatewayHandler(w, r, host)
			case "cdn.discordapp.com":
				cdnHandler(w, r)
			case planeHost:
				planeHandler(w, r)
			case boxHost:
				if box == nil {
					R.add("unknown-host %s %s %s", host, r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				box.ServeHTTP(w, r)
			default:
				R.add("unknown-host %s %s %s", host, r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		}),
	}
	ln, err := net.Listen("tcp", ":443")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(dir+"/ready", []byte("ok\n"), 0o644); err != nil {
		log.Fatal(err)
	}
	R.add("phase listening")
	log.Fatal(srv.ServeTLS(ln, "", ""))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// makeCA writes ca.pem and answers a leaf for every host, signed by it.
func makeCA() (tls.Certificate, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "clippet e2e mock CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "clippet e2e mock"}, DNSNames: hosts,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(dir+"/ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, nil
}

// makeSealKey writes seal.jwk (the private JWK cmd/clippet-auto's opener
// reads as AUTO_SEAL_PRIVATE) and keeps the public half to seal with.
func makeSealKey() error {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	sealPub = k.PublicKey()
	pub := sealPub.Bytes() // 0x04 ‖ X ‖ Y
	b64 := base64.RawURLEncoding.EncodeToString
	jwk, _ := json.Marshal(map[string]string{"kty": "EC", "crv": "P-256", "x": b64(pub[1:33]), "y": b64(pub[33:65]), "d": b64(k.Bytes())})
	return os.WriteFile(dir+"/seal.jwk", jwk, 0o600)
}

// seal is clippet-pass forward/src/claim/seal.ts's format, which
// cmd/clippet-auto/seal.go opens: 0x01 ‖ ephemeral ‖ IV ‖ AES-256-GCM, the key
// HKDF-SHA256(ECDH, ephemeral ‖ recipient, "clippet-onepress-v1"), AAD
// "v1|<guild>|<host>".
func seal(plain, guild, host string) (string, error) {
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	shared, err := eph.ECDH(sealPub)
	if err != nil {
		return "", err
	}
	ephRaw := eph.PublicKey().Bytes()
	key, err := hkdf.Key(sha256.New, shared, append(append([]byte{}, ephRaw...), sealPub.Bytes()...), "clippet-onepress-v1", 32)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	blob := append([]byte{0x01}, ephRaw...)
	blob = append(blob, iv...)
	blob = gcm.Seal(blob, iv, []byte(plain), []byte("v1|"+guild+"|"+host))
	return base64.RawURLEncoding.EncodeToString(blob), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// Discord's REST.

func restHandler(w http.ResponseWriter, r *http.Request) {
	auth := "none"
	switch h := r.Header.Get("Authorization"); {
	case h == "Bot "+token:
		auth = "ok"
	case h != "":
		auth = "bad"
	}
	var body map[string]any
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	_ = json.Unmarshal(raw, &body)
	p := strings.TrimPrefix(r.URL.Path, "/api/v10")
	seg := strings.Split(strings.Trim(p, "/"), "/")
	line := fmt.Sprintf("rest %s %s auth=%s", r.Method, r.URL.Path, auth)
	switch {
	case r.Method == http.MethodGet && p == "/gateway/bot":
		R.add("%s", line)
		writeJSON(w, 200, map[string]any{"url": "wss://gateway.discord.gg", "shards": 1,
			"session_start_limit": map[string]any{"total": 1000, "remaining": 999, "reset_after": 0, "max_concurrency": 1}})
	case r.Method == http.MethodGet && len(seg) == 3 && seg[0] == "channels" && seg[2] == "messages":
		R.add("%s after=%s limit=%s", line, r.URL.Query().Get("after"), r.URL.Query().Get("limit"))
		writeJSON(w, 200, []any{})
	case r.Method == http.MethodPost && len(seg) == 3 && seg[0] == "channels" && seg[2] == "messages":
		content, _ := body["content"].(string)
		if _, offer := body["components"]; offer {
			custom := ""
			if b, err := json.Marshal(body["components"]); err == nil && bytes.Contains(b, []byte(`"custom_id":"auto:go"`)) {
				custom = "auto:go"
			}
			ref, _ := body["message_reference"].(map[string]any)
			// The answer first, the line after it: the process learns the reply's id
			// from this response, and the script presses the button the moment the
			// line appears — recorded before the answer, a press under load beat the
			// id into the process's waiting table and was read as a stale button
			// (gate 56's first attempt, 2026-10-27; a person cannot press a button
			// before Discord has answered the message that carries it).
			writeJSON(w, 200, map[string]any{"id": replyFor(seg[1]), "channel_id": seg[1]})
			R.add("%s kind=offer custom_id=%s reply_to=%v", line, custom, ref["message_id"])
			return
		}
		R.add("%s kind=post content=%s", line, strconv.Quote(content))
		writeJSON(w, 200, map[string]any{"id": "1430000000000000401", "channel_id": seg[1]})
	case r.Method == http.MethodDelete && len(seg) == 4 && seg[0] == "channels" && seg[2] == "messages":
		R.add("%s", line)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && len(seg) == 4 && seg[0] == "interactions" && seg[3] == "callback":
		data, _ := body["data"].(map[string]any)
		R.add("%s type=%v content=%s flags=%v", line, body["type"], strconv.Quote(fmt.Sprint(data["content"])), data["flags"])
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && len(seg) == 3 && seg[0] == "webhooks":
		content, _ := body["content"].(string)
		R.add("%s kind=followup content=%s", line, strconv.Quote(content))
		writeJSON(w, 200, map[string]any{"id": "1430000000000000501"})
	case r.Method == http.MethodGet && p == "/users/@me":
		R.add("%s", line)
		writeJSON(w, 200, map[string]any{"id": selfID})
	case r.Method == http.MethodGet && p == "/applications/@me":
		R.add("%s", line)
		writeJSON(w, 200, map[string]any{"id": appID, "flags": 1 << 18})
	default:
		R.add("%s unknown=1", line)
		writeJSON(w, 404, map[string]any{"message": "Unknown", "code": 0})
	}
}

// ---------------------------------------------------------------------------
// The plane (the claim Worker's /auto routes, under /claim).

func planeHandler(w http.ResponseWriter, r *http.Request) {
	auth := "bad"
	if planeSecret != "" && r.Header.Get("Authorization") == "Bearer "+planeSecret {
		auth = "ok"
	}
	p := strings.TrimPrefix(r.URL.Path, "/claim")
	line := fmt.Sprintf("plane %s %s auth=%s", r.Method, p, auth)
	if auth != "ok" {
		R.add("%s", line)
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	switch {
	case r.Method == http.MethodGet && p == "/auto/guilds":
		R.add("%s", line)
		ids := make([]string, 0, len(servers))
		for _, s := range servers {
			ids = append(ids, s.guild)
		}
		writeJSON(w, 200, map[string]any{"guilds": ids, "routable": ids})
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/auto/guild/"):
		g := strings.TrimPrefix(p, "/auto/guild/")
		target, err := os.ReadFile(dir + "/box-url")
		srv, scripted := serverOf(g)
		if err != nil || !scripted {
			R.add("%s answer=404", line)
			writeJSON(w, 404, map[string]any{"error": "not_found"})
			return
		}
		u, err := seal(strings.TrimSpace(string(target)), g, boxHost)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "seal"})
			return
		}
		mode, notice := srv.mode, srv.mode == "auto"
		R.add("%s answer=%s", line, mode)
		writeJSON(w, 200, map[string]any{"on": true, "mode": mode, "s": 60, "n": notice, "u": u, "host": boxHost})
	case r.Method == http.MethodGet && p == "/auto/session":
		R.mu.Lock()
		s := R.sess
		R.mu.Unlock()
		if s == nil {
			R.add("%s answer=404", line)
			writeJSON(w, 404, map[string]any{"error": "not_found"})
			return
		}
		R.add("%s answer=200", line)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s)
	case r.Method == http.MethodPut && p == "/auto/session":
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		var s struct {
			ID        string `json:"session_id"`
			Seq       int64  `json:"seq"`
			ResumeURL string `json:"resume_gateway_url"`
			Last      string `json:"last"`
			UserID    string `json:"user_id"`
			AppID     string `json:"app_id"`
		}
		if json.Unmarshal(raw, &s) != nil {
			R.add("%s malformed=1", line)
			writeJSON(w, 400, map[string]any{"error": "malformed"})
			return
		}
		R.mu.Lock()
		R.sess = raw
		R.mu.Unlock()
		R.add("%s session=%s seq=%d resume=%s last=%s user=%s app=%s", line, s.ID, s.Seq, s.ResumeURL, s.Last, s.UserID, s.AppID)
		w.WriteHeader(http.StatusNoContent)
	default:
		R.add("%s unknown=1", line)
		writeJSON(w, 404, map[string]any{"error": "not_found"})
	}
}

// ---------------------------------------------------------------------------
// The CDN: each video's own file (two different files, so the box's dedupe —
// one account, the same bytes — never answers the second with the first), by
// Range when asked.

func clipFile(name string) string {
	if name != "e2e-auto.mp4" && name != "e2e-press.mp4" {
		return ""
	}
	return clipDir + "/" + name
}

func cdnHandler(w http.ResponseWriter, r *http.Request) {
	R.add("cdn %s %s range=%s", r.Method, r.URL.Path, r.Header.Get("Range"))
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if !strings.HasPrefix(r.URL.Path, "/attachments/") || clipFile(name) == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(clipFile(name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, name, time.Unix(0, 0), f)
}

func clipSize(name string) int64 {
	st, err := os.Stat(clipFile(name))
	if err != nil {
		return 0
	}
	return st.Size()
}

// ---------------------------------------------------------------------------
// The box's edge: what a tunnel does in production — the https name in front,
// the box's plain listener behind.

func boxProxy(upstream string) http.Handler {
	target := &url.URL{Scheme: "http", Host: upstream}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = boxHost
		},
		ModifyResponse: func(resp *http.Response) error {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(b))
			var out struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(b, &out)
			path := resp.Request.URL.Path
			if i := strings.Index(path, "/ca_"); i >= 0 && len(path) > i+12 {
				path = path[:i+12] + "…" // the secret, by its display prefix only
			}
			R.add("box %s %s status=%d link=%s", resp.Request.Method, path, resp.StatusCode, out.URL)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			R.add("box %s upstream-error", r.Method)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		var in struct {
			GuildID     string `json:"guild_id"`
			URL         string `json:"url"`
			Filename    string `json:"filename"`
			ContentType string `json:"content_type"`
			Size        int64  `json:"size"`
		}
		_ = json.Unmarshal(b, &in)
		R.add("box-in guild=%s url=%s filename=%s type=%s size=%d ua=%s auth=%t", in.GuildID, in.URL, in.Filename,
			in.ContentType, in.Size, r.Header.Get("User-Agent"), r.Header.Get("Authorization") != "")
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		rp.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// The gateway.

var (
	gwMu    sync.Mutex
	gwConns int
	gwSeq   int // the last sequence dispatched, across sockets (a RESUME continues it)
)

type gwConn struct {
	n   int
	ws  *websocket.Conn
	ctx context.Context
	in  chan map[string]any
	wmu sync.Mutex
}

func (c *gwConn) send(v any) {
	b, _ := json.Marshal(v)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.Write(c.ctx, websocket.MessageText, b)
}

// dispatch sends one event under the next sequence: bundle 110's run is
// READY 1, two GUILD_CREATEs 2-3, the videos 4-5, the press 6; after op 7
// RESUMED 7 and the duplicate 8; the restart's RESUMED 9.
func (c *gwConn) dispatch(t string, d any) {
	gwMu.Lock()
	gwSeq++
	s := gwSeq
	gwMu.Unlock()
	c.send(map[string]any{"op": 0, "t": t, "s": s, "d": d})
}

// next is the next frame with op 2 or 6 (the opening), or nil.
func (c *gwConn) opening(d time.Duration) map[string]any {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case f, ok := <-c.in:
			if !ok {
				return nil
			}
			if op, _ := f["op"].(float64); op == 2 || op == 6 {
				return f
			}
		case <-t.C:
			return nil
		}
	}
}

func gatewayHandler(w http.ResponseWriter, r *http.Request, host string) {
	gwMu.Lock()
	gwConns++
	n := gwConns
	gwMu.Unlock()
	R.add("gw conn=%d host=%s query=%s", n, host, r.URL.RawQuery)
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		R.add("gw conn=%d accept-failed", n)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &gwConn{n: n, ws: ws, ctx: ctx, in: make(chan map[string]any, 64)}
	go func() {
		defer close(c.in)
		for {
			_, p, err := ws.Read(ctx)
			if err != nil {
				if code := websocket.CloseStatus(err); code != -1 {
					R.add("gw conn=%d closed code=%d", n, code)
				}
				cancel()
				return
			}
			var m map[string]any
			if json.Unmarshal(p, &m) != nil {
				continue
			}
			if op, _ := m["op"].(float64); op == 1 {
				c.send(map[string]any{"op": 11})
				continue
			}
			select {
			case c.in <- m:
			default:
			}
		}
	}()
	c.send(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 41250}})
	f := c.opening(30 * time.Second)
	if f == nil {
		R.add("gw conn=%d no-opening", n)
		return
	}
	d, _ := f["d"].(map[string]any)
	tok := "bad"
	if d["token"] == token {
		tok = "ok"
	}
	if op, _ := f["op"].(float64); op == 2 {
		R.add("identify conn=%d token=%s intents=%v", n, tok, d["intents"])
		if n != 1 {
			R.add("phase FAILED identify on conn %d", n)
			return
		}
		firstSession(c)
		return
	}
	R.add("resume conn=%d session=%v seq=%v token=%s", n, d["session_id"], d["seq"], tok)
	switch n {
	case 2:
		c.dispatch("RESUMED", nil)
		R.add("phase resumed")
		if boxScript {
			break // the box acts on a duplicate twice by design (no `last`): not replayed
		}
		// Discord may deliver an event twice: server A's video again, as a new sequence.
		s := servers[0]
		c.dispatch("MESSAGE_CREATE", videoMessage(s.msg, s.guild, s.channel, s.att, s.file))
		time.Sleep(3 * time.Second)
		R.add("phase replayed")
	default:
		c.dispatch("RESUMED", nil)
		R.add("phase restarted")
	}
	<-ctx.Done()
}

func videoMessage(id, guild, channel, att, name string) map[string]any {
	return map[string]any{
		"id": id, "channel_id": channel, "guild_id": guild, "content": "",
		"author": map[string]any{"id": posterID, "username": "poster"},
		"attachments": []any{map[string]any{
			"id": att, "filename": name, "content_type": "video/mp4", "size": clipSize(name),
			"url": "https://cdn.discordapp.com/attachments/" + channel + "/" + att + "/" + name + "?ex=1&is=2&hm=3",
		}},
	}
}

func guildCreate(id, channel string) map[string]any {
	return map[string]any{"id": id, "name": "e2e " + id, "channels": []any{
		map[string]any{"id": channel, "type": 0, "name": "clips"},
		map[string]any{"id": "1430000000000000099", "type": 4, "name": "a category"}, // not read back
	}}
}

// firstSession is conn 1: READY, the servers, a video per server (and the
// press on a Prompt server's offer), the save (hosted), op 7.
func firstSession(c *gwConn) {
	fail := func(what string) { R.add("phase FAILED %s", what) }
	c.dispatch("READY", map[string]any{"v": 10, "session_id": session1,
		"resume_gateway_url": "wss://" + resumeHost, "user": map[string]any{"id": selfID, "bot": true},
		"application": map[string]any{"id": appID}, "guilds": []any{}})
	for _, s := range servers {
		c.dispatch("GUILD_CREATE", guildCreate(s.guild, s.channel))
	}
	R.add("phase ready")

	for _, s := range servers {
		c.dispatch("MESSAGE_CREATE", videoMessage(s.msg, s.guild, s.channel, s.att, s.file))
		if s.mode == "auto" {
			// Auto: the video is uploaded, the link posted, the original removed.
			if !R.waitFor(180*time.Second, prefixed("rest DELETE /api/v10/channels/"+s.channel+"/messages/"+s.msg+" ")) {
				fail("server " + s.name + "'s original was never removed")
				return
			}
			R.add("phase auto")
			continue
		}
		// Prompt: the offer, then the poster presses it.
		if !R.waitFor(30*time.Second, prefixed("rest POST /api/v10/channels/"+s.channel+"/messages auth=ok kind=offer")) {
			fail("server " + s.name + " was never offered the button")
			return
		}
		// A beat between the offer's answer and the press, as between a message
		// appearing and a hand reaching it: the process stores the reply's id
		// right after reading the answer, and the gateway event must not overtake
		// that read (the record line now follows the answer; this is the margin).
		time.Sleep(500 * time.Millisecond)
		c.dispatch("INTERACTION_CREATE", map[string]any{
			"id": pressID, "type": 3, "token": pressTok, "application_id": appID, "guild_id": s.guild, "channel_id": s.channel,
			"data":    map[string]any{"custom_id": "auto:go", "component_type": 2},
			"member":  map[string]any{"permissions": "0", "user": map[string]any{"id": posterID}},
			"message": map[string]any{"id": s.reply, "author": map[string]any{"id": selfID}},
		})
		if !R.waitFor(180*time.Second, prefixed("rest DELETE /api/v10/channels/"+s.channel+"/messages/"+s.reply+" ")) {
			fail("server " + s.name + "'s reply was never removed after the press")
			return
		}
		R.add("phase press")
	}
	if !boxScript {
		// The act's save: the session with `last` = the newest video acted on.
		last := servers[len(servers)-1].msg
		if !R.waitFor(15*time.Second, func(l string) bool {
			return strings.HasPrefix(l, "plane PUT /auto/session auth=ok session="+session1+" ") && strings.Contains(l, " last="+last+" ")
		}) {
			fail("the session was never saved with last=" + last)
			return
		}
		R.add("phase saved")
	}
	c.send(map[string]any{"op": 7, "d": nil})
	R.add("phase op7")
	<-c.ctx.Done()
}
