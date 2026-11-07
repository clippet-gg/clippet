package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"clippet/internal/auth"
	"clippet/internal/urls"
	"clippet/internal/web"
)

// TestPageLinkAndAPICursorAreTheSameFilter is the cross-package half of N044's
// pin, and the only test anywhere that crosses this seam.
//
// The library page mints "Older clips" with the keys `cursor`/`tag`/`q`/`mine`
// (internal/web's filterQuery); GET /api/clips reads a query with exactly those
// four names and hands them to the same db.ListClips. Nothing made the two
// agree. ListClips REFUSES a cursor whose filter does not equal the query's
// (§20 errata 81, 115), so a rename or a dropped filter on either side turns
// "Older clips" under a filter into a 303 bounce on the page and a 400
// invalid_cursor on the API — and no test noticed either.
//
// Both directions are exercised: a cursor the API minted must be a page the
// LIBRARY renders, and the link the library renders must be a query the API
// answers. internal/httpapi may import internal/web in a TEST — nothing under
// internal/web imports this package, so there is no cycle, and only
// cmd/clippet mounts both handlers for real.
func TestPageLinkAndAPICursorAreTheSameFilter(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("carol")

	// One more clip than the library renders per page, every one of them the
	// owner's, tagged, and findable by the same needle — so each filter below
	// matches enough rows for BOTH pagers to produce a link: the API's, which
	// this test sets with `limit`, and the LIBRARY's, whose page size is an
	// unexported variable internal/httpapi cannot reach. Without that second
	// link the page→API direction below would never run, so the seed is sized
	// against internal/web's default rather than left to chance; if that
	// default ever grows, the "rendered no next-page link" failure below says
	// exactly that rather than passing silently.
	const libraryPageSize = 100
	now := e.now()
	for i := 0; i <= libraryPageSize; i++ {
		c := e.seedClip(owner, clipOpts{title: "gaming " + strconv.Itoa(i), createdAt: now - int64(i)})
		if err := e.db.SetClipTags(context.Background(), c.ID, []string{"gaming"}); err != nil {
			t.Fatal(err)
		}
	}

	base, err := url.Parse(testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	// A second handler over the SAME database: the pages, with a real session
	// resolver so the harness's cookie signs in there too.
	pages, err := web.New(web.Deps{
		DB:   e.db,
		Auth: &auth.Resolver{DB: e.db, Cfg: e.cfg, Now: time.Now},
		URLs: urls.New(base, "test"),
		Cfg:  e.cfg,
		Log:  slog.New(slog.DiscardHandler),
		Now:  time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	renderPage := func(t *testing.T, path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		pages.ServeHTTP(rec, req)
		return rec
	}

	for _, filter := range []string{"?tag=gaming", "?q=gaming", "?mine=1", "?tag=gaming&q=gaming&mine=1"} {
		t.Run(strings.TrimPrefix(filter, "?"), func(t *testing.T) {
			// The API pages at `limit`, which a test can set; the library pages
			// at libraryPageSize, which it cannot reach from here. So the
			// cursor is minted by the API and spent on BOTH.
			apiFirst := e.do(http.MethodGet, "/api/clips"+filter+"&limit=1", nil, withCookie(cookie))
			if apiFirst.code != http.StatusOK {
				t.Fatalf("GET /api/clips%s = %d: %s", filter, apiFirst.code, apiFirst.body)
			}
			cursor := apiFirst.str("next_cursor")
			if cursor == "" {
				t.Fatalf("no next_cursor for %s: %s", filter, apiFirst.body)
			}

			// (1) The API's cursor, carried on the PAGE's key set, is a page
			// the library renders. A 303 here is the library having dropped a
			// filter on the way in, which is what makes ListClips refuse it.
			pageURL := "/" + filter + "&cursor=" + url.QueryEscape(cursor)
			if rec := renderPage(t, pageURL); rec.Code != http.StatusOK {
				t.Fatalf("the library refused a cursor minted under the same filters: GET %s = %d (303 = a filter was dropped)",
					pageURL, rec.Code)
			}

			// (2) …and a link the LIBRARY rendered is a query the API answers.
			// Only the query crosses over — the path is the page's — and that
			// query is exactly what the two encoders have to agree about.
			href := nextLinkOf(renderPage(t, "/"+filter).Body.String())
			if href == "" {
				t.Fatalf("the library rendered no next-page link for %s, so the page→API direction could not be checked", filter)
			}
			q := ""
			if i := strings.Index(href, "?"); i >= 0 {
				q = href[i+1:]
			}
			apiSecond := e.do(http.MethodGet, "/api/clips?"+q, nil, withCookie(cookie))
			if apiSecond.code != http.StatusOK {
				t.Fatalf("GET /api/clips?%s = %d: the API rejected a link the library rendered: %s",
					q, apiSecond.code, apiSecond.body)
			}
		})
	}
}

// nextLinkOf pulls the "older clips" href out of a rendered library page and
// un-escapes it into something a request can be made with.
func nextLinkOf(body string) string {
	i := strings.Index(body, `class="more"`)
	if i < 0 {
		return ""
	}
	rest := body[i:]
	j := strings.Index(rest, `href="`)
	if j < 0 {
		return ""
	}
	rest = rest[j+len(`href="`):]
	k := strings.Index(rest, `"`)
	if k < 0 {
		return ""
	}
	return strings.ReplaceAll(rest[:k], "&amp;", "&")
}

// The same seam stated as a fact rather than inferred from a rendered page: the
// four filter names the library's links carry are four names GET /api/clips
// honours, together, in one request.
func TestListClipsHonoursEveryFilterThePageLinksCarry(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("carol")
	c := e.seedClip(owner, clipOpts{title: "gaming one"})
	if err := e.db.SetClipTags(context.Background(), c.ID, []string{"gaming"}); err != nil {
		t.Fatal(err)
	}
	e.seedClip(owner, clipOpts{title: "something else"})

	r := e.do(http.MethodGet, "/api/clips?tag=gaming&q=gaming&mine=1&limit=10", nil, withCookie(cookie))
	if r.code != http.StatusOK {
		t.Fatalf("every page-link filter at once = %d: %s", r.code, r.body)
	}
	if got := listIDs(t, r); len(got) != 1 || got[0] != c.ID {
		t.Fatalf("listed %v, want [%s] — one of the four filters was ignored", got, c.ID)
	}
}
