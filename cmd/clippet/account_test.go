package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
)

// runIn is fx.run with a stdin, for `users create`.
func (fx *adminFixture) runIn(t *testing.T, stdin string, args ...string) (int, string) {
	t.Helper()
	fx.out.Reset()
	fx.errOut.Reset()
	code := runAdminWith(args, fx.cfg, adminEnv{
		stdout:    &fx.out,
		stderr:    &fx.errOut,
		stdin:     strings.NewReader(stdin),
		openStore: func(config.Config) (store.Store, error) { return fx.fake, nil },
		now:       func() time.Time { return fx.now },
	})
	return code, fx.out.String() + fx.errOut.String()
}

// seedSessions gives `carol` three live sessions and one API token, and
// returns the second user it also creates.
func (fx *adminFixture) seedSessions(t *testing.T) (db.User, db.User) {
	t.Helper()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	carol, err := d.GetUserByUsername(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	friend, err := d.CreateUser(ctx, ids.New(), "friend", "h", 1)
	if err != nil {
		t.Fatal(err)
	}
	now := fx.now.Unix()
	for _, s := range []struct{ hash, user, ua string }{
		{"k1", carol.ID, "Firefox on the laptop"},
		{"k2", carol.ID, "Chrome on the phone"},
		{"f1", friend.ID, "Safari"},
	} {
		if err := d.CreateSession(ctx, s.hash, s.user, now, now+3600, now+7200, s.ua); err != nil {
			t.Fatal(err)
		}
	}
	for _, tk := range []db.APIToken{
		{ID: "tok-one", UserID: carol.ID, Name: "ShareX", TokenHash: "th1", Prefix: "aaaaaaaa",
			Scopes: []string{"upload"}, CreatedAt: now},
		{ID: "tok-two", UserID: carol.ID, Name: "backup", TokenHash: "th2", Prefix: "bbbbbbbb",
			Scopes: []string{"read"}, CreatedAt: now},
		{ID: "tok-friend", UserID: friend.ID, Name: "theirs", TokenHash: "th3", Prefix: "cccccccc",
			Scopes: []string{"upload"}, CreatedAt: now},
	} {
		if err := d.CreateToken(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	return carol, friend
}

// `clippet admin sessions` (§20 erratum 109): list, revoke one user, revoke
// everybody — and never a default that means "everything".
func TestAdminSessions(t *testing.T) {
	fx := newAdminFixture(t)
	_, _ = fx.seedSessions(t)

	// A bare verb, an unknown sub-verb, and a revoke with no selector are all
	// usage errors — the `users prune` guard, applied here.
	for _, args := range [][]string{
		{"sessions"},
		{"sessions", "wat"},
		{"sessions", "revoke"},
		{"sessions", "list"},
		{"sessions", "revoke", "carol", "--all"}, // a username AND --all is not a selector, it is a mistake
	} {
		if code, out := fx.run(t, args...); code != 2 || !strings.Contains(out, "usage") {
			t.Errorf("%v: %d %q", args, code, out)
		}
	}
	code, out := fx.run(t, "sessions", "list", "carol")
	if code != 0 || !strings.Contains(out, "2 live session(s) for carol") {
		t.Fatalf("list: %d %q", code, out)
	}
	if !strings.Contains(out, "Firefox on the laptop") || strings.Contains(out, "Safari") {
		t.Errorf("list shows the wrong sessions: %q", out)
	}
	// The hash never reaches an operator's terminal.
	if strings.Contains(out, "k1") || strings.Contains(out, "k2") {
		t.Errorf("the session list printed a token hash: %q", out)
	}
	if code, out = fx.run(t, "sessions", "list", "nobody"); code != 1 || !strings.Contains(out, "no such user") {
		t.Errorf("unknown user: %d %q", code, out)
	}

	// Revoke one user: theirs go, the friend's stays.
	if code, out = fx.run(t, "sessions", "revoke", "carol"); code != 0 || !strings.Contains(out, "revoked 2 session(s) for carol") {
		t.Fatalf("revoke carol: %d %q", code, out)
	}
	if code, out = fx.run(t, "sessions", "list", "carol"); code != 0 || !strings.Contains(out, "0 live session(s)") {
		t.Errorf("after revoke: %d %q", code, out)
	}
	if code, out = fx.run(t, "sessions", "list", "friend"); code != 0 || !strings.Contains(out, "1 live session(s)") {
		t.Errorf("the friend must be untouched: %d %q", code, out)
	}
	// …and the big hammer, which has to be spelled out.
	if code, out = fx.run(t, "sessions", "revoke", "--all"); code != 0 || !strings.Contains(out, "revoked 1 session(s) across every account") {
		t.Fatalf("revoke --all: %d %q", code, out)
	}
}

// `max_expires_at <= 0` is migration 0005's "no ceiling recorded" sentinel,
// which every read in internal/db guards for. Printed through rfc() it read
// 1970-01-01T00:00:00Z — a session that looks like it died decades ago rather
// than one with no ceiling on record. Production never writes such a row
// (auth.CreateSession always sets a ceiling), but ListSessions returns it, so
// the CLI must say what it is (N021).
func TestAdminSessionsListRendersTheNoCeilingSentinel(t *testing.T) {
	fx := newAdminFixture(t)
	ctx := context.Background()
	seed := func(hash string, maxExpires int64, ua string) {
		t.Helper()
		d, err := openForTest(fx.cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		u, err := d.GetUserByUsername(ctx, "carol")
		if err != nil {
			t.Fatal(err)
		}
		now := fx.now.Unix()
		if err := d.CreateSession(ctx, hash, u.ID, now, now+3600, maxExpires, ua); err != nil {
			t.Fatal(err)
		}
	}
	seed("legacy", 0, "an older binary")
	code, out := fx.run(t, "sessions", "list", "carol")
	if code != 0 || !strings.Contains(out, "max=none") {
		t.Fatalf("a 0 ceiling must render max=none: %d %q", code, out)
	}
	if strings.Contains(out, "1970") {
		t.Fatalf("the sentinel was printed as a date: %q", out)
	}
	// A real ceiling still prints as one, so the fix did not hide the column.
	seed("modern", fx.now.Unix()+7200, "a current binary")
	if code, out = fx.run(t, "sessions", "list", "carol"); code != 0 || !strings.Contains(out, "max="+rfc(fx.now.Unix()+7200)) {
		t.Fatalf("a real ceiling must still print: %d %q", code, out)
	}
}

// `clippet admin tokens` (§20 erratum 109). A username alone is deliberately
// not a selector for `revoke`.
func TestAdminTokens(t *testing.T) {
	fx := newAdminFixture(t)
	fx.seedSessions(t)

	for _, args := range [][]string{
		{"tokens"},
		{"tokens", "wat"},
		{"tokens", "list"},
		{"tokens", "revoke"},
		{"tokens", "revoke", "carol"}, // no tid and no --all
		{"tokens", "revoke", "carol", "tok-one", "--all"}, // both is a mistake
	} {
		if code, out := fx.run(t, args...); code != 2 || !strings.Contains(out, "usage") {
			t.Errorf("%v: %d %q", args, code, out)
		}
	}
	code, out := fx.run(t, "tokens", "list", "carol")
	if code != 0 || !strings.Contains(out, "2 live token(s) for carol") {
		t.Fatalf("list: %d %q", code, out)
	}
	for _, want := range []string{"tok-one", "ck_aaaaaaaa", "scopes=upload", "scopes=read", `name="backup"`} {
		if !strings.Contains(out, want) {
			t.Errorf("token list lacks %q: %q", want, out)
		}
	}
	// The hash is not a display value, here or anywhere.
	if strings.Contains(out, "th1") || strings.Contains(out, "th2") {
		t.Errorf("the token list printed a hash: %q", out)
	}
	if strings.Contains(out, "tok-friend") {
		t.Errorf("the token list crossed accounts: %q", out)
	}

	// One token by id.
	if code, out = fx.run(t, "tokens", "revoke", "carol", "tok-one"); code != 0 || !strings.Contains(out, "revoked 1 token(s) for carol") {
		t.Fatalf("revoke one: %d %q", code, out)
	}
	// Again: unknown, already revoked and somebody else's are one answer.
	for _, tid := range []string{"tok-one", "tok-friend", "nope"} {
		if code, out = fx.run(t, "tokens", "revoke", "carol", tid); code != 1 || !strings.Contains(out, "no such live token") {
			t.Errorf("revoke %s again: %d %q", tid, code, out)
		}
	}
	// The rest, explicitly.
	if code, out = fx.run(t, "tokens", "revoke", "carol", "--all"); code != 0 || !strings.Contains(out, "revoked 1 token(s) for carol") {
		t.Fatalf("revoke --all: %d %q", code, out)
	}
	if code, out = fx.run(t, "tokens", "list", "friend"); code != 0 || !strings.Contains(out, "1 live token(s) for friend") {
		t.Errorf("the friend's token must survive: %d %q", code, out)
	}
}

// `clippet admin users create` (ROADMAP #21): the account REGISTRATION=first
// leaves the operator to make by hand. The password comes from stdin and
// never from argv.
func TestAdminUsersCreate(t *testing.T) {
	fx := newAdminFixture(t)

	// No name, a bad name, and a password that breaks the API's own rules.
	if code, out := fx.runIn(t, "correct horse", "users", "create"); code != 2 || !strings.Contains(out, "usage") {
		t.Errorf("no name: %d %q", code, out)
	}
	if code, out := fx.runIn(t, "correct horse", "users", "create", "no-dashes-allowed"); code != 2 ||
		!strings.Contains(out, "letters, digits or underscore") {
		t.Errorf("bad name: %d %q", code, out)
	}
	if code, out := fx.runIn(t, "short", "users", "create", "alice"); code != 2 ||
		!strings.Contains(out, "8-72 bytes") {
		t.Errorf("short password: %d %q", code, out)
	}

	// The real thing, with the trailing newline a shell adds.
	code, out := fx.runIn(t, "correct horse battery\n", "users", "create", "alice")
	if code != 0 || !strings.Contains(out, "created ") || !strings.Contains(out, "alice") {
		t.Fatalf("create: %d %q", code, out)
	}
	// The password is never echoed, in any form.
	if strings.Contains(out, "correct horse") {
		t.Fatalf("the CLI echoed the password: %q", out)
	}
	// It is a real account: the hash is bcrypt and it verifies against exactly
	// what was typed, newline stripped.
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(u.PasswordHash, "correct horse battery") {
		t.Fatal("the stored hash does not verify against the password on stdin")
	}
	if auth.CheckPassword(u.PasswordHash, "correct horse battery\n") {
		t.Error("the trailing newline was kept as part of the password")
	}
	// Taken names are refused, case-insensitively like registration.
	if code, out = fx.runIn(t, "another good one", "users", "create", "ALICE"); code != 1 || !strings.Contains(out, "is taken") {
		t.Errorf("duplicate: %d %q", code, out)
	}
	// And the new account shows up in the inventory the operator reads.
	if code, out = fx.run(t, "users"); code != 0 || !strings.Contains(out, "alice") {
		t.Errorf("users: %d %q", code, out)
	}
}

// `clippet admin users password` (§20 erratum 341): the recovery verb. The
// password comes from stdin; every browser of the account is signed out; an
// account `/signin` made with NO password gains one and can then log in.
func TestAdminUsersPassword(t *testing.T) {
	fx := newAdminFixture(t)
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	u, err := d.CreateDiscordUser(ctx, "nopw-id", "nopw", "100000000000000009", "nopw", 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"sess-a", "sess-b"} {
		if err := d.CreateSession(ctx, h, u.ID, 1000, 1<<40, 1<<40, "ua"); err != nil {
			t.Fatal(err)
		}
	}
	if auth.CheckPassword(u.PasswordHash, "") || auth.CheckPassword(u.PasswordHash, "anything at all") {
		t.Fatal("a passwordless account verifies a password")
	}
	if code, out := fx.runIn(t, "correct horse", "users", "password"); code != 2 || !strings.Contains(out, "usage") {
		t.Errorf("no name: %d %q", code, out)
	}
	if code, out := fx.runIn(t, "short", "users", "password", "nopw"); code != 2 || !strings.Contains(out, "8-72 bytes") {
		t.Errorf("short password: %d %q", code, out)
	}
	if code, out := fx.runIn(t, "correct horse battery", "users", "password", "nobody"); code != 1 || !strings.Contains(out, "no such user") {
		t.Errorf("unknown user: %d %q", code, out)
	}
	code, out := fx.runIn(t, "correct horse battery\n", "users", "password", "NOPW")
	if code != 0 || !strings.Contains(out, "password set for nopw-id  nopw; signed out of 2 browser(s)") {
		t.Fatalf("password: %d %q", code, out)
	}
	if strings.Contains(out, "correct horse") {
		t.Fatalf("the CLI echoed the password: %q", out)
	}
	back, err := d.GetUserByUsername(ctx, "nopw")
	if err != nil || !auth.CheckPassword(back.PasswordHash, "correct horse battery") {
		t.Fatalf("the new password does not verify: %v", err)
	}
	if back.DiscordID == nil || *back.DiscordID != "100000000000000009" {
		t.Fatal("setting a password touched the Discord link")
	}
	if rows, err := d.ListSessions(ctx, u.ID, 1001); err != nil || len(rows) != 0 {
		t.Fatalf("sessions after users password = %d, %v; want none", len(rows), err)
	}
}
