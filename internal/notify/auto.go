// Clippet Auto's network half (§20 erratum 333, DEPLOY §30.10): the gateway
// socket and the REST calls internal/autobot makes, built HERE because this is
// the one package allowed to build an outbound client (§15, §20 errata 66/96).
// internal/autobot holds the protocol and the modes; it is handed AutoClient's
// Call and Dial and never touches the network itself.
//
// Two destinations, and no others:
//
//	discord.com/api/v10/…           REST (config.DiscordAPIHost, as the bot's)
//	gateway.discord.gg, and the regional gateway-….discord.gg Discord names
//	                                the socket (config.ValidGatewayURL), checked
//	                                again here before every dial
//
// The token goes in the Authorization header of a REST call that asks for it
// and in the IDENTIFY/RESUME payloads autobot writes on the socket — nowhere
// else. Nothing returned from here names a URL (an interaction token rides in
// a callback's path) or the token.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"clippet/internal/autobot"
)

// autoCallTimeout bounds one REST call; autoAnswerMax the body kept.
const (
	autoCallTimeout = 10 * time.Second
	autoAnswerMax   = 64 << 10
	// autoReadLimit bounds one gateway frame. READY and GUILD_CREATE carry the
	// server list and its channels; 8 MiB is far above a self-hosted bot's.
	autoReadLimit = 8 << 20
)

// AutoDeps configures an AutoClient.
type AutoDeps struct {
	Token string // SECRET
	// APIHost / APIScheme as BotDeps' (tests point them at httptest).
	APIHost   string
	APIScheme string
	// AllowGateway is config.ValidGatewayURL in production. A test passes a
	// rule that admits its own server.
	AllowGateway func(string) (*url.URL, error)
	// Transport overrides the client's transport (tests only).
	Transport http.RoundTripper
}

// AutoClient makes Clippet Auto's REST calls and opens its socket.
type AutoClient struct {
	token   string
	scheme  string
	host    string
	allowGW func(string) (*url.URL, error)
	client  *http.Client
}

// NewAutoClient returns the client, or nil without a token or a host.
func NewAutoClient(d AutoDeps) *AutoClient {
	if d.Token == "" || d.APIHost == "" || d.AllowGateway == nil {
		return nil
	}
	scheme := d.APIScheme
	if scheme == "" {
		scheme = "https"
	}
	return &AutoClient{token: d.Token, scheme: scheme, host: d.APIHost,
		allowGW: d.AllowGateway, client: newHTTPClient(0, d.Transport)}
}

// errAutoCall is every transport failure, worded once: the cause's text could
// carry the URL, and with it an interaction token.
var errAutoCall = errors.New("the Discord call did not complete")

// Call is autobot.Caller.
func (c *AutoClient) Call(ctx context.Context, method, path string, body []byte, auth bool) (autobot.Response, error) {
	cctx, cancel := context.WithTimeout(ctx, autoCallTimeout)
	defer cancel()
	// A path may carry its query (the backfill's `?after=…&limit=…`): it goes
	// in RawQuery, never in Path, where `?` would be escaped as `%3F` and the
	// call would name another resource (§20 erratum 347 (e), found by the
	// end-to-end bundle).
	p, q, _ := strings.Cut(path, "?")
	endpoint := (&url.URL{Scheme: c.scheme, Host: c.host, Path: botAPIPath + p, RawQuery: q}).String()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(cctx, method, endpoint, rd)
	if err != nil {
		return autobot.Response{}, errAutoCall
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bot "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return autobot.Response{}, errAutoCall
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, autoAnswerMax))
	return autobot.Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: answer}, nil
}

// Dial is autobot.Dialer: the URL is checked against the gateway rule again,
// then opened with this package's one client (no redirect followed), no
// compression, and a frame limit.
func (c *AutoClient) Dial(ctx context.Context, rawURL string) (autobot.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errAutoCall
	}
	bare := *u
	bare.RawQuery = ""
	if _, err := c.allowGW(bare.String()); err != nil {
		return nil, errAutoCall
	}
	conn, resp, err := websocket.Dial(ctx, rawURL, &websocket.DialOptions{
		HTTPClient:      c.client,
		CompressionMode: websocket.CompressionDisabled,
		HTTPHeader:      http.Header{"User-Agent": []string{userAgent}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, errAutoCall
	}
	conn.SetReadLimit(autoReadLimit)
	return &autoConn{c: conn}, nil
}

type autoConn struct{ c *websocket.Conn }

// gatewayClosed is a Read error naming the close code the peer sent.
type gatewayClosed struct{ code int }

func (e *gatewayClosed) Error() string  { return "the gateway closed the socket" }
func (e *gatewayClosed) CloseCode() int { return e.code }

func (a *autoConn) Read(ctx context.Context) ([]byte, error) {
	_, p, err := a.c.Read(ctx)
	if err != nil {
		if code := websocket.CloseStatus(err); code != -1 {
			return nil, &gatewayClosed{code: int(code)}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errAutoCall
	}
	return p, nil
}

func (a *autoConn) Write(ctx context.Context, p []byte) error {
	if err := a.c.Write(ctx, websocket.MessageText, p); err != nil {
		return errAutoCall
	}
	return nil
}

func (a *autoConn) Close(code int, reason string) error {
	return a.c.Close(websocket.StatusCode(code), reason)
}

// Me is the doctor's read-only probe: GET /users/@me answers (the token is
// live), and GET /applications/@me carries the Message Content intent's flag
// (GATEWAY_MESSAGE_CONTENT 1<<18, or _LIMITED 1<<19). The answers are reduced
// to booleans; nothing of them is printed.
func (c *AutoClient) Me(ctx context.Context) (tokenOK, contentIntent bool, err error) {
	r, err := c.Call(ctx, http.MethodGet, "/users/@me", nil, true)
	if err != nil {
		return false, false, err
	}
	if r.Status/100 != 2 {
		return false, false, nil
	}
	r, err = c.Call(ctx, http.MethodGet, "/applications/@me", nil, true)
	if err != nil || r.Status/100 != 2 {
		return true, false, err
	}
	flags, ok := appFlags(r.Body)
	return true, ok && flags&(1<<18|1<<19) != 0, nil
}

// appFlags reads an application object's `flags`.
func appFlags(body []byte) (int64, bool) {
	var a struct {
		Flags *int64 `json:"flags"`
	}
	if json.Unmarshal(body, &a) != nil || a.Flags == nil {
		return 0, false
	}
	return *a.Flags, true
}
