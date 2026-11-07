// "Sign in with Discord"'s outbound half (ROADMAP #44, DESIGN.md §20 erratum
// 302): the OAuth authorization-code exchange, and the one read that follows
// it. It lives in this package for the bot's reason — internal/notify is the
// ONLY place in the binary allowed to build an HTTP client (§15, §20 erratum
// 66) — and its client comes from the one construction function every other
// caller uses, so redirects are never followed here either.
//
// Two calls, and no others, both on the API host the bot already names:
//
//	POST discord.com/api/v10/oauth2/token     code (+ PKCE verifier) → an access token
//	GET  discord.com/api/v10/users/@me        the access token → {id, username}
//
// The access token is used for that one GET and discarded; the refresh token
// is ignored and never stored. `identify` gives this instance no standing
// capability on anybody's Discord, and it keeps none.
//
// **The client secret is a SECRET** and is scrubbed from every string that
// leaves this package by the same longest-first replacement the bot token
// gets. The code and the access token are per-flow and never appear in an
// error either: no message here embeds one, and the *url.Error wrapper, whose
// text carries the URL, is unwrapped to its cause exactly as call() does.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// loginCallTimeout bounds each of the two calls. Both are small JSON
// exchanges; a Discord that takes longer is a Discord that is down, and the
// person is looking at a spinning browser tab.
const loginCallTimeout = 10 * time.Second

// LoginDeps configures a Login.
//
// APIHost and APIScheme are injected for BotDeps's reason: production passes
// config.DiscordAPIHost() and "https"; the tests drive an httptest server and
// never the network, and that is the only thing that ever makes this package
// speak plain HTTP.
type LoginDeps struct {
	// ClientID is the OAuth2 client id (not secret).
	ClientID string
	// Secret is the client secret (SECRET), sent on the token call alone.
	Secret string
	// APIHost is the Discord API host ("" is not allowed).
	APIHost string
	// APIScheme is "https" in production; a test passes "http".
	APIScheme string
	// Timeout overrides loginCallTimeout (tests only; 0 = the default).
	Timeout time.Duration
	// Transport overrides the client's transport (tests only).
	Transport http.RoundTripper
}

// Login performs the exchange for the instance's own application.
type Login struct {
	clientID string
	secret   string   // SECRET
	secrets  []string // every spelling of the secret, longest first
	scheme   string
	host     string
	timeout  time.Duration
	client   *http.Client
}

// NewLogin returns a Login, or nil when the application is not configured —
// the OFF state, in which no client is built at all.
func NewLogin(d LoginDeps) *Login {
	if d.ClientID == "" || d.Secret == "" || d.APIHost == "" {
		return nil
	}
	scheme := d.APIScheme
	if scheme == "" {
		scheme = "https"
	}
	return &Login{
		clientID: d.ClientID, secret: d.Secret, secrets: tokenSpellings(d.Secret),
		scheme: scheme, host: d.APIHost, timeout: orDuration(d.Timeout, loginCallTimeout),
		// The one construction site (§15): never follows a redirect. Its own
		// Timeout is the per-call budget outright — both calls are one small
		// JSON body each, so unlike the bot's client there is no download to
		// leave unbounded.
		client: newHTTPClient(orDuration(d.Timeout, loginCallTimeout), d.Transport),
	}
}

// DiscordIdentity is what the exchange proves: which Discord user consented.
// The ID decides everything; the two names are display only.
type DiscordIdentity struct {
	ID         string
	Username   string
	GlobalName string
}

// ErrLoginExchange is the class every failure here belongs to: the operator's
// log carries the scrubbed cause, the person sees one sentence.
var ErrLoginExchange = errors.New("discord login: the exchange failed")

// Identify exchanges a code (with its PKCE verifier and the redirect URI the
// code was minted for) for the Discord user behind it. Any failure — a refused
// code, a 5xx, a hung server, a body that is not the shape Discord documents —
// is one error wrapping ErrLoginExchange, scrubbed of the secret.
func (l *Login) Identify(ctx context.Context, code, verifier, redirectURI string) (DiscordIdentity, error) {
	token, err := l.exchange(ctx, code, verifier, redirectURI)
	if err != nil {
		return DiscordIdentity{}, err
	}
	return l.me(ctx, token)
}

// exchange is POST /oauth2/token: the form Discord documents, nothing more.
// The secret travels in the body as `client_secret` (Discord accepts the Basic
// header too; the body is the form its own examples use).
func (l *Login) exchange(ctx context.Context, code, verifier, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {l.clientID},
		"client_secret": {l.secret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := l.call(ctx, http.MethodPost, botAPIPath+"/oauth2/token", strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded", "", &tok); err != nil {
		return "", fmt.Errorf("%w: token: %s", ErrLoginExchange, err.Error())
	}
	if tok.AccessToken == "" || !strings.EqualFold(tok.TokenType, "Bearer") {
		return "", fmt.Errorf("%w: token: the answer carried no bearer token", ErrLoginExchange)
	}
	return tok.AccessToken, nil
}

// me is GET /users/@me with the access token as a Bearer — the `identify`
// scope's one read.
func (l *Login) me(ctx context.Context, token string) (DiscordIdentity, error) {
	var me struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
	}
	if err := l.call(ctx, http.MethodGet, botAPIPath+"/users/@me", nil, "", token, &me); err != nil {
		return DiscordIdentity{}, fmt.Errorf("%w: identity: %s", ErrLoginExchange, err.Error())
	}
	if !isSnowflakeText(me.ID) {
		return DiscordIdentity{}, fmt.Errorf("%w: identity: the answer carried no user id", ErrLoginExchange)
	}
	return DiscordIdentity{ID: me.ID, Username: me.Username, GlobalName: me.GlobalName}, nil
}

// call makes one request and decodes a 2xx JSON body into out. Nothing it
// returns names a URL, the secret, the code or a token: the endpoint is built
// from the allowlisted host and a fixed path, the *url.Error wrapper is
// unwrapped to its cause, a rejection body is read through a window and
// scrubbed before it is cut, and the bearer never appears in any string.
func (l *Login) call(ctx context.Context, method, path string, body io.Reader, contentType, bearer string, out any) error {
	cctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	endpoint := (&url.URL{Scheme: l.scheme, Host: l.host, Path: path}).String()
	req, err := http.NewRequestWithContext(cctx, method, endpoint, body)
	if err != nil {
		return errors.New("request could not be built")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return errors.New(l.scrub(cause(err)))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode/100 != 2 {
		// Includes the 3xx this client refuses to follow.
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, l.bodySnippet(resp.Body))
	}
	// A 2xx body is bounded by the same window a rejection is: Discord's two
	// answers are a few hundred bytes, and a body that is not is not theirs.
	if err := json.NewDecoder(io.LimitReader(resp.Body, bodyWindow)).Decode(out); err != nil {
		return errors.New("the answer was not the JSON Discord documents")
	}
	return nil
}

// bodySnippet renders a rejection body for the log, in the notifier's order:
// read a window, scrub, and only then cut.
func (l *Login) bodySnippet(r io.Reader) string {
	s := truncateBytes(l.scrub(readWindow(r)), bodySnippet)
	if s == "" {
		return "(empty body)"
	}
	return "body=" + s
}

// scrub removes every spelling of the client secret from a string on its way
// out.
func (l *Login) scrub(s string) string {
	for _, secret := range l.secrets {
		s = strings.ReplaceAll(s, secret, redacted)
	}
	return s
}

// isSnowflakeText is the bot's snowflake shape (internal/config's isSnowflake,
// which §3 forbids importing): 15-25 digits. It guards the one value that
// becomes a database key.
func isSnowflakeText(s string) bool {
	if len(s) < 15 || len(s) > 25 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
