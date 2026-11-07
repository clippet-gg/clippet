package db

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// The bot's one-time login links (§20 erratum 341, migration 0023).

func signinRow(hash string, now int64) SigninToken {
	return SigninToken{Hash: hash, GuildID: "222222222222222222", DiscordID: "100000000000000001",
		Username: "mallory", DisplayName: "Mallory", CreatedAt: now, ExpiresAt: now + SigninTTLSeconds}
}

// A link is single use and lives five minutes: a peek changes nothing, the
// first consume wins, the second finds nothing, and an expired link is gone.
func TestSigninTokenSingleUseAndExpiry(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	const now = 1_000_000
	if err := d.CreateSigninToken(ctx, signinRow("h1", now)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // the crawler's GETs, and the person's
		if got, err := d.PeekSigninToken(ctx, "h1", now+10); err != nil || got.DiscordID != "100000000000000001" {
			t.Fatalf("peek %d = %+v, %v", i, got, err)
		}
	}
	got, err := d.ConsumeSigninToken(ctx, "h1", now+20)
	if err != nil || got.Username != "mallory" || got.DisplayName != "Mallory" || got.GuildID != "222222222222222222" {
		t.Fatalf("consume = %+v, %v", got, err)
	}
	if _, err := d.ConsumeSigninToken(ctx, "h1", now+21); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second consume = %v, want ErrNotFound", err)
	}
	if _, err := d.PeekSigninToken(ctx, "h1", now+21); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a peek after the consume = %v, want ErrNotFound", err)
	}
	// Expiry: alive at 299 s, dead at 300 s — and the dead row is removed.
	if err := d.CreateSigninToken(ctx, signinRow("h2", now)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PeekSigninToken(ctx, "h2", now+SigninTTLSeconds-1); err != nil {
		t.Fatalf("a 299 s old link must still read: %v", err)
	}
	if _, err := d.PeekSigninToken(ctx, "h2", now+SigninTTLSeconds); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a 300 s old link reads: %v", err)
	}
	if _, err := d.ConsumeSigninToken(ctx, "h2", now+SigninTTLSeconds); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired consume = %v, want ErrNotFound", err)
	}
	var n int
	if err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM signin_tokens").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left = %d (%v), want 0: an expired consume keeps its DELETE", n, err)
	}
	// Minting tidies: an expired row goes when the next one is made.
	if err := d.CreateSigninToken(ctx, signinRow("old", now)); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateSigninToken(ctx, signinRow("new", now+SigninTTLSeconds)); err != nil {
		t.Fatal(err)
	}
	if err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM signin_tokens WHERE token_hash = 'old'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the expired row survived the next mint: %d %v", n, err)
	}
}

// Of many concurrent consumes of one link, exactly one wins.
func TestSigninTokenConsumeRace(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	if err := d.CreateSigninToken(ctx, signinRow("race", 5000)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.ConsumeSigninToken(ctx, "race", 5001); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d consumes won, want exactly 1", won)
	}
}

// A new Discord account: the name as given, `_2`, `_3` on a collision (case-
// insensitive, like every username), linked by id in the same row, no password;
// a second creation for the same id is a conflict, never a second account.
func TestCreateDiscordUser(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	if _, err := d.CreateUser(ctx, "u0", "Mallory", "$2a$12$notahash", 1); err != nil {
		t.Fatal(err)
	}
	u, err := d.CreateDiscordUser(ctx, "u1", "mallory", "100000000000000001", "mallory", 10)
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "mallory_2" || u.PasswordHash != "" || u.DiscordID == nil || *u.DiscordID != "100000000000000001" {
		t.Fatalf("created %+v, want mallory_2 linked with no password", u)
	}
	back, err := d.GetUserByDiscordID(ctx, "100000000000000001")
	if err != nil || back.ID != "u1" || back.PasswordHash != "" || back.DiscordUsername != "mallory" || back.DiscordLinkedAt == nil {
		t.Fatalf("read back %+v, %v", back, err)
	}
	// The name that already existed is untouched and still unlinked: a
	// username match is never a link.
	if pre, _ := d.GetUserByID(ctx, "u0"); pre.DiscordID != nil {
		t.Fatal("the account that only shared the NAME was linked")
	}
	if _, err := d.CreateDiscordUser(ctx, "u2", "mallory", "100000000000000001", "mallory", 11); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second account for one Discord id = %v, want ErrConflict", err)
	}
	u3, err := d.CreateDiscordUser(ctx, "u3", "mallory", "100000000000000003", "mallory", 12)
	if err != nil || u3.Username != "mallory_3" {
		t.Fatalf("the third mallory = %+v, %v", u3, err)
	}
	// The suffix always fits inside 32 bytes.
	long := strings.Repeat("a", 32)
	for i := 0; i < 2; i++ {
		if _, err := d.CreateDiscordUser(ctx, "L"+string(rune('a'+i)), long, "20000000000000000"+string(rune('1'+i)), long, 13); err != nil {
			t.Fatal(err)
		}
	}
	l2, err := d.GetUserByDiscordID(ctx, "200000000000000002")
	if err != nil || l2.Username != strings.Repeat("a", 30)+"_2" {
		t.Fatalf("the long name's second = %+v, %v", l2, err)
	}
	if got := signinNameCandidate("ab", 12); got != "ab_12" {
		t.Errorf("candidate = %q", got)
	}
}
