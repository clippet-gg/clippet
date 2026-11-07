package config

import "log/slog"

// Sign in with Discord (ROADMAP #44, DESIGN.md §20 erratum 302): the two
// variables of the INSTANCE's OWN Discord application, used for exactly one
// thing — the OAuth authorization-code exchange that proves which Discord user
// is standing at the login page. They travel together: both unset is the OFF
// state (no route under /api/auth/discord, no button on /login, no client), and
// one without the other is fatal at config.Load for the reason the bot's three
// are (erratum 254): a half-configured login boots and is silently dead.
//
// They are INDEPENDENT of the bot's variables and of `managed` (erratum 279).
// An instance running its own bot application may reuse it here — the OAuth2
// client id IS the application id — and a managed instance, which holds no
// secret of the official application, makes a small application of its own
// (DEPLOY.md §19.6).
const (
	// EnvDiscordLoginClientID is the application's OAuth2 client id — the
	// application snowflake. NOT a secret: it is in the authorize URL every
	// browser is sent to.
	EnvDiscordLoginClientID = "DISCORD_LOGIN_CLIENT_ID"
	// EnvDiscordLoginClientSecret is the application's OAuth2 client secret.
	// It IS a secret: with it and a freshly minted code this binary is the
	// application. It is sent to exactly one place (the token endpoint, by
	// internal/notify), reachable only through Secret(), and redacted
	// everywhere a value can be printed (erratum 84's discipline, a third
	// time).
	EnvDiscordLoginClientSecret = "DISCORD_LOGIN_CLIENT_SECRET"
)

// The client secret's bounds. Discord's secrets are 32 characters of the
// URL-safe base64 alphabet; the range is generous for erratum 254's reason —
// the check exists to catch a value pasted into the wrong variable, not to
// second-guess a format Discord may change. The CHARSET is not decoration: it
// is what lets internal/notify scrub the secret out of an echoed error by a
// plain string match, exactly as the bot token's does.
const (
	minClientSecretChars = 16
	maxClientSecretChars = 128
)

// DiscordLogin is the validated OAuth2 application a browser is sent to when
// it clicks "Sign in with Discord".
//
// **The secret is a SECRET** and is reachable only through Secret(). The
// String()/LogValue() pair renders the harmless half — the client id — so a
// Config printed with %v, handed to slog or walked by `admin doctor` can never
// leak it.
type DiscordLogin struct {
	clientID string
	secret   string // SECRET
}

// Configured reports whether the login exists at all. False is the default
// and the OFF state: nothing under /api/auth/discord is registered, /login
// shows no button and no client is built.
func (d DiscordLogin) Configured() bool { return d.clientID != "" }

// ClientID is the OAuth2 client id (not secret).
func (d DiscordLogin) ClientID() string { return d.clientID }

// Secret is the client secret — the secret. Only internal/notify may put it
// on the wire; nothing may put it in a log line, an error or a DB row.
func (d DiscordLogin) Secret() string { return d.secret }

// String is the redacted rendering: never the secret.
func (d DiscordLogin) String() string {
	if !d.Configured() {
		return "(unset)"
	}
	return "client " + d.clientID + ", secret (redacted)"
}

// LogValue makes slog print the redacted form even if the whole value is
// logged by accident.
func (d DiscordLogin) LogValue() slog.Value { return slog.StringValue(d.String()) }

// discordLogin reads and validates the two variables as ONE value. Nothing it
// says echoes the secret: a rejection names the rule that failed.
func (l *loader) discordLogin() DiscordLogin {
	id := l.raw(EnvDiscordLoginClientID)
	secret := l.raw(EnvDiscordLoginClientSecret)
	if id == "" && secret == "" {
		return DiscordLogin{} // the default: the login does not exist
	}
	if id == "" || secret == "" {
		set := EnvDiscordLoginClientID
		if id == "" {
			set = EnvDiscordLoginClientSecret
		}
		// One variable of a two-variable feature is a typo, not a partial
		// configuration: a login with no secret can never complete an
		// exchange and a login with no client id can never start one, and
		// both would look like Discord's fault from the outside. Fail at boot
		// instead (§4).
		l.fail(EnvDiscordLoginClientID, "Sign in with Discord needs both %s and %s, or neither (set: %s); unset the one you did set to turn it off deliberately (DEPLOY.md §19.6)",
			EnvDiscordLoginClientID, EnvDiscordLoginClientSecret, set)
		return DiscordLogin{}
	}
	if !isSnowflake(id) {
		l.fail(EnvDiscordLoginClientID, "must be the application's numeric client id: %d-%d digits, no spaces (Developer Portal → OAuth2 → CLIENT ID)",
			minSnowflakeDigits, maxSnowflakeDigits)
		return DiscordLogin{}
	}
	if !isClientSecret(secret) {
		// The value is a live credential: name the rule, never the value.
		l.fail(EnvDiscordLoginClientSecret, "must be the client secret as the portal prints it: %d-%d characters of A-Z, a-z, 0-9, '_', '.' and '-' (the value is a secret and is not echoed here)",
			minClientSecretChars, maxClientSecretChars)
		return DiscordLogin{}
	}
	return DiscordLogin{clientID: id, secret: secret}
}

// isClientSecret applies the bot token's charset (isBotToken) at the secret's
// own length bounds.
func isClientSecret(s string) bool {
	if len(s) < minClientSecretChars || len(s) > maxClientSecretChars {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}
