package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
)

// regFixture is an EMPTY database (no users) driving a server in
// REGISTRATION=first, with the clock under the test's control.
type regFixture struct {
	d   *db.DB
	s   *server
	now int64
}

func newRegFixture(t *testing.T) *regFixture {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := &regFixture{d: d, now: 1_700_000_000}
	s, err := newServer(Deps{
		DB: d, Auth: passAuth{}, URLs: builder(t, testBase),
		Cfg: config.Config{SSRTimeout: 5 * time.Second, Registration: config.RegistrationFirst,
			SessionCookie: "clippet_session"},
		Now: func() time.Time { return time.Unix(f.now, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	return f
}

func (f *regFixture) open(t *testing.T) bool {
	t.Helper()
	return f.s.registrationOpen(httptest.NewRequest(http.MethodGet, "/login", nil))
}

func (f *regFixture) addUser(t *testing.T, name string) {
	t.Helper()
	if _, err := f.d.CreateUser(context.Background(), ids.New(), name, "$2a$12$hash", f.now); err != nil {
		t.Fatal(err)
	}
}

// The page-side flag is memoised for a second and NOT latched: it goes stale
// for at most registrationCacheTTL in BOTH directions, so `admin users prune`
// re-opens a `first` instance without a restart (N014).
func TestRegistrationOpenIsCachedButNeverLatched(t *testing.T) {
	f := newRegFixture(t)
	if !f.open(t) {
		t.Fatal("an empty users table under REGISTRATION=first must read OPEN")
	}
	// The first user lands. Within the TTL the page still reads open -- the
	// bounded staleness this cache buys -- and the ROUTE is unaffected: it
	// counts again for real before it creates anything.
	f.addUser(t, "carol")
	if !f.open(t) {
		t.Fatal("within the TTL the cached answer must still be used")
	}
	f.now++ // one second on: the entry is exactly TTL old, so it is stale
	if f.open(t) {
		t.Fatal("past the TTL the page must see the user that registered")
	}
	// `admin users prune` empties the table again. A latch-closed cache would
	// need a restart here; this one re-opens on the next miss.
	if _, err := f.d.PruneUsers(context.Background(), "%", f.now+1); err != nil {
		t.Fatal(err)
	}
	f.now++
	if !f.open(t) {
		t.Fatal("an emptied users table must re-open `first` without a restart")
	}
}

// A clock that steps BACKWARDS (the app container's does) must not freeze the
// cache: a negative age is a stale entry, not a fresh one.
func TestRegistrationCacheRejectsABackwardsClock(t *testing.T) {
	var c regCache
	base := time.Unix(1_700_000_000, 0)
	c.put(base, true)
	if _, ok := c.get(base.Add(-time.Second)); ok {
		t.Fatal("an entry stamped in the future was treated as fresh")
	}
	if got, ok := c.get(base); !ok || !got {
		t.Fatal("the entry must still be fresh at its own instant")
	}
	if _, ok := c.get(base.Add(registrationCacheTTL)); ok {
		t.Fatal("the entry must be stale at exactly the TTL")
	}
}

// The other two modes never touch the database at all, cache or no cache.
func TestRegistrationOpenSkipsTheCountForTheConstantModes(t *testing.T) {
	f := newRegFixture(t)
	for mode, want := range map[config.RegistrationMode]bool{
		config.RegistrationOpen:   true,
		config.RegistrationClosed: false,
	} {
		f.s.d.Cfg.Registration = mode
		if got := f.open(t); got != want {
			t.Errorf("REGISTRATION=%s reads %v, want %v", mode, got, want)
		}
	}
}
