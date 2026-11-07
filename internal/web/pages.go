package web

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/transcode"
	"clippet/internal/urls"
)

// ---- index -------------------------------------------------------------

type indexPage struct {
	page
	Clips      []clipRow
	TTLOptions []ttlOption
	DefaultTTL string // the Delete after no-op option's whole text: the account's default, else the server's (run 8)
	// LinkStyle is the ACCOUNT's default link grammar (§20 erratum 250), which
	// is the option the upload form's two-way control starts on. Unlike the TTL
	// picker there is no third "server default" entry to select, because there
	// is no third state: migration 0015's column is NOT NULL and every account
	// has one of the two. The form therefore always sends a value, which is the
	// same answer silence would have produced.
	LinkStyle string
	// LinkExamples is what the control's hint says: one EXAMPLE of each grammar
	// rather than a sentence about them (§20 erratum 264).
	LinkExamples linkExamples
	// UploadChunk is UPLOAD_CHUNK_BYTES (§20 erratum 344): the upload form's
	// `data-upload-chunk`, the size past which app.js sends a file in chunks.
	UploadChunk int64
	NextURL     string
	// NewestURL is the way BACK from a later page (§20 erratum 266): the
	// un-cursored library under the filters in force, rendered only when the
	// request carried a `?cursor=`. It is deliberately NOT a reverse cursor —
	// walking back one page at a time needs a token of its own and is ROADMAP
	// #4's leftover — and it is the same URL a stale cursor bounces to, so
	// "take me back" and "that token died" land in the same place.
	NewestURL string
	Tag       string // the active ?tag= filter ("" = the whole library)
	// Q and Mine are the other two filters (§20 errata 114-115). Q is the
	// normalised search term, which is also what the search box is re-filled
	// with — the server normalises, so the box shows what the list did.
	Q    string
	Mine bool
	// Filtered says at least one filter is on, so the template can render the
	// "showing …" line and its clear-all link once rather than three times.
	// TagOnly is the one filtered case with copy of its own: "No clips tagged
	// x" is a true and useful empty state, and it stops being either as soon
	// as a second filter could be what excluded everything.
	Filtered bool
	TagOnly  bool
	// ClearURL drops every filter. It is "/" today and stays a field because
	// the library's own URL is the only thing that has to change if it ever
	// stops being the site root.
	ClearURL string
	// ScopeOwn is LIBRARY_SCOPE=own (§20 erratum 113): the library is the
	// caller's own clips whatever the filters say, so the page says so and
	// does not offer a "mine only" toggle that could not be turned off.
	ScopeOwn    bool
	Collections []collectionRow
	// Stats is the owner's OWN tally, rendered as one line above the list
	// (ROADMAP #23, §20 erratum 156). It counts the caller's clips and nobody
	// else's, it is never on a public page, and it carries no rank and no
	// comparison — it is the answer to "how much is in here", not a score.
	Stats libraryStats
	// Budget is the operator's advisory storage line (ROADMAP #24, §20
	// erratum 157). Over is false — and nothing is rendered — unless
	// STORAGE_BUDGET_BYTES is set, a measurement has been taken and the line
	// has been crossed. Nothing about uploading changes either way.
	Budget budgetNotice
}

// libraryStats is the owner's strip. Every value is pre-rendered so the
// template does no arithmetic and no formatting.
type libraryStats struct {
	Clips, Ready, Expiring7d int
	Views                    int64
	Stored                   string // humanBytes of source + everything derived
}

// budgetNotice is the storage-budget warning. By default it exists only to be
// read: no route consults it, no upload is refused because of it, and the
// sentence it renders says so, because an operator who sees a red line and
// cannot tell whether their friends' uploads have stopped is worse off than one
// who saw nothing at all.
//
// Hard is STORAGE_BUDGET_HARD=1 (§20 erratum 277), which makes the same line a
// CAP — and makes the sentence's job the opposite one: a person whose upload
// has just been refused needs the notice to say that this is the reason. The
// notice therefore says WHICH of the two this instance is, always; it is the
// one place a person who does not read `.env` can find out.
type budgetNotice struct {
	Over                 bool
	Hard                 bool
	Used, Budget, Excess string
}

// clipRow is one library entry. Views is rendered only when IsOwn (§8.2:
// private view counts are the ceiling).
type clipRow struct {
	ID, Title, State, StateLabel string
	PageURL, PosterURL           string
	ProgressURL                  string // set only while queued/processing
	Owner                        string
	Uploaded, Expiry             string
	Unlisted, IsOwn              bool
	Views                        int64
	Tags                         []tagChip
	// Duration and Resolution are the two facts that tell a four-second clip
	// from a four-minute one in a list of thumbnails (ROADMAP #30). Both are
	// measurements the row already carries and the share page already shows,
	// so neither is new information — and both are empty until the clip is
	// ready, which is when they exist.
	Duration   string
	Resolution string
	// StageNote says what is HAPPENING to a clip that is not ready yet, in as
	// few words as the fact allows: "queued", "encoding on the server",
	// "uploading to storage" (§20 erratum 224). It is the answer to the
	// question the striped bar could not answer — the owner's first look, 2026-09-08
	// — and it is SERVER-rendered from the transcode job's persisted stage, so a
	// reader with no JavaScript sees it too and a reader with JavaScript sees
	// the same words when the batched poll updates them. Empty for a ready or
	// failed row: there is nothing in progress to describe.
	StageNote string
}

// tagChip is one tag rendered as a link back into the library filter. It
// carries no count: a tag is a word, not a ranking (§17 #9 anti-features).
type tagChip struct{ Tag, URL string }

// collectionRow is one entry of the library's Collections section — the
// OWNER's own collections, the only place they are ever listed.
type collectionRow struct {
	ID, Title, PageURL string
	ClipCount          int
}

// tagChips renders a clip's tags as library-filter links.
func tagChips(tags []string) []tagChip {
	if len(tags) == 0 {
		return nil
	}
	out := make([]tagChip, 0, len(tags))
	for _, t := range tags {
		out = append(out, tagChip{Tag: t, URL: "/?tag=" + url.QueryEscape(t)})
	}
	return out
}

// frontPage is what a SESSION-LESS visitor gets at `/` (ROADMAP #36, §20
// erratum 246): the wordmark, one line, a way in. Open drives the "Create an
// account" link exactly as it does on the login page.
type frontPage struct {
	page
	Open bool
}

// front renders the front door. It is the answer `/` gives when there is no
// session — a 200, not the `303 /login?next=/` it was until erratum 246 — so
// a stranger who followed the wordmark from a share page lands on a page
// that says whose player that was, rather than on a login form. It carries
// the same headers the redirect it replaces carried (private, no-store,
// Vary: Cookie): WHICH page `/` renders depends on the cookie (§20 erratum
// 37), so a shared cache must never store this one and serve it to a session.
// No header slot (page.Wordmark stays false): the page IS the front door, and
// a wordmark linking to `/` on `/` would be noise. Indexable, and no card
// tags — it is not a clip.
func (s *server) front(w http.ResponseWriter, r *http.Request) {
	setPrivate(w)
	data := frontPage{
		page: s.basePage(r, productName),
		Open: s.registrationOpen(r),
	}
	s.render(w, r, "front", http.StatusOK, data)
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if !s.resolved(w, r) {
		return
	}
	u, ok := sessionUser(r)
	if !ok {
		s.front(w, r)
		return
	}
	ctx, cancel := s.ssrContext(r)
	defer cancel()
	// ?tag= is the same filter GET /api/clips?tag= applies, over the same
	// visibility set. A tag that cannot exist gets the same answer a cursor
	// that cannot exist gets — a bounce to the unfiltered library — because
	// this is a page, not an API: there is nothing useful to render for a word
	// no clip can carry, and inventing an error page for a mistyped chip would
	// be a worse answer than the library itself (§20 erratum 81).
	//
	// `?q=` and `?mine=1` are the two other filters (§20 errata 114-115) and
	// bounce exactly the same way: a search that cannot be a search and a
	// `mine` that is not `1` are both a mistyped URL, and the library is a
	// better answer to one than an error page.
	tag := ""
	if raw := r.URL.Query().Get("tag"); raw != "" {
		clean, err := db.CleanTag(raw)
		if err != nil {
			redirectPrivate(w, r, "/")
			return
		}
		tag = clean
	}
	q, err := db.CleanSearch(r.URL.Query().Get("q"))
	if err != nil {
		redirectPrivate(w, r, "/")
		return
	}
	mine := false
	if r.URL.Query().Has("mine") {
		if r.URL.Query().Get("mine") != "1" {
			redirectPrivate(w, r, "/")
			return
		}
		mine = true
	}
	scopeOwn := s.d.Cfg.LibraryScope == config.LibraryScopeOwn
	cursor := r.URL.Query().Get("cursor")
	clips, next, err := s.d.DB.ListClips(ctx, db.ListQuery{
		ViewerID: u.ID, Limit: libraryPageSize, Cursor: cursor,
		Tag: tag, Q: q, Mine: mine, ScopeOwn: scopeOwn,
	})
	if errors.Is(err, db.ErrInvalidCursor) {
		// A stale or foreign page token is not a reason to throw the FILTERS
		// away as well. The reader is looking at `?tag=x` and asked for the
		// next page OF THAT; the honest landing is its first page, not the
		// whole library. (The illegal-filter bounces above go to `/` because
		// there is no filter left to keep.)
		redirectPrivate(w, r, libraryURL(filterQuery("", tag, q, mine)))
		return
	}
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	rows, err := s.clipRows(ctx, clips, u)
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	cols, err := s.d.DB.ListCollections(ctx, u.ID)
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	// The caller's OWN tally (ROADMAP #23). One aggregate on a page that is
	// already making several, and it is scoped by owner_id in the SQL — there
	// is no spelling of this page that shows anybody else's numbers.
	stats, err := s.d.DB.AccountStatsOf(ctx, u.ID, s.d.Now().Unix())
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	// The account's default link grammar, for the upload form's control (§20
	// erratum 250). It is deliberately NOT a page failure: a default is a
	// convenience, and a library that 503s because one indexed lookup of a
	// two-word column failed would be a worse page than one whose radio starts
	// on `compact` — which is also what an upload with no field resolves to
	// when the same read fails there (httpapi's userDefaults).
	linkStyle := db.LinkStyleCompact
	defaultTTL := "server default · " + defaultTTLLabel(s.d.Cfg.DefaultTTLSeconds)
	if def, derr := s.d.DB.GetUserDefaults(ctx, u.ID); derr == nil {
		linkStyle = def.LinkStyleOr()
		// Delete after's no-op option names what an upload that sends no
		// lifetime GETS: the account's own default when it has a legal one,
		// the order applyDefaults resolves (run 8; the upload form's must 5,
		// which `server default · …` broke whenever the account had one).
		if secs, terr := config.ParseTTLDays(def.TTLDays, nil); def.TTLDays != nil && terr == nil {
			defaultTTL = "default · " + defaultTTLLabel(secs)
		}
	} else if !errors.Is(derr, db.ErrNotFound) {
		s.log.Warn("library: read account defaults; the upload form starts on the instance default",
			"user", u.ID, "err", derr)
	}
	data := indexPage{
		page:         s.basePage(r, productName),
		Clips:        rows,
		TTLOptions:   ttlOptions,
		DefaultTTL:   defaultTTL,
		LinkStyle:    linkStyle,
		LinkExamples: linkExamplesOf(s.d.URLs),
		UploadChunk:  s.d.Cfg.UploadChunkBytes,
		Tag:          tag,
		Q:            q,
		Mine:         mine,
		Filtered:     tag != "" || q != "" || mine,
		TagOnly:      tag != "" && q == "" && !mine,
		ClearURL:     libraryPath,
		ScopeOwn:     scopeOwn,
		Collections:  collectionRows(cols, s.d.URLs),
		Stats: libraryStats{
			Clips: stats.Clips, Ready: stats.Ready, Expiring7d: stats.Expiring7d,
			Views:  stats.ViewsTotal,
			Stored: humanBytes(stats.BytesSource + stats.BytesRenditions),
		},
		Budget: s.budgetNotice(),
	}
	if next != "" {
		data.NextURL = libraryURL(filterQuery(next, tag, q, mine))
	}
	// Somebody who followed "Older clips" has no way back without the browser's
	// own button, and a page two reached from a bookmark has none at all. The
	// answer is the first page of what they are looking at — the SAME URL the
	// stale-cursor bounce above mints — and it exists exactly while a cursor
	// was spent to get here.
	if cursor != "" {
		data.NewestURL = libraryURL(filterQuery("", tag, q, mine))
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "index", http.StatusOK, data)
}

// budgetNotice renders the storage-budget warning, or the zero value when
// there is nothing to say: no holder wired, no STORAGE_BUDGET_BYTES set, no
// measurement taken yet, or the instance is under its line (ROADMAP #24).
//
// It is shown to every LOGGED-IN viewer of the library rather than to an
// "operator" the app has no concept of: there is no admin role in this
// product (§14 has users and nothing else), the number is the same one
// `admin stats` and /healthz print, and on a household instance the person
// who would clear space is as likely to be the friend uploading as the person
// who owns the bucket. It is never on a public page — the library is
// session-only — and it names no clip and no account.
func (s *server) budgetNotice() budgetNotice {
	if s.d.Budget == nil {
		return budgetNotice{}
	}
	b := s.d.Budget.Get()
	if !b.Over {
		return budgetNotice{}
	}
	return budgetNotice{
		Over:   true,
		Hard:   s.d.Cfg.StorageBudgetHard,
		Used:   humanBytes(b.Used),
		Budget: humanBytes(b.Budget),
		Excess: humanBytes(b.Used - b.Budget),
	}
}

// libraryPath is the library's own URL — the page every filter link and every
// bounce is relative to.
const libraryPath = "/"

// libraryURL joins the library path to an already-encoded query ("" = none).
func libraryURL(query string) string {
	if query == "" {
		return libraryPath
	}
	return libraryPath + "?" + query
}

// filterQuery builds a library link carrying the filters in force (and, when
// non-empty, a page cursor). Every filter rides along because the cursor is
// minted UNDER them: dropping one would answer 400 invalid_cursor (§20
// errata 81, 115).
func filterQuery(cursor, tag, q string, mine bool) string {
	v := url.Values{}
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	if tag != "" {
		v.Set("tag", tag)
	}
	if q != "" {
		v.Set("q", q)
	}
	if mine {
		v.Set("mine", "1")
	}
	return v.Encode()
}

// collectionRows formats the owner's collections for the library section.
func collectionRows(cols []db.Collection, u urls.Builder) []collectionRow {
	out := make([]collectionRow, 0, len(cols))
	for _, c := range cols {
		out = append(out, collectionRow{
			ID: c.ID, Title: c.Title, PageURL: samePath(u.Collection(c.ID)), ClipCount: c.ClipCount,
		})
	}
	return out
}

// clipRows resolves owner names (memoised per request — a household has a
// handful of users) and formats each clip for the library list.
func (s *server) clipRows(ctx context.Context, clips []db.Clip, viewer db.User) ([]clipRow, error) {
	names := map[string]string{viewer.ID: viewer.Username}
	ids := make([]string, len(clips))
	for i, c := range clips {
		ids[i] = c.ID
	}
	// ONE query for the whole page's tags: the library renders up to 100 rows,
	// and a per-row lookup would be 100 round trips inside the SSR budget.
	tags, err := s.d.DB.TagsForClips(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	rows := make([]clipRow, 0, len(clips))
	// The ids of the rows that are still working, collected as the loop builds
	// them so the stage-note read below is one query rather than one per row.
	var unfinished []string
	for _, c := range clips {
		owner, ok := names[c.OwnerID]
		if !ok {
			ou, err := s.d.DB.GetUserByID(ctx, c.OwnerID)
			switch {
			case errors.Is(err, db.ErrNotFound):
				owner = "unknown"
			case err != nil:
				return nil, fmt.Errorf("owner %s: %w", c.OwnerID, err)
			default:
				owner = ou.Username
			}
			names[c.OwnerID] = owner
		}
		row := clipRow{
			ID: c.ID, Title: c.Title, State: c.State, StateLabel: stateLabel(c.State),
			PageURL:  samePath(s.d.URLs.Page(c.ID)),
			Owner:    owner,
			Uploaded: formatDate(c.CreatedAt),
			Expiry:   expiryText(c.ExpiresAt, s.d.Now()),
			Unlisted: c.Unlisted,
			IsOwn:    c.OwnerID == viewer.ID,
			Views:    c.Views,
			Tags:     tagChips(tags[c.ID]),
		}
		switch c.State {
		case db.ClipReady:
			row.PosterURL = samePath(s.d.URLs.Poster(c.ID))
			// The two badges of ROADMAP #30, from the measurements the row
			// already holds. They are rendered ONLY for a ready clip: a
			// queued row has no duration yet, and a "0:00" that later became
			// "4:12" would be a wrong fact rather than a missing one.
			if c.DurationMS != nil {
				row.Duration = formatDuration(*c.DurationMS)
			}
			row.Resolution = resolutionText(c)
		case db.ClipQueued, db.ClipProcessing:
			row.ProgressURL = samePath(s.d.URLs.Progress(c.ID))
			// A row that has not been claimed yet says so in the label itself,
			// which is what app.js's own first word for that state has always
			// been: two renderers of the same line must not disagree about it
			// (§20 erratum 224). Only the CLIP page and the embed keep the
			// undifferentiated "Processing…" for a queued clip — there the line
			// is one clip's whole status and the distinction earns nothing.
			if c.State == db.ClipQueued {
				row.StateLabel = statusQueued
			}
			unfinished = append(unfinished, c.ID)
		}
		rows = append(rows, row)
	}
	// ONE query for the whole page's stages, and only when something on the
	// page is still working (§20 erratum 224). It is the same read the batched
	// progress route makes — db.ProgressRows, the clip's own state plus its
	// latest TRANSCODE job's stage — so the phrase the page ships with and the
	// phrase the client's first poll writes over it are the same words from the
	// same source. A failure here is NOT a 503 page: the stage note is a
	// courtesy on a row that already says it is processing, and a library that
	// refused to render because one aggregate failed would be the worse answer.
	if len(unfinished) > 0 {
		stages, err := s.d.DB.ProgressRows(ctx, unfinished)
		if err != nil {
			s.log.Warn("library stage notes unavailable", "clips", len(unfinished), "err", err)
		} else {
			note := make(map[string]string, len(stages))
			for _, p := range stages {
				note[p.ClipID] = stageNote(p.State, p.Stage)
			}
			for i := range rows {
				if rows[i].ProgressURL != "" {
					rows[i].StageNote = note[rows[i].ID]
				}
			}
		}
	}
	return rows, nil
}

// stageNote is the library row's short phrase for what is happening to a clip
// right now (§20 erratum 224). One clause per stage of the §8.4 vocabulary, in
// the fewest words that are true — the owner asked for "queued", "encoding on the
// server", "uploading to storage", and this is that list finished.
//
// Only a clip that is actually being WORKED ON gets a phrase. A `queued` clip's
// label already reads "Queued…", and a phrase repeating it would be two words
// spent saying one thing; a claimed job that has not reported a stage yet is
// honestly still waiting, and the row says so; a ready or failed row has nothing
// in progress to describe at all.
//
// An unrecognised stage yields "" rather than the raw word: a stage name is a
// vocabulary this server owns, and printing one it does not recognise would be
// showing a reader an internal identifier.
//
// The percentage is deliberately NOT here. It is already on the bar, and app.js
// appends it to this same line for a reader who has JavaScript; the phrase's job
// is to say what the bar cannot.
//
// It is kept in step with app.js's `stageNotes` by
// TestLibraryStageNotesMatchTheClient, which reads the shipped script.
// stageNotes is the §8.4 stage vocabulary in a library row's own words. It is a
// TABLE rather than a switch so that a test can compare its keys with app.js's
// `stageNotes` object in BOTH directions: a phrase only this side knows drifts
// exactly as visibly as one only the client knows (the row would change its
// words a second after it loads).
var stageNotes = map[string]string{
	"acquire": "reading the upload",
	"probe":   "inspecting the video",
	"encode":  "encoding on the server",
	"copy":    "encoding on the server",
	// remux is the `-c copy` container rewrite of a playable upload (§20 erratum
	// 200): no frame is re-encoded, so "encoding" would be a lie about the one
	// path that is fast because it does not encode.
	"remux": "repacking the upload",
	// light is the card's own copy (§20 erratum 203) — the stage a clip whose
	// upload the browser CAN play spends its wait in.
	"light":    "encoding the card copy",
	"verify":   "checking the result",
	"poster":   "making the thumbnail",
	"store":    "uploading to storage",
	"finalize": "finishing up",
	"done":     "finishing up",
}

func stageNote(state, stage string) string {
	if state != db.ClipProcessing {
		return ""
	}
	return stageNotes[stage]
}

// resolutionText is the "1280×720" half of a library row's badges. It is
// videoFacts' first clause, alone: the share page joins the two with a
// separator inside one line, while a row puts each in its own place — the
// duration over the thumbnail, the size beside the date.
func resolutionText(c db.Clip) string {
	if c.IsAudio() {
		// Where a video shows its picture an audio clip says what it is (§20
		// erratum 328): it has no picture, and its length is beside this already.
		return db.ClipKindAudio
	}
	if c.Width == nil || c.Height == nil {
		return ""
	}
	return fmt.Sprintf("%d×%d", *c.Width, *c.Height)
}

// ---- login / register --------------------------------------------------

type authPage struct {
	page
	Next   string
	AltURL string
	Open   bool
	// Discord is whether /login carries the "Sign in with Discord" anchor
	// (ROADMAP #44, §20 erratum 302): the instance's own application is
	// configured. DiscordStart is the anchor's href, carrying `next`. Notice
	// is the one line a callback refusal lands with (`?discord=<reason>`, a
	// closed map — an unknown value renders nothing).
	Discord      bool
	DiscordStart string
	Notice       string
	// Host is the instance as a person names it (urls.Builder.Host), for
	// /register's closed notice: whoever runs THIS host is who to ask
	// (§20 erratum 339) — and, since erratum 341, for /login's closed line and
	// /register's no-JavaScript hint, which said "the operator" until then.
	Host string
	// SignIn is whether /login carries the line that sends a member of the
	// connected server to `/signin` (§20 erratum 341): the bot configured and
	// REGISTRATION=members|role:<id>.
	SignIn bool
}

// discordLoginStart is the start path of the Discord sign-in flow — spelled
// here because this package cannot import internal/httpapi (its tests import
// this one); httpapi's DiscordLoginStartPath is the same string, and a test in
// each package pins its spelling.
const discordLoginStart = "/api/auth/discord/start"

// loginNotices is the closed map of what /login says after a Discord
// callback refused. Terse, no jargon, and the one that matters says what to
// do instead (§20 erratum 302 (b): a Discord account is not an account here).
var loginNotices = map[string]string{
	"expired":  "That Discord sign-in expired. Try again.",
	"denied":   "Discord sign-in cancelled.",
	"failed":   "Discord did not answer. Try again.",
	"unlinked": "No account here is linked to that Discord. Log in with your password, then link it in Settings.",
	// The bot's one-time link, used, expired or unknown (§20 erratum 341). It
	// is the one key shown without the OAuth button: `/signin` is its door.
	signinNotice: "That sign-in link expired. Run /signin again.",
}

// settingsNotices is the same map for the Discord panel on /settings; the
// bool is whether the line is the green kind.
var settingsNotices = map[string]struct {
	Text string
	OK   bool
}{
	"linked":  {"Discord linked. It can sign in to this account from now on.", true},
	"taken":   {"That Discord account is already linked to another account here.", false},
	"denied":  {"Discord sign-in cancelled; nothing was linked.", false},
	"failed":  {"Discord did not answer. Try again.", false},
	"expired": {"That Discord link expired. Try again.", false},
}

// registrationOpen answers the pages' half of REGISTRATION (§20 erratum 106).
// `open`/`closed` are constants; `first` is open only while the users table is
// empty — one COUNT(*) on the smallest table in the schema, on two anonymous
// pages that are already rendering a template.
//
// That count is memoised for registrationCacheTTL (server.go). Both callers
// are ANONYMOUS pages, so an instance being crawled hard paid the query per
// request for an answer that changes once in an instance's life. The cache is
// this package's alone: internal/httpapi's twin is deliberately uncached,
// because /register closing INSTANTLY is the whole point of `first`.
//
// A database failure reads as CLOSED here rather than as a 503 page: the flag
// only decides whether a form and a link are drawn, /register answers the same
// question again for real before it creates anything, and drawing a signup
// form that cannot work is the worse of the two wrong answers. A failure is
// never cached, so the next request asks again.
func (s *server) registrationOpen(r *http.Request) bool {
	mode := s.d.Cfg.Registration
	if mode == "" {
		mode = config.RegistrationFirst
	}
	if !mode.NeedsFirstUser() {
		return mode.AlwaysOpen()
	}
	now := s.d.Now()
	if open, ok := s.reg.get(now); ok {
		return open
	}
	ctx, cancel := s.ssrContext(r)
	defer cancel()
	n, err := s.d.DB.CountUsers(ctx)
	if err != nil {
		return false
	}
	open := n == 0
	s.reg.put(now, open)
	return open
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if _, ok := sessionUser(r); ok {
		redirectPrivate(w, r, next)
		return
	}
	// Open drives the "Create an account" link, so it is read HERE too: with
	// registration closed the only thing that link can reach is /register's
	// own "registration is closed" notice, and sending a new household member
	// down that dead end is the app misinforming them about itself.
	data := authPage{
		page:   s.basePage(r, "Log in · "+productName),
		Next:   next,
		AltURL: "/register?" + url.Values{"next": {next}}.Encode(),
		Open:   s.registrationOpen(r),
		Host:   s.d.URLs.Host(),
		SignIn: s.signinDoor(),
	}
	reason := r.URL.Query().Get("discord")
	if s.d.Cfg.DiscordLogin.Configured() {
		// The button, and the notice a callback lands with — neither exists
		// with the feature off, because nothing could have sent anybody
		// here with a reason.
		data.Discord = true
		data.DiscordStart = discordLoginStart + "?" + url.Values{"next": {next}}.Encode()
		data.Notice = loginNotices[reason]
	}
	if data.SignIn && reason == signinNotice {
		// A dead one-time link (§20 erratum 341) — the same rule: only while
		// the door that mints them is open.
		data.Notice = loginNotices[reason]
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "login", http.StatusOK, data)
}

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if _, ok := sessionUser(r); ok {
		redirectPrivate(w, r, next)
		return
	}
	data := authPage{
		page:   s.basePage(r, "Register · "+productName),
		Next:   next,
		AltURL: "/login?" + url.Values{"next": {next}}.Encode(),
		Open:   s.registrationOpen(r),
		Host:   s.d.URLs.Host(),
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "register", http.StatusOK, data)
}

// ---- /settings (ROADMAP #1; §20 erratum 110) -----------------------------

// settingsPage is the one account page: change the password, see what is
// logged in, and mint or revoke API tokens. Tokens moved here rather than
// keeping a page of their own because all three are answers to the same
// question — "who can act as me right now" — and the incident that starts
// with "I pasted my token into the wrong channel" ends with revoking a
// session, or the other way round.
type settingsPage struct {
	page
	Username string
	Tokens   []tokenRow
	Sessions []sessionRow
	// SessionsError is set when the session list could not be read. The page
	// still renders: the password form and the token list are independent of
	// it, and a settings page that 503s because one of its four sections is
	// unavailable takes away the three that work.
	SessionsError string
	// Scopes are the mint form's checkboxes, in auth.Scopes order.
	Scopes []scopeChoice
	// InboundHost is the configured host the Kind hint's inbound example is
	// written on (§20 erratum 322), rendered as the Links examples are — and
	// the host the three CLI lines name: whoever runs it (the owner's call, 2026-11-02).
	InboundHost string
	// SetupWizard is SETUP_WIZARD (§20 erratum 335 (d)): off, the Storage
	// head carries no link to a /setup that would answer 404.
	SetupWizard bool
	// AutoFetch draws the Kind control's third option, Clippet Auto URL (§20
	// erratum 324), only where it can work: an instance that answers the
	// official bot (DISCORD_BOT_TOKEN=managed), the only way Clippet Auto
	// reaches a box. Not tiering: the official bot is free for every instance,
	// and the API and `admin tokens mint` mint the URL anywhere.
	AutoFetch bool
	// MaxSessionAge is the SESSION_MAX_TTL ceiling in words, so the "expires"
	// column has a reason beside it rather than looking like a countdown that
	// resets forever (§20 erratum 105).
	MaxSessionAge string
	// The account's upload defaults (ROADMAP #22, §20 erratum 155). The
	// section is the fourth answer to this page's one question — what happens
	// when I act as me — and erratum 155 said it slots into whichever layout
	// the account page ends up having; this is that layout.
	Defaults defaultsForm
	// Discord is the "Sign in with Discord" panel (ROADMAP #44, §20 erratum
	// 302): rendered only when the instance's own application is configured.
	Discord discordPanel
	// Storage is the instance-wide storage-cost line (ROADMAP #39, §20 erratum
	// 270): what is stored, what has been served this month, and what the
	// operator's own prices make of the two.
	Storage storagePanel
}

// ownerOnlyLine is the one sentence a signed-in account that is not the
// instance's owner reads on the owner's surfaces — `/setup` and the Storage
// panel of /settings (§20 erratum 349). The owner is the account that set
// this Clippet up (migration 0025); `admin users owner` moves it.
const ownerOnlyLine = "Only the account that set up this Clippet can see this."

// storagePanel is the "Storage" block of /settings — three rows in the row
// grammar and no chart (ROADMAP #39, §20 erratum 270).
//
// It is INSTANCE-WIDE, and since §20 erratum 349 it is the OWNER's: the
// account that set the instance up (migration 0025) reads its figures, and
// every other signed-in account gets the panel's head and one sentence
// (OwnerOnly) — the bill and the provider are the operator's business, not a
// member's. (Errata 270/277 shipped it to every account, reasoning that §14
// had users and nothing else.) There is deliberately no per-account figure
// here — that would be a tier by another name, and the account's own tally
// already lives on the library page.
//
// Every field is pre-rendered: the template does no arithmetic and no
// formatting, exactly as libraryStats above.
type storagePanel struct {
	// Show is false when no counter is wired — then the panel is not rendered
	// at all, because an empty one would say something untrue.
	Show bool
	// OwnerOnly is set, to ownerOnlyLine, for a signed-in account that is not
	// the instance's owner (§20 erratum 349): the panel renders its head and
	// that sentence, and NO figure — every other field is left empty, so
	// nothing about the instance's storage reaches the page.
	OwnerOnly string
	// Quota is the FIRST row when STORAGE_BUDGET_BYTES is set (§20 erratum
	// 277): `quota | 41.2 MiB of 190.7 MiB | 21 %`. It is the headline because
	// it is the only row that answers "is there room", which is the question
	// somebody opens this panel with; the money rows sit under it and are
	// about a different question. It REPLACES the `budget` row erratum 270
	// shipped — one row, not two, because two rows saying the same pair of
	// numbers is how a panel starts being skimmed.
	//
	// QuotaStored and QuotaLimit are the two byte figures in the IEC units the
	// rest of the panel prints, and QuotaAt the percentage. QuotaFull is the
	// instance being AT or past its line, which paints the state cell red.
	Quota                   bool
	QuotaStored, QuotaLimit string
	QuotaAt                 string
	QuotaFull               bool
	// Stored and Served are the two byte figures in words; StoredCost and
	// ServedCost the state cells beside them, which read `≈ 0.01 / month` when
	// the matching price is set and the muted word `unpriced` when it is not.
	// Never a 0.00 with no price behind it.
	Stored, StoredCost string
	Served, ServedCost string
	// ServedNote qualifies the served FIGURE (never the money beside it), and
	// is empty in every mode but one: with MEDIA_DELIVERY=redirect the bytes
	// leave the bucket without passing through this app, so the counter is
	// charged each redirect's whole object and the row reads `estimated
	// (redirect)` (§20 erratum 278). Erratum 270's law is that an unmeasured
	// number is ABSENT rather than a zero; this is its other half — a number
	// measured a different way says so where it is read.
	ServedNote string
	// Drop is the drop folder's one line, and it is there only when the folder
	// is ON (ROADMAP #42 (b), §20 erratum 296): `drop folder | dropped videos
	// become clips owned by <owner> | on`. It is on THIS panel because the
	// media directory is what the panel is about, and it is one line because
	// the only thing anybody signed in needs to know is whose library those
	// files land in — the folder's own state is the operator's and lives on
	// `admin doctor`.
	//
	// It names the OWNER and never the path. The directory is the operator's
	// disk layout, which /healthz has always refused to leak (§20 erratum 276)
	// and which this page, shown to every signed-in account, has no more
	// business printing.
	Drop      bool
	DropOwner string
	// Source names where the prices came from, after the panel's own hint:
	// `an estimate from R2 list prices as of 2026-09-13 (USD), never a bill`,
	// `local: no provider bill`, or the operator's-own-prices wording when no
	// preset is named (§20 erratum 277).
	Source string
}

// unpricedState, unmeasuredState and unavailableState are the three state words
// the cost cells fall back to, and they are THREE rather than two because
// "nothing has been measured yet" and "the measurement could not be read" are
// different facts and only one of them goes away on its own (§20 erratum 275).
// `unpriced` is the operator having typed no price; `not measured yet` is the
// seconds between boot and the sweeper's first reading; `unavailable` is a read
// that FAILED — which is what `admin stats` has always called the same thing
// (`served:  unavailable: …`), so the page and the CLI say it alike.
const (
	unpricedState    = "unpriced"
	unmeasuredState  = "not measured yet"
	unavailableState = "unavailable"
	noFigure         = "—"
)

// storageSourceHint is the second half of the panel's hint — where its money
// came from (§20 erratum 277).
//
// Three answers, and the third is the shipped one. A PRESET is somebody else's
// dated list and says so, because an estimate whose source is unstated is a
// number a reader has to trust blind; `local` says there is no provider bill at
// all, which is why its cost cells read `0.00` rather than `unpriced` (the owner's
// ask, 2026-09-13: print the zero); and with no preset the wording is the
// one erratum 270 shipped, because the prices ARE the operator's own.
func storageSourceHint(p cost.Prices) string {
	if src := p.Source(); src != "" {
		if p.Preset.Note != "" {
			return src // `local: no provider bill` — nothing is being estimated
		}
		return "an estimate from " + src + ", never a bill"
	}
	return "an estimate from your own prices, never a bill"
}

// servedRedirectNote is what the served row says when this instance does not
// see the bytes it is counting (§20 erratum 278). DEPLOY §31.3 uses the same
// two words, so the page and the page that explains it cannot describe the
// same number differently.
const servedRedirectNote = "estimated (redirect)"

// servedNote is the qualifier the served figure carries, or "" when the app
// wrote every byte it counted.
func servedNote(cfg config.Config) string {
	if cfg.RedirectsMedia() {
		return servedRedirectNote
	}
	return ""
}

// storageStatus builds the panel. It reads the sweeper's CACHED storage
// reading (never an aggregate of its own) and the served-bytes counter's
// totals, which are the same numbers /healthz answers with — one source, so the
// page and the probe cannot disagree.
//
// Nothing here can fail the page: a served read that errors leaves the row's
// figures as `—`, the way the session list already degrades to a sentence.
func (s *server) storageStatus(ctx context.Context) storagePanel {
	if s.d.Served == nil {
		return storagePanel{}
	}
	prices := cost.PricesOf(s.d.Cfg)
	p := storagePanel{Show: true,
		Stored: noFigure, StoredCost: unmeasuredState,
		Served: noFigure, ServedCost: unmeasuredState,
		ServedNote: servedNote(s.d.Cfg),
		Drop:       s.d.Cfg.DropFolderOwner != "",
		DropOwner:  s.d.Cfg.DropFolderOwner,
		Source:     storageSourceHint(prices)}
	var stored int64
	if s.d.Budget != nil {
		if b := s.d.Budget.Get(); b.Known {
			stored = b.Used
			p.Stored, p.StoredCost = humanBytes(b.Used), unpricedState
			if amount := prices.Stored(b.Used); amount != nil {
				p.StoredCost = "≈ " + amount.String() + " / month"
			}
			if b.Budget > 0 {
				p.Quota = true
				p.QuotaStored, p.QuotaLimit = humanBytes(b.Used), humanBytes(b.Budget)
				p.QuotaAt = strconv.FormatInt(b.Used*100/b.Budget, 10) + " %"
				// AT the line is full: a budget of N bytes is N bytes of
				// room. The red is the one exception erratum 270 allowed
				// itself on this panel and web/STYLE.md's law explains it —
				// red is LOSS, and a quota reached is the loss of the next
				// upload, whether the cap is armed (it is refused) or not
				// (the next one is what nobody planned for).
				p.QuotaFull = b.Used >= b.Budget
			}
		}
	}
	if _, month, err := s.d.Served.Totals(ctx, s.d.Now()); err == nil {
		p.Served, p.ServedCost = humanBytes(month), unpricedState
		if amount := prices.Served(month, stored); amount != nil {
			p.ServedCost = "≈ " + amount.String()
		}
	} else {
		// A read that FAILED is not a stack that has measured nothing: the
		// figure stays `—`, and the cell beside it says which of the two this
		// is. Still not an error page — the panel's other rows are independent
		// of this read, exactly as the session list degrades to a sentence.
		p.ServedCost = unavailableState
	}
	return p
}

// defaultsForm is the "Upload defaults" section's model. TTLOptions is the
// same closed set every other TTL picker offers, plus the "server default"
// entry that spells the instance's own current answer — an option labelled
// "server default" that does not say what that is makes the person choosing
// guess. The row reads "Delete after" (§20 erratum 244).
type defaultsForm struct {
	APIURL           string
	TTLOptions       []ttlOption
	SelectedTTL      string // "" = follow the instance; otherwise "0"|"1"|"7"|"30"|"90"
	InstanceTTLLabel string
	Unlisted         bool
	// LinkStyle is the account's default grammar, `compact` or `memorable`
	// (§20 erratum 250). Two states, no third: the column is NOT NULL.
	LinkStyle string
	// LinkExamples is the same hint the upload form carries — one example of
	// each grammar (§20 erratum 264). Settings keeps the word list's CREDIT
	// after it, which is an attribution and not copy about the choice.
	LinkExamples linkExamples
}

type tokenRow struct {
	ID, Name, Prefix  string
	Created, LastUsed string
	Scopes            string
	// Lead is the secret's own prefix as the Prefix cell shows it: `ci_` for an
	// inbound URL (§20 erratum 322), `ck_` for every other token.
	Lead string
}

// tokenLead is a row's display prefix, decided by its scopes: the inbound set
// is exactly ["inbound"] and the Clippet Auto set exactly ["autofetch"] (§20
// erratum 324; CleanScopes never stores either beside another scope).
func tokenLead(scopes []string) string {
	switch {
	case auth.IsInboundSet(scopes):
		return ids.InboundPrefix
	case auth.IsAutoFetchSet(scopes):
		return ids.AutoFetchPrefix
	}
	return ids.TokenPrefix
}

// sessionRow is one live session as the page renders it. There is no id and
// no per-row control: the DTO carries none either (§20 erratum 107), and the
// one action is the page-level "log out everywhere else".
type sessionRow struct {
	Started, LastSeen, Expires string
	Current                    bool
	UserAgent                  string
}

// scopeChoice is one checkbox of the mint form.
type scopeChoice struct {
	Value, Label, Hint string
	Checked            bool
}

// discordPanel is the Discord section of /settings: whether the account is
// linked, to whom and since when (display only), and the one line a callback
// landed with. The panel is the same question the sessions list answers —
// "who can act as me" — which is why it sits right after it.
type discordPanel struct {
	Configured bool
	Linked     bool
	Username   string
	Since      string
	Notice     string
	NoticeOK   bool
}

// scopeChoices describes the §20 erratum 111 scopes for the mint form. Upload
// is pre-ticked because it is what the overwhelming majority of tokens are
// for and what the shipped `.sxcu` needs.
func scopeChoices() []scopeChoice {
	return []scopeChoice{
		{Value: auth.ScopeUpload, Label: "Upload", Checked: true,
			Hint: "post new clips"},
		{Value: auth.ScopeRead, Label: "Read",
			Hint: "list the library and your collections, for a backup script"},
	}
}

// settings renders /settings. Session-only, like the library: a capture token
// must never reach the page that mints capture tokens.
func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	if !s.resolved(w, r) {
		return
	}
	u, ok := sessionUser(r)
	if !ok {
		redirectToLogin(w, r, "/settings")
		return
	}
	ctx, cancel := s.ssrContext(r)
	defer cancel()
	toks, err := s.d.DB.ListTokens(ctx, u.ID)
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	rows := make([]tokenRow, 0, len(toks))
	for _, t := range toks {
		row := tokenRow{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Created: formatDate(t.CreatedAt),
			LastUsed: "never", Scopes: strings.Join(t.Scopes, ", "), Lead: tokenLead(t.Scopes)}
		if t.LastUsedAt != nil {
			row.LastUsed = formatDate(*t.LastUsedAt)
		}
		rows = append(rows, row)
	}
	def, err := s.d.DB.GetUserDefaults(ctx, u.ID)
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	data := settingsPage{
		page:          s.basePage(r, "Settings · "+productName),
		Username:      u.Username,
		Tokens:        rows,
		Scopes:        scopeChoices(),
		InboundHost:   s.d.URLs.Host(),
		SetupWizard:   !s.d.Cfg.SetupWizardOff,
		AutoFetch:     s.d.Cfg.DiscordBot.Managed(),
		MaxSessionAge: humanDuration(auth.SessionMaxTTL(s.d.Cfg)),
		Defaults: defaultsForm{
			APIURL:           samePath(s.d.URLs.Abs("/api/auth/me/settings")),
			TTLOptions:       ttlOptions,
			SelectedTTL:      selectedDefaultTTL(def.TTLDays),
			InstanceTTLLabel: defaultTTLLabel(s.d.Cfg.DefaultTTLSeconds),
			Unlisted:         def.Unlisted,
			LinkStyle:        def.LinkStyleOr(),
			LinkExamples:     linkExamplesOf(s.d.URLs),
		},
	}
	// The Storage panel is the owner's (§20 erratum 349). A member gets its
	// head and one sentence, and none of its figures are even computed.
	owner, err := s.d.DB.IsOwner(ctx, u.ID)
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	if owner {
		data.Storage = s.storageStatus(ctx)
	} else if s.d.Served != nil {
		data.Storage = storagePanel{Show: true, OwnerOnly: ownerOnlyLine}
	}
	if s.d.Cfg.DiscordLogin.Configured() {
		// The row by id: the session's Principal does not carry the link
		// fields (db.User), and the panel must show the row as it is now.
		row, err := s.d.DB.GetUserByID(ctx, u.ID)
		if err != nil {
			s.backendFailure(w, r, err)
			return
		}
		data.Discord = discordPanel{Configured: true, Linked: row.DiscordID != nil, Username: row.DiscordUsername}
		if row.DiscordLinkedAt != nil {
			data.Discord.Since = formatDate(*row.DiscordLinkedAt)
		}
		if n, ok := settingsNotices[r.URL.Query().Get("discord")]; ok {
			data.Discord.Notice, data.Discord.NoticeOK = n.Text, n.OK
		}
	}
	sessions, serr := s.d.DB.ListSessions(ctx, u.ID, s.d.Now().Unix())
	if serr != nil {
		data.SessionsError = "the session list is temporarily unavailable"
	} else {
		p, _ := auth.FromContext(r.Context())
		data.Sessions = make([]sessionRow, 0, len(sessions))
		for _, sess := range sessions {
			data.Sessions = append(data.Sessions, sessionRow{
				Started:  formatDate(sess.CreatedAt),
				LastSeen: formatDate(sess.LastSeenAt),
				Expires:  formatDate(sess.ExpiresAt),
				Current:  sess.TokenHash == p.SessionHash,
				// The stored string is the browser's own; it is rendered by
				// html/template as text and reaches nobody but its owner.
				UserAgent: sess.UserAgent,
			})
		}
	}
	// The owner's username, token prefixes and session list are on this page:
	// private, and never stored by a shared cache.
	setPrivate(w)
	s.render(w, r, "settings", http.StatusOK, data)
}

// settingsTokens keeps the Phase 1 URL answering (§20 erratum 110). Tokens
// are a section of /settings now, and a 303 to its anchor is what a
// bookmarked or documented `/settings/tokens` should do — not a 404, and not
// a second copy of the page.
func (s *server) settingsTokens(w http.ResponseWriter, r *http.Request) {
	// A permanent redirect would be cached by the browser forever, and this
	// is a compatibility shim rather than a statement about the URL space.
	// The private headers are the same ones the page it stands in for
	// carries, because a session-less caller is bounced to /login from there.
	redirectPrivate(w, r, "/settings#tokens")
}

// humanDuration lives in format.go with the other page formatters.

// selectedDefaultTTL renders the account's stored preference as the <option>
// value that carries it. "" is the "server default" entry — the THIRD state
// migration 0011 keeps — and is what an account that has expressed no
// preference selects.
func selectedDefaultTTL(days *int) string {
	if days == nil {
		return ""
	}
	return strconv.Itoa(*days)
}

// ---- /setup (ROADMAP #45; §20 errata 297-299, 309-311) --------------------

// setupPage is the first-run wizard's model. Everything the template renders
// is pre-rendered here — the rule storagePanel already follows — so the
// template does no arithmetic, no comparison and no formatting.
type setupPage struct {
	page
	// OwnerOnly is set, to ownerOnlyLine, when a signed-in account that is not
	// the instance's owner opens the page (§20 erratum 349): the template then
	// renders the heading and this sentence and nothing else.
	OwnerOnly string
	// Panel one: what is true now. BackendID is the value the E2E and the
	// visual pass read off `data-setup-backend`, and it is the CONFIG's word
	// (`fs` or `s3`), not the page's three-way classification.
	BackendID, BackendWord, Where string
	// StoreState is the shared last-ping cache's rendering: `pending`, `ok` or
	// `error: <class>` in the closed §20 erratum 31 vocabulary, and never the
	// doctor's own sentence, which interpolates the bucket and the endpoint.
	StoreState string
	// Checked is when that reading was taken, or "" while it is pending.
	Checked string
	// Clips is the `clips` row of the Now panel (§20 erratum 311): `none`, or
	// the count — the fact the STOP names later is on the page as a fact first.
	Clips string
	// Signed says a session is what got in here. Anonymous access happens on
	// exactly one kind of instance — one nobody has registered on yet — where
	// there is no owner to be and `Check now` has nobody to run as.
	Signed bool

	// Panel two: the two classifications, and the selected exposure. Every
	// option is a radio card whose State is the verdict on it (§20 errata 309,
	// 311): its own fact, or `on now` on the diagonal. No pick is refused
	// (erratum 348 (b)).
	Options   []setupChoice
	Exposures []setupChoice
	Exposure  string
	WalksRead string
	// Guessed says the exposure was read off the request rather than chosen:
	// `via` was absent from the query, so the page guessed from CF-Ray or its
	// absence, and the Reached row's state cell says so.
	Guessed bool
	// PerTenGB is the one optional row: what 10 GB is in THIS instance's own
	// clips. "" below twenty clips, because under that it is a guess.
	PerTenGB string

	// Panel three onwards, when a walk was chosen.
	Walk *setupWalk
	// Unknown is a `?store=` this table has no row for: the chooser renders,
	// no panel does, and the status is still 200 — a mistyped query is not a
	// missing page.
	Unknown bool
	// StepsOnly is `?view=steps&store=<id>` with a store this table has
	// (§20 erratum 335): the template renders Walk's head, caveat, steps and
	// `labels read` and nothing else. Walk then carries no Stop, no Block and
	// no Hints.
	StepsOnly bool
}

// setupWalk is one option's panel, already decided. Every known pick walks
// since §20 erratum 348 (b) withdrew the Cloudflare refusal.
type setupWalk struct {
	ID, Head, Caveat string
	Steps            []setupStep
	// Stop is the state-gated STOP callout, the panel's first thing; nil when
	// it does not apply (§20 erratum 311).
	Stop *setupStop
	// Block is the total `.env` block and Hints the lines under it: the one
	// shared line on a bucket block, then the option's own closing line(s).
	Block    string
	Hints    []string
	Services string
}

// blockHints are the lines under a block: blockHint on a bucket block, nothing
// on a local one (§20 erratum 309, which collapsed erratum 298's seven).
func blockHints(o setupOption) []string {
	if o.Bucket {
		return []string{blockHint}
	}
	return nil
}

// exposureOf classifies how this instance is reached, and is the one place
// that decision is made (§20 erratum 299).
//
// The default is DETECTED, not asked: a request that came through Cloudflare —
// a tunnel or a proxied hostname alike — carries `CF-Ray`, which this reads
// and nothing else in the app does. It is read for its PRESENCE only and never
// for identity, so a forged one buys nothing but a refusal against the forger.
// Somebody behind something else that sets it, or setting a stack up before
// its tunnel exists, says so with the Reached control: the query wins over the
// header, always. The second value says whether the answer was GUESSED — no
// `via` in the query — which the page prints as one word in the Reached row's
// state cell (§20 erratum 309).
//
// A Clippet Pass name is never DETECTED, only chosen (§20 erratum 303). It
// carries `CF-Ray` like any other tunnel and there is nothing in a request
// that distinguishes one, and there must not be: the Pass is something
// somebody bought, and a page that guessed it would be printing a paid
// product's instructions at everybody behind a tunnel of their own.
func exposureOf(r *http.Request) (exposure string, guessed bool) {
	switch r.URL.Query().Get("via") {
	case exposureCloudflare:
		return exposureCloudflare, false
	case exposurePass:
		return exposurePass, false
	case exposureOther:
		return exposureOther, false
	}
	if r.Header.Get("CF-Ray") != "" {
		return exposureCloudflare, true
	}
	return exposureOther, true
}

// perTenGB is the one line that makes "10 GB free" mean something, out of two
// integers this page already loaded: what ten DECIMAL gigabytes — the unit
// both free tiers are quoted in — is in this instance's own clips.
//
// Decimal and not binary on purpose: the binary reading is about 7 % more
// flattering, and the flattering direction is the one a self-hosted product
// must not round in. Below twenty clips it is "": an average of four clips is
// a guess, and an absent row beats a wrong number.
func perTenGB(clips int, bytes int64) string {
	const decimalTenGB = 10_000_000_000
	if clips < 20 || bytes <= 0 {
		return ""
	}
	avg := bytes / int64(clips)
	if avg <= 0 {
		return ""
	}
	return "about " + strconv.FormatInt(decimalTenGB/avg, 10) +
		" clips, at this instance's own average (" + humanBytes(avg) + " a clip)"
}

// setup renders /setup (ROADMAP #45).
//
// Who may see it: a signed-in account, exactly as the Storage panel is
// instance-wide and shown to every one of them (§20 errata 270, 277) — there
// is no admin role in this product, and inventing one for a page that prints
// no credential would be a new concept for no gain. And, on an instance NOBODY
// has registered on yet, anybody: that is a first-run wizard's whole premise,
// there is no owner to be yet, and the page holds nothing private. The moment
// the first account exists that door closes and an anonymous caller goes to
// /login, the way /settings has always sent one.
//
// A database that cannot answer "are there any users" reads as CONFIGURED and
// sends the caller to log in: closed is the safe way for that question to
// fail.
//
// With SETUP_WIZARD=off — set by the desktop app, which does the setup on the
// instances it runs — every view but the steps-only one answers the ordinary
// 404, before and after the first account (§20 erratum 335 (d)). The steps
// view stays, behind the same gate as everywhere else: the app opens it in
// the browser during first setup.
func (s *server) setup(w http.ResponseWriter, r *http.Request) {
	if !s.resolved(w, r) {
		return
	}
	if s.d.Cfg.SetupWizardOff && !isStepsView(r) {
		s.notFound(w, r)
		return
	}
	ctx, cancel := s.ssrContext(r)
	defer cancel()
	u, signed := sessionUser(r)
	if !signed {
		n, err := s.d.DB.CountUsers(ctx)
		if err != nil || n != 0 {
			redirectToLogin(w, r, "/setup")
			return
		}
	} else {
		// The owner's page (§20 erratum 349): another signed-in account gets
		// the heading and one sentence, 403, and nothing the page would have
		// read — not even the steps-only view, which is the same door.
		owner, err := s.d.DB.IsOwner(ctx, u.ID)
		if err != nil {
			s.backendFailure(w, r, err)
			return
		}
		if !owner {
			setPrivate(w)
			s.render(w, r, "setup", http.StatusForbidden, setupPage{
				page:      s.basePage(r, "Where clips live · "+productName),
				OwnerOnly: ownerOnlyLine,
			})
			return
		}
	}
	// The steps-only view (§20 erratum 335): one option's numbered steps and
	// dated hints, for a caller that owns everything else on this page — the
	// desktop app's Storage screen. Same gate as the page; it reads no
	// instance fact, so nothing new is exposed. Without a valid store it is
	// the ordinary page.
	if isStepsView(r) {
		if o, ok := setupOptionByID(r.URL.Query().Get("store")); ok {
			setPrivate(w)
			s.render(w, r, "setup", http.StatusOK, setupPage{
				page:      s.basePage(r, "Where clips live · "+productName),
				WalksRead: walksRead,
				StepsOnly: true,
				Walk:      &setupWalk{ID: o.ID, Head: o.Head, Caveat: o.Caveat, Steps: o.Steps},
			})
			return
		}
	}
	cfg := s.d.Cfg
	current := currentStore(cfg)
	clips, bytes, cerr := s.d.DB.ClipCensus(ctx)
	if cerr != nil {
		// Nothing on this page needs the census to be right: without it the
		// STOP row does not render and the per-10-GB line is absent, which is
		// what an instance with no clips gets anyway. A wizard that 503s
		// because a count failed would take away the five walks it exists for.
		clips, bytes = 0, 0
	}
	data := setupPage{
		page:       s.basePage(r, "Where clips live · "+productName),
		BackendID:  cfg.StorageBackend,
		Where:      whereWord(cfg, current),
		Signed:     signed,
		StoreState: "pending",
		Exposures:  exposureChoices(),
		WalksRead:  walksRead,
		Clips:      "none",
		PerTenGB:   perTenGB(clips, bytes),
	}
	if clips > 0 {
		data.Clips = strconv.Itoa(clips)
	}
	data.Exposure, data.Guessed = exposureOf(r)
	if data.BackendID == "" {
		data.BackendID = config.BackendS3
	}
	data.BackendWord = backendWord(current)
	if s.d.Health != nil {
		data.StoreState = s.d.Health.State()
		if t := s.d.Health.LastChecked(); !t.IsZero() {
			data.Checked = formatDate(t.Unix())
		}
	}
	for i := range data.Exposures {
		data.Exposures[i].Selected = data.Exposures[i].Value == data.Exposure
	}
	want := r.URL.Query().Get("store")
	opts := setupOptions()
	data.Options = make([]setupChoice, 0, len(opts))
	for _, o := range opts {
		// Every card carries its verdict (§20 errata 309, 311): the option's
		// own fact, or `on now` for the one this instance is on.
		c := setupChoice{Value: o.ID, Label: o.Label, Hint: o.Hint, Selected: o.ID == want, State: o.Hint}
		if diagonal(current, o) {
			c.State = "on now"
		}
		data.Options = append(data.Options, c)
	}
	if want != "" {
		o, ok := setupOptionByID(want)
		switch {
		case !ok:
			data.Unknown = true
		default:
			data.Walk = buildSetupWalk(o, current, clips, data.Exposure)
		}
	}
	// Instance-wide facts and, when there is a session, the header nav: the
	// same privacy the page carrying the Storage panel has.
	setPrivate(w)
	s.render(w, r, "setup", http.StatusOK, data)
}

// isStepsView reports whether r asks for /setup's steps-only view of a known
// card (§20 erratum 335): `view=steps` with a valid `store`. Anything else is
// the ordinary page.
func isStepsView(r *http.Request) bool {
	q := r.URL.Query()
	if q.Get("view") != "steps" {
		return false
	}
	_, ok := setupOptionByID(q.Get("store"))
	return ok
}

// buildSetupWalk decides one panel for a pick — every known one walks (§20
// erratum 348 (b)). Everything it reads is
// the target, the derived current store, the clip count and the exposure —
// the whole of the state this page depends on, and it is two booleans and an
// integer rather than five times a matrix.
func buildSetupWalk(o setupOption, current string, clips int, exposure string) *setupWalk {
	w := &setupWalk{
		ID: o.ID, Head: o.Head, Caveat: o.Caveat,
		Steps: o.Steps, Services: o.Services,
		Stop: stopFor(current, o, clips),
	}
	block, notes := setupBlockFor(o, exposure)
	w.Block = block
	// The option's own closing line goes LAST, under the shared one: it is
	// about this block and the shared line is about every block. On a Clippet
	// Pass name there are two of them (§20 erratum 303) — the target's, then
	// the name's — in that order for the same reason.
	w.Hints = append(blockHints(o), notes...)
	return w
}

// ---- share page ----------------------------------------------------------

// clipPage is the share page model. Every URL the browser loads or the JS
// calls is a same-origin path; PageURL (what copy-link puts on the
// clipboard) is the absolute share URL.
type clipPage struct {
	page
	ID, Title, Description       string
	State, StateLabel            string
	Ready, Failed                bool
	FailureReason                string
	PageURL, APIURL, ProgressURL string
	// IsAudio puts an <audio> in the player region with the card picture above
	// it, and leaves out every control that is about a picture — trim, the GIF,
	// the chosen frame, the replacement (§20 erratum 328).
	IsAudio bool
	// AudioTierLabel is what an audio clip is playing, in the quality control's
	// words (`Full · FLAC 24-bit · 4.1 MiB`), shown when there is no choice.
	AudioTierLabel string
	// Peaks and WaveGeometry draw an audio clip's own progress over its
	// waveform (§20 erratum 329): the row's bars as transcode.EncodePeaks
	// wrote them, and where the server drew them — "frameW,frameH,bandX,bandY,
	// bandW,bandH,barW,barGap,minBarH" — so app.js has no geometry of its own.
	// Both empty when the row has no readable peaks.
	Peaks, WaveGeometry string
	// There is no LinkStyle here any more: it existed for erratum 251's one
	// owner-only line about a memorable unlisted id, which §20 erratum 264
	// withdrew. The fact itself is not gone — `ClipDTO.link_style` still carries
	// it, derived from the id — the PAGE simply says nothing about it.
	TouchURL   string // owner-only: the §8.1 keep-alive
	TrimURL    string // owner-only: the §8.1 trim route
	CanTrim    bool   // owner-only: ready AND long enough to cut
	TrimLive   bool   // owner-only: a trim is queued/running, so the page rejoins it
	GIFURL     string // owner-only: GET the preview (202 while it is made)
	GIFPostURL string // owner-only: POST to (re)generate it
	HasGIF     bool   // a preview exists right now: show it without asking
	// ReplaceURL is the owner's re-upload under this same id (ROADMAP #31,
	// §20 erratum 135): POST a file to it, GET it for the job's progress. It
	// is owner-only because the route is, and it is rendered only for a ready
	// clip, which is the only state that has something to replace.
	ReplaceURL string
	// ReplaceLive is `data-replace-live`: a replacement is running, so the
	// page rejoins it instead of asking (§20 erratum 240).
	ReplaceLive bool
	// The card-image control's world, all owner-only (ROADMAP #5, §20 errata
	// 127-130). PosterPostURL queues a frame, PosterStatusURL reports the
	// job, CanPoster is "this clip is ready and long enough to point at",
	// PosterChosen says a frame was CHOSEN rather than picked automatically
	// (with PosterSeconds the moment, on the ORIGINAL's timeline) and
	// PosterLive says a job is queued or running so a reload rejoins it
	// rather than offering a button whose only answer would be 409.
	PosterPostURL   string
	PosterStatusURL string
	CanPoster       bool
	PosterChosen    bool
	PosterSeconds   string
	PosterLive      bool
	PosterNote      string
	// PosterCutStartMS is where the current rendition begins on the ORIGINAL's
	// timeline — the number the client adds to the player's own clock to speak
	// the API's coordinate system. It is read here rather than borrowed from
	// the trim control's TrimStartMS, because a clip can be too short to trim
	// and still have a cut (and a card image to choose).
	PosterCutStartMS int64
	// VideoDurationMS is the CURRENT rendition's length, on the facts list
	// where every viewer sees it. It is a `data-` hook, not owner-only
	// information: the same number is already printed beside it, and the
	// attribute is what lets a trim re-render the line without a reload.
	VideoDurationMS int64
	// The trim control's world, all owner-only (§20 erratum 68). Times are on
	// the ORIGINAL: SourceDurationMS bounds the inputs, TrimStartMS/TrimEndMS
	// are the cut the clip has now and pre-fill them, and TrimNote says both
	// in one sentence. HasSourceBound is false for a clip whose original was
	// never measured — then the inputs carry no max, exactly as the API
	// carries no bound.
	HasSourceBound           bool
	SourceDurationMS         int64
	SourceDurationSec        string // the bound as the number inputs speak it (floored, never rounded up)
	TrimStartMS, TrimEndMS   int64
	TrimStartSec, TrimEndSec string
	TrimNote                 string
	// The scrubber's filmstrip (§20 erratum 220). FilmstripURL always answers —
	// it MAKES a sheet when there is none — so it is rendered wherever the trim
	// control is; the four geometry numbers are rendered only when a sheet
	// exists right now, which is what lets the strip appear with the page
	// instead of after a poll. They are the MEASURED tile layout, so the client
	// reads a moment's tile as floor(ms / interval_ms) and trusts it.
	FilmstripURL                   string
	HasFilmstrip                   bool
	FilmstripTiles                 int
	FilmstripTileW, FilmstripTileH int
	FilmstripIntervalMS            int64
	VideoURL, PosterURL, ViewURL   string
	// The share row, which belongs to EVERY viewer of a ready clip (§9.1,
	// §20 erratum 98). A download is not a call to action — it hands over the
	// clip the viewer came for — and an embed snippet is how a link ends up on
	// a forum or a wiki. Neither is a share TARGET, and none is ever added.
	EmbedCode   string // the exact <iframe> "Copy embed" puts on the clipboard
	DownloadURL string // the rendition as a save: video.mp4?download=1
	// OriginalURL and SourceSize are the OWNER's alone — /original is
	// owner-gated (§8.1) — and are rendered in every state the page renders at
	// all, so they also rescue the source of a clip that failed to encode.
	OriginalURL string
	SourceSize  string
	// The two dates, in the two forms a page needs (ROADMAP #8, §20 erratum
	// 133). Uploaded and Expiry are the absolute UTC sentences the server has
	// always rendered and remain what a reader with no JavaScript sees;
	// UploadedISO and ExpiryISO are the same instants as RFC3339, which is
	// what `<time datetime>` carries and what the client re-renders into the
	// viewer's own locale and a countdown. ExpiryPrefix/ExpiryDate/
	// ExpirySuffix are Expiry split at the timestamp, so the machine-readable
	// element wraps the DATE and the English stays outside it. ExpiryISO is
	// empty for a clip kept forever — there is no instant to name.
	UploadedISO       string
	ExpiryISO         string
	ExpiryPrefix      string
	ExpiryDate        string
	ExpirySuffix      string
	Uploaded, Expiry  string
	Expires           bool   // false = kept forever: no timer to reset
	VideoFacts        string // "1280×720 · 0:06"; empty → entry omitted
	Unlisted, IsOwner bool
	Views             int64
	TTLOptions        []ttlOption
	// TagsText is the OWNER's tag editor: the clip's tags as the
	// comma-separated line the field holds. It is populated only for the
	// owner — §9 says a watch page is the clip and nothing else, and a
	// visitor's page therefore renders no tag markup at all, not even a
	// read-only list.
	TagsText string
	// Collections is the OWNER's "file this clip" control (ROADMAP #17, §20
	// erratum 151): every collection they own, ticked where this clip is
	// already a member. Owner-only for TagsText's reason exactly — a
	// collection is listed in one place, the owner's own library (§9.4), and
	// a visitor's watch page says nothing about one.
	Collections []clipCollection
	// StoredSize is what this clip occupies in the bucket: the untouched
	// upload plus everything the transcode produced. OWNER-ONLY, beside
	// "Download original", which is where the source size already was
	// (ROADMAP #23, §20 erratum 156).
	StoredSize string
	// The quality selector (§20 errata 205, 211). Tiers is every tier that has a
	// file to play, best first, labelled with the numbers that were MEASURED;
	// HasTierChoice is "there is more than one, so render the control at all";
	// and TierNote is the one short line about a tier that has not arrived —
	// "Full quality still encoding" — or empty when there is nothing to say.
	//
	// It is rendered for EVERY viewer, not just the owner: choosing what to
	// stream is a viewer's decision, and it is the whole point of the feature.
	Tiers         []clipTier
	HasTierChoice bool
	TierNote      string
	// TiersLive is "a tier is still coming", which is what makes the row render
	// even with nothing in it yet: app.js needs a mount point for the option it
	// will add when that tier lands.
	TiersLive bool
	// CardNote is the ONE short line a human gets while the clip's Discord card
	// is not ready yet (§20 erratum 213). The owner asked two things of it on
	// 2026-09-08: say what is happening and where (what processing is and where
	// it happens), and say it shortly — the existing tooltips were too verbose
	// for what they are. So it names the machine and the consequence in one
	// sentence, with no countdown and no estimate (an estimate is too often a bit
	// of a lie). Empty for a card-ready clip and for a failed one, which has its
	// own line already.
	CardNote string
}

// clipTier is one entry of the quality selector: the value app.js persists, the
// URL the <video> switches to, and the label a person reads. Selected marks the
// tier the page starts on — the best playable one — because a <select> with
// nothing selected shows its first option whatever the player is doing.
type clipTier struct {
	Tier     string
	URL      string
	Label    string
	Selected bool
}

// clipCollection is one checkbox of the clip page's collection control.
// PostURL and DeleteURL are the two single-clip routes, pre-built so the
// client concatenates nothing.
type clipCollection struct {
	ID, Title string
	MemberURL string // /api/collections/{cid}/clips/{clip}: POST to add, DELETE to remove
	Selected  bool
	ClipCount int
}

func (s *server) clipModel(v ClipView) clipPage {
	c, u := v.Clip, v.URLs
	m := clipPage{
		ID: c.ID, Title: c.Title, Description: c.Description,
		State: c.State, StateLabel: stateLabel(c.State),
		Ready:      c.State == db.ClipReady,
		Failed:     c.State == db.ClipFailed,
		PageURL:    u.Page(c.ID),
		APIURL:     samePath(u.Abs("/api/clips/" + c.ID)),
		TouchURL:   samePath(u.Abs("/api/clips/" + c.ID + "/touch")),
		TrimURL:    samePath(u.Abs("/api/clips/" + c.ID + "/trim")),
		Uploaded:   formatDate(c.CreatedAt),
		Expiry:     expiryText(c.ExpiresAt, s.d.Now()),
		Expires:    c.ExpiresAt != nil,
		Unlisted:   c.Unlisted,
		IsOwner:    v.IsOwner,
		Views:      c.Views,
		TTLOptions: ttlOptions,
	}
	if v.IsOwner {
		m.TagsText = strings.Join(v.Tags, ", ")
		// The upload itself, in every state this page renders (a `deleting`
		// clip 404s before it gets here). "My recording is gone, give me my
		// original back" is the request this answers, and a clip whose
		// transcode failed is exactly when it is asked.
		// `?download=1` because `/original` is no longer only a download: for a
		// web-playable upload it is what the watch page STREAMS, so it is served
		// inline there and saves only when asked (§20 errata 98, 207). The
		// parameter is harmless on a clip whose upload stays owner-only — that
		// answer is an attachment either way.
		m.OriginalURL = samePath(u.Abs("/api/clips/" + c.ID + "/original?download=1"))
		m.SourceSize = humanBytes(c.SourceSize)
		m.StoredSize = humanBytes(storedBytes(c))
		m.Collections = clipCollections(v.Collections, v.InCollections, c.ID, u)
	}
	m.fillDates(c, s.d.Now())
	m.CardNote = cardNote(c)
	if c.FailureReason != nil {
		m.FailureReason = *c.FailureReason
	}
	if m.FailureReason == "" && m.Failed {
		m.FailureReason = "unknown error"
	}
	switch c.State {
	case db.ClipReady:
		m.VideoURL = samePath(u.Video(c.ID))
		m.PosterURL = samePath(u.Poster(c.ID))
		m.ViewURL = samePath(u.Abs("/api/clips/" + c.ID + "/view"))
		m.VideoFacts = videoFacts(c)
		m.DownloadURL = samePath(u.Video(c.ID) + "?download=1")
		m.EmbedCode = embedCode(u.Embed(c.ID), c.Width, c.Height)
		// The GIF is on-demand: the OWNER gets the control, and an anonymous
		// watch page gets no <img> at all (§17 #6). A preview is a few
		// megabytes nobody asked to download, and the watch page's job is the
		// player.
		m.GIFURL = samePath(u.GIF(c.ID))
		m.GIFPostURL = samePath(u.Abs("/api/clips/" + c.ID + "/gif"))
		m.HasGIF = c.GIFKey != nil
		// The re-upload (ROADMAP #31). Same shape as the two above: a
		// same-origin path the owner's browser posts to, rendered only where
		// the route would answer — the template gates it on IsOwner as well.
		m.ReplaceURL = samePath(u.Abs("/api/clips/" + c.ID + "/replace"))
		m.ReplaceLive = v.ReplaceLive
		if c.DurationMS != nil {
			m.VideoDurationMS = *c.DurationMS
		}
		m.fillTrim(c)
		m.TrimLive = m.CanTrim && v.TrimLive
		m.fillPoster(c, v)
		m.fillFilmstrip(c, u)
		// The quality selector, LAST of the ready-state fillers because it also
		// settles which file the <video> starts on (§20 erratum 211).
		m.fillTiers(c, u)
		if c.IsAudio() {
			// Sound (§20 erratum 328): the <audio> plays `/audio` (fillTiers set
			// it), the Download hands over that same file under its real
			// extension, and every picture control is off — their routes answer
			// not_for_audio anyway, and a button that can only fail is worse
			// than none. The embed snippet is sized by the card copy, which is
			// what the embed page plays.
			m.IsAudio = true
			if transcode.DecodePeaks(c.Peaks) != nil {
				m.Peaks, m.WaveGeometry = c.Peaks, waveGeometry
			}
			m.DownloadURL = samePath(u.Audio(c.ID) + "?download=1")
			m.CanTrim, m.TrimLive, m.CanPoster = false, false, false
			m.EmbedCode = embedCode(u.Embed(c.ID), c.SocialWidth, c.SocialHeight)
		}
		if tiersLive(c) {
			// The one case where a READY clip polls: a better tier is encoding, and
			// the page adds it to the selector when it lands.
			m.ProgressURL = samePath(u.Progress(c.ID))
		}
	case db.ClipQueued, db.ClipProcessing:
		m.ProgressURL = samePath(u.Progress(c.ID))
	}
	return m
}

// embedCode is the exact <iframe> "Copy embed" puts on the clipboard. It is
// built on the server so the page can be pinned to the literal string, and it
// is the SAME function the oEmbed document's `html` uses (§20 erratum 152):
// one snippet, whether a person copied it or a consumer asked for it.
func embedCode(embedURL string, width, height *int) string {
	return urls.EmbedCode(embedURL, width, height)
}

// oembedLink builds the `href` of the share page's oEmbed discovery link:
// /oembed?url=<this page>&format=json, with the page URL percent-encoded as a
// query value (it carries a scheme and slashes, and an unencoded one would end
// the parameter at the first `&` a future URL ever grows).
func oembedLink(u urls.Builder, clipID string) string {
	q := url.Values{"url": {u.Page(clipID)}, "format": {"json"}}
	return u.Abs("/oembed") + "?" + q.Encode()
}

// storedBytes is what one clip occupies: the untouched upload plus every
// derived object the row references. It is the per-clip half of the same
// arithmetic StorageUsed does for the instance, so the owner's page and the
// operator's budget line count the same bytes (§20 errata 156-157).
// The LIGHT copy is in the sum since the quality tiers, for the reason it is in
// StorageUsed (§20 erratum 209): it is a real object under the clip's own prefix,
// and an owner comparing "Stored" against what a remuxed original costs them was
// short by every card file they had.
func storedBytes(c db.Clip) int64 {
	n := c.SourceSize
	for _, p := range []*int64{c.RenditionSize, c.PosterSize, c.GIFSize, c.FilmstripSize, c.SocialSize} {
		if p != nil {
			n += *p
		}
	}
	return n
}

// clipCollections renders the owner's collections with this clip's membership
// already resolved, so the control needs no request to know its own state.
func clipCollections(cols []db.Collection, in map[string]bool, clipID string, u urls.Builder) []clipCollection {
	out := make([]clipCollection, 0, len(cols))
	for _, c := range cols {
		out = append(out, clipCollection{
			ID: c.ID, Title: c.Title, ClipCount: c.ClipCount, Selected: in[c.ID],
			MemberURL: samePath(u.Abs("/api/collections/" + c.ID + "/clips/" + clipID)),
		})
	}
	return out
}

// minTrimmableMS is the shortest clip the share page offers a trim for: the
// API's own floor (500 ms must survive the cut), so the control is never
// rendered where every window it could send is a 400.
const minTrimmableMS = 500

// fillTrim builds the owner's trim control from the ORIGINAL, which is the
// timeline every window is measured on (§20 erratum 68). The inputs are
// pre-filled with the cut the clip HAS — so "trim again" starts from the
// truth rather than from 0..whatever-is-left — and bounded by the original's
// length, so the footage an earlier cut removed is still reachable.
//
// The bound is FLOORED to a tenth of a second, never rounded: an input whose
// max the server would answer 400 for is worse than no input at all.
func (m *clipPage) fillTrim(c db.Clip) {
	length := int64(0)
	if c.SourceDurationMS != nil && *c.SourceDurationMS > 0 {
		length, m.HasSourceBound = *c.SourceDurationMS, true
	} else if c.DurationMS != nil {
		// The original was never measured (a row from before migration 0003
		// that has already been cut). The control still works — the API stops
		// bounding such a clip too — it simply offers no maximum.
		length = *c.DurationMS
	}
	if length <= minTrimmableMS {
		return
	}
	m.CanTrim = true
	m.SourceDurationMS = length
	m.SourceDurationSec = floorTenth(length)
	m.TrimStartMS, m.TrimEndMS = 0, length
	if c.TrimStartMS != nil && c.TrimEndMS != nil {
		m.TrimStartMS, m.TrimEndMS = *c.TrimStartMS, *c.TrimEndMS
	}
	m.TrimStartSec, m.TrimEndSec = floorTenth(m.TrimStartMS), floorTenth(m.TrimEndMS)
	cut := "the whole of it"
	if m.TrimStartMS != 0 || m.TrimEndMS != length {
		cut = formatDuration(m.TrimStartMS) + "–" + formatDuration(m.TrimEndMS)
	}
	if !m.HasSourceBound {
		m.TrimNote = "Seconds on the original upload, not on this cut."
		return
	}
	m.TrimNote = "Seconds on the original upload (0:00–" + formatDuration(length) + "); the cut is " + cut + "."
}

// fillFilmstrip builds the scrubber's filmstrip state (§20 erratum 220).
//
// The URL is unconditional for a ready clip: the route answers 200 when a sheet
// exists and 202-plus-enqueue when it does not, so a page that has the address
// needs nothing else to get one. The geometry is conditional, and that is the
// whole point of rendering it at all — a sheet that already exists is drawn
// before the first frame paints, with no request and no poll, which is the case
// every visit after the first.
//
// A row carrying some of the five and not the others cannot come from this
// server (one UPDATE writes them together), and rather than draw a strip from
// half a layout this reports no filmstrip, which sends the client to the route
// for a whole one.
func (m *clipPage) fillFilmstrip(c db.Clip, u urls.Builder) {
	m.FilmstripURL = samePath(u.Filmstrip(c.ID))
	if c.FilmstripKey == nil || c.FilmstripTiles == nil || c.FilmstripTileW == nil ||
		c.FilmstripTileH == nil || c.FilmstripIntervalMS == nil {
		return
	}
	if *c.FilmstripTiles <= 0 || *c.FilmstripTileW <= 0 || *c.FilmstripTileH <= 0 || *c.FilmstripIntervalMS <= 0 {
		return
	}
	m.HasFilmstrip = true
	m.FilmstripTiles, m.FilmstripTileW, m.FilmstripTileH = *c.FilmstripTiles, *c.FilmstripTileW, *c.FilmstripTileH
	m.FilmstripIntervalMS = *c.FilmstripIntervalMS
}

// minPosterableMS is the shortest clip the share page offers a card-image
// choice for: the API's own edge guard (posterEdgeGuardMS), so the control
// is never rendered where every moment it could send is a 400.
const minPosterableMS = 100

// fillPoster builds the owner's card-image control (ROADMAP #5, §20 errata
// 127-130). Like the trim's it speaks the ORIGINAL's timeline, because that
// is the timeline the choice is STORED on — which is what lets a later cut
// keep it — and the client turns the player's own clock into it by adding
// the cut's start.
//
// PosterSeconds is rendered only when a frame was actually chosen: an
// automatic poster is not a decision anybody made, and pre-filling a control
// with one would tell the owner they had picked something they had not.
func (m *clipPage) fillPoster(c db.Clip, v ClipView) {
	if c.DurationMS == nil || *c.DurationMS <= minPosterableMS {
		return
	}
	m.CanPoster = true
	m.PosterPostURL = samePath(v.URLs.Abs("/api/clips/" + c.ID + "/poster"))
	m.PosterStatusURL = m.PosterPostURL
	m.PosterLive = v.PosterLive
	if c.TrimStartMS != nil && *c.TrimStartMS > 0 {
		m.PosterCutStartMS = *c.TrimStartMS
	}
	if c.PosterSeconds == nil {
		m.PosterNote = "Card image: automatic."
		return
	}
	m.PosterChosen = true
	m.PosterSeconds = strconv.FormatFloat(*c.PosterSeconds, 'f', 3, 64)
	m.PosterNote = "Card image: the frame at " + formatDuration(int64(math.Round(*c.PosterSeconds*1000))) +
		" of the original."
}

// fillDates renders the two timestamps on the facts list in both forms the
// page needs (ROADMAP #8, §20 erratum 133): the absolute UTC sentence the
// server has always written, which is what a reader with no JavaScript sees,
// and the RFC3339 instant that `<time datetime>` carries so the client can
// re-render it in the viewer's own locale with a countdown under it.
//
// The expiry sentence is split at the timestamp rather than wrapped whole,
// so the machine-readable element holds a DATE and the English stays outside
// it. A clip kept forever has no instant to name and gets no <time> at all —
// which is also what stops the client from putting a countdown on it.
func (m *clipPage) fillDates(c db.Clip, now time.Time) {
	m.UploadedISO = rfc3339(c.CreatedAt)
	if c.ExpiresAt == nil {
		return
	}
	m.ExpiryISO = rfc3339(*c.ExpiresAt)
	m.ExpiryDate = formatDate(*c.ExpiresAt)
	if *c.ExpiresAt <= now.Unix() {
		m.ExpiryPrefix, m.ExpirySuffix = "expired ", " (pending deletion)"
		return
	}
	m.ExpiryPrefix = "expires "
}

// rfc3339 renders unix seconds the way httpapi's DTOs do, which is what a
// `datetime` attribute must carry: an unambiguous instant in UTC.
func rfc3339(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.RFC3339) }

// floorTenth renders milliseconds as seconds with one decimal, FLOORED. The
// number inputs use it for their bounds and their values, so neither can ever
// offer a moment past the end of the original.
func floorTenth(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatFloat(math.Floor(float64(ms)/100)/10, 'f', 1, 64)
}

// videoFacts is the "1280×720 · 0:06" line of a ready clip, built only from
// the measurements the row actually has (a ready row always has all of them;
// the template omits the entry when this is empty).
func videoFacts(c db.Clip) string {
	var parts []string
	if res := resolutionText(c); res != "" && !c.IsAudio() {
		parts = append(parts, res)
	}
	if c.DurationMS != nil {
		parts = append(parts, formatDuration(*c.DurationMS))
	}
	return strings.Join(parts, " · ")
}

func (s *server) clip(w http.ResponseWriter, r *http.Request) {
	if !s.resolved(w, r) {
		return
	}
	c, ok := s.lookupClip(w, r)
	if !ok {
		return
	}
	// A link-preview crawler asking about a clip whose card is still coming gets
	// 503 + Retry-After and NO card tags, so it cannot remember a link-out card
	// for a clip that is about to have a player one (§20 erratum 213). A human
	// falls through to the processing page, which is unchanged.
	if holdCrawler(r, c) {
		s.crawlerHold(w, r, c)
		return
	}
	// The owner's view count and the session nav render into this HTML: a
	// shared cache must never store or replay it by URL (§15).
	setPrivate(w)
	v := s.clipView(r, c)
	// Tags are fetched only for the owner: they render nowhere else on this
	// page, so an anonymous view costs no query at all.
	if v.IsOwner {
		ctx, cancel := s.ssrContext(r)
		defer cancel()
		tags, err := s.d.DB.TagsOf(ctx, c.ID)
		if err != nil {
			s.backendFailure(w, r, err)
			return
		}
		v.Tags = tags
		// Is there a trim to REJOIN? The page answers that itself rather than
		// letting the owner's browser find out with a GET …/trim that is a
		// 404 for every clip nobody has trimmed (§20 erratum 87). One indexed
		// query on a page that is already making them, in place of an HTTP
		// round trip and a 404 access-log line per view.
		job, jerr := s.d.DB.LatestJobOfKind(ctx, c.ID, db.KindTrim)
		switch {
		case errors.Is(jerr, db.ErrNotFound):
		case jerr != nil:
			s.backendFailure(w, r, jerr)
			return
		default:
			v.TrimLive = job.State == db.JobQueued || job.State == db.JobRunning
		}
		// …and the same question about the card image, for the same reason
		// and at the same cost: one indexed query on a page already making
		// them, instead of a GET …/poster per owner view that is a 404 for
		// every clip nobody has chosen a frame for.
		pjob, perr := s.d.DB.LatestJobOfKind(ctx, c.ID, db.KindPoster)
		switch {
		case errors.Is(perr, db.ErrNotFound):
		case perr != nil:
			s.backendFailure(w, r, perr)
			return
		default:
			v.PosterLive = pjob.State == db.JobQueued || pjob.State == db.JobRunning
		}
		// …and the replacement, for the same reason again (§20 erratum 240):
		// the client used to ask GET …/replace on every owner view, and "never
		// replaced" is a 404 — one console error per page load.
		rjob, rerr := s.d.DB.LatestJobOfKind(ctx, c.ID, db.KindReplace)
		switch {
		case errors.Is(rerr, db.ErrNotFound):
		case rerr != nil:
			s.backendFailure(w, r, rerr)
			return
		default:
			v.ReplaceLive = rjob.State == db.JobQueued || rjob.State == db.JobRunning
		}
		// …and the owner's collections, with this clip's membership already
		// resolved (ROADMAP #17). Two indexed queries on a page already making
		// four, in place of the GET /api/collections + read-modify-write PUT a
		// client would otherwise need before it could tick a box.
		cols, cerr := s.d.DB.ListCollections(ctx, c.OwnerID)
		if cerr != nil {
			s.backendFailure(w, r, cerr)
			return
		}
		in, ierr := s.d.DB.CollectionsOfClip(ctx, c.ID, c.OwnerID)
		if ierr != nil {
			s.backendFailure(w, r, ierr)
			return
		}
		v.Collections, v.InCollections = cols, in
	}
	data := s.clipModel(v)
	data.page = s.basePage(r, c.Title+" · "+productName)
	data.BodyClass = "watch"
	// A stranger's page carries the wordmark alone, linking home (§20 erratum
	// 244). It is the ONE brand a watch page may show; the card inventory, the
	// embed and the no-CTA rule are untouched.
	data.Wordmark = true
	data.Meta = BuildMeta(v)
	// The card's bar (§20 erratum 287). This is the ONE page that carries a
	// clip's card -- the ready player card and the link-out card are two shapes
	// of it -- so it is the one page that says what colour Discord paints down
	// the left edge of the unfurl. It is deliberately NOT in data.Meta: base.html
	// renders it after the theme-color pair and the inventory renders above
	// them, so a tag in the inventory would be FIRST and break both consumers
	// (the template's own comment has the measurement). §9.1's count does not move.
	data.CardColor = CardColor(c.State)
	// The page's own Discord card (§20 erratum 316), beside the §9.1 tags and
	// never instead of them: they are what Discord falls back to.
	if s.d.Cfg.CardComponents {
		data.ComponentEmbed = BuildComponentEmbed(v, s.d.Cfg.InstanceBadge)
	}
	data.Canonical = s.d.URLs.Page(c.ID)
	// The oEmbed discovery link (ROADMAP #18). It is ABSOLUTE, like the card
	// tags and the canonical link beside it: a consumer resolves it from
	// wherever it fetched the page, and PUBLIC_BASE_URL is the origin this
	// instance says it is at. Both URLs in it — the link's own and the `url`
	// parameter — are built here, never assembled by the consumer.
	data.OEmbed = oembedLink(s.d.URLs, c.ID)
	// "Unlisted" means do not surface this, and a search result is exactly the
	// surfacing it rules out. The card tags are UNTOUCHED — the unfurlers are
	// allowed by /robots.txt and read og:* whatever this says — so an unlisted
	// clip still pastes into Discord as the card it always did (§9.1 renders
	// unlisted and listed identically, and still does).
	data.NoIndex = c.Unlisted
	s.render(w, r, "clip", http.StatusOK, data)
}

// ---- collection page -----------------------------------------------------

// collectionPage is the /c/{id} model (§9.4, §17 #9). It is deliberately
// spare: a title, a list of links, and — for the owner alone — the controls
// that change them. There is no view count anywhere on it, no owner name, no
// call to action and no "more from this user": a collection is a list of
// links somebody chose to share, and the page is that list.
type collectionPage struct {
	page
	ID, Title string
	PageURL   string // absolute: what the copy-link button puts on the clipboard
	APIURL    string // same-origin: /api/collections/{id}
	ClipsURL  string // same-origin: /api/collections/{id}/clips
	Clips     []collectionEntry
	IsOwner   bool
	// Choices is the owner's membership picker: every clip they own, the
	// current members FIRST in their current order (the client PUTs the
	// ticked boxes in DOCUMENT order, so this is what makes the picker
	// preserve the order the collection already has), then the rest, newest
	// first.
	Choices []collectionChoice
}

// collectionEntry is one clip in the list: a thumbnail or a state badge, a
// title, and a link to the clip's own share page. Nothing else — no duration
// beside it and certainly no counter.
type collectionEntry struct {
	ID, Title, State, StateLabel string
	PageURL, PosterURL           string
	// MemberURL is the single-clip membership route (ROADMAP #17, §20 erratum
	// 151): DELETE removes this row from this collection. It is filled for the
	// OWNER only, which is what keeps the remove control off a visitor's copy
	// of the page rather than a CSS rule doing it.
	MemberURL string
}

// collectionChoice is one checkbox of the membership picker. It is a checkbox
// and not an <option> because an unmodified click on a multi-select REPLACES
// the selection, which silently emptied collections (§20 erratum 95).
type collectionChoice struct {
	ID, Label string
	Selected  bool
	// PosterURL is the clip's card image, so the picker is a list of PICTURES
	// with titles beside them rather than a list of titles (§20 erratum 225).
	// The owner's first look: selecting clips for a collection by title alone
	// was hard. Empty while the clip is not ready — there is no poster yet, and
	// Label already carries the state in that case.
	PosterURL string
}

func (s *server) collection(w http.ResponseWriter, r *http.Request) {
	if !s.resolved(w, r) {
		return
	}
	id := r.PathValue("id")
	if !ids.Valid(id) {
		s.notFound(w, r)
		return
	}
	ctx, cancel := s.ssrContext(r)
	defer cancel()
	col, err := s.d.DB.GetCollection(ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		s.notFound(w, r)
		return
	case err != nil:
		s.backendFailure(w, r, err)
		return
	}
	clips, err := s.d.DB.CollectionClips(ctx, id)
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	v := CollectionView{Collection: col, Clips: clips, URLs: s.d.URLs}
	if u, ok := sessionUser(r); ok {
		v.Viewer = &u
		v.IsOwner = u.ID != "" && u.ID == col.OwnerID
	}
	data, err := s.collectionModel(ctx, v)
	if err != nil {
		s.backendFailure(w, r, err)
		return
	}
	// The owner controls and the session nav render into this HTML, exactly as
	// they do on the share page: private, and never stored by a shared cache.
	setPrivate(w)
	data.page = s.basePage(r, col.Title)
	// The link-out page a stranger lands on carries the wordmark alone, like
	// the share page (§20 erratum 244).
	data.Wordmark = true
	data.Meta = BuildCollectionMeta(v)
	data.Canonical = s.d.URLs.Collection(col.ID)
	// ALWAYS noindex: §9.4 lists a collection in exactly one place, the
	// owner's own library, so there is no "listed" collection for a search
	// engine to have been meant to find. The link-out card is unaffected.
	data.NoIndex = true
	s.render(w, r, "collection", http.StatusOK, data)
}

func (s *server) collectionModel(ctx context.Context, v CollectionView) (collectionPage, error) {
	col, u := v.Collection, v.URLs
	m := collectionPage{
		ID: col.ID, Title: col.Title,
		PageURL:  u.Collection(col.ID),
		APIURL:   samePath(u.Abs("/api/collections/" + col.ID)),
		ClipsURL: samePath(u.Abs("/api/collections/" + col.ID + "/clips")),
		IsOwner:  v.IsOwner,
		Clips:    make([]collectionEntry, 0, len(v.Clips)),
	}
	member := make(map[string]bool, len(v.Clips))
	for _, c := range v.Clips {
		member[c.ID] = true
		e := collectionEntry{
			ID: c.ID, Title: c.Title, State: c.State, StateLabel: stateLabel(c.State),
			PageURL: samePath(u.Page(c.ID)),
		}
		if c.State == db.ClipReady {
			e.PosterURL = samePath(u.Poster(c.ID))
		}
		if v.IsOwner {
			e.MemberURL = samePath(u.Abs("/api/collections/" + col.ID + "/clips/" + c.ID))
		}
		m.Clips = append(m.Clips, e)
	}
	if !v.IsOwner {
		return m, nil
	}
	// The picker offers the owner's OWN clips and only those: the API refuses
	// anything else with 400 not_your_clip, so offering a stranger's clip
	// would be offering an error.
	own, err := s.d.DB.ListOwnerClips(ctx, col.OwnerID, db.MaxClipsPerCollection)
	if err != nil {
		return collectionPage{}, fmt.Errorf("owner clips: %w", err)
	}
	labels := make(map[string]string, len(own))
	posters := make(map[string]string, len(own))
	for _, c := range own {
		labels[c.ID] = choiceLabel(c)
		posters[c.ID] = choicePoster(c, u)
	}
	m.Choices = make([]collectionChoice, 0, len(own)+len(v.Clips))
	for _, c := range v.Clips { // current members first, in position order
		label, ok := labels[c.ID]
		if !ok {
			label, posters[c.ID] = choiceLabel(c), choicePoster(c, u)
		}
		m.Choices = append(m.Choices, collectionChoice{ID: c.ID, Label: label,
			PosterURL: posters[c.ID], Selected: true})
	}
	for _, c := range own {
		if !member[c.ID] {
			m.Choices = append(m.Choices, collectionChoice{ID: c.ID, Label: labels[c.ID], PosterURL: posters[c.ID]})
		}
	}
	return m, nil
}

// choicePoster is the thumbnail beside a picker entry (§20 erratum 225), and it
// is empty for a clip that is not ready — `poster.jpg` answers 409 for one, and
// a broken image would be a worse answer than the state word choiceLabel
// already appends.
func choicePoster(c db.Clip, u urls.Builder) string {
	if c.State != db.ClipReady || c.PosterKey == nil {
		return ""
	}
	return samePath(u.Poster(c.ID))
}

// choiceLabel names a clip in the picker: its title, with the state appended
// while it is not ready, so an owner adding a clip that is still encoding can
// see that is what it is.
func choiceLabel(c db.Clip) string {
	if c.State == db.ClipReady {
		return c.Title
	}
	return c.Title + " (" + c.State + ")"
}

// ---- embed ---------------------------------------------------------------

// embedOptions are the ?autoplay/&muted/&loop flags of the embed URL (§9.2,
// §17 #8). Phase 1 parsed and ignored them so the URL shape was stable from
// the start; they now map to the <video> attributes of the same names.
//
// Autoplay implies Muted, and that is not a courtesy: every current browser
// refuses to start an unmuted video without a user gesture, so `?autoplay=1`
// alone would produce a player that silently does nothing — the one outcome
// that looks like a bug from every side. Muting is what makes autoplay
// actually autoplay.
type embedOptions struct{ Autoplay, Muted, Loop bool }

// resolve applies that implication. It is a method rather than a line inside
// the parser so the parser keeps saying exactly what the URL said (which is
// what TestParseEmbedOptions pins) and the template gets what the browser
// needs.
func (o embedOptions) resolve() embedOptions {
	if o.Autoplay {
		o.Muted = true
	}
	return o
}

// Preload is the <video> preload attribute for these options: a player that
// is about to start playing by itself needs the media, not just its header.
func (o embedOptions) Preload() string {
	if o.Autoplay {
		return "auto"
	}
	return "metadata"
}

func parseEmbedOptions(q url.Values) embedOptions {
	flag := func(k string) bool {
		if !q.Has(k) {
			return false
		}
		switch q.Get(k) {
		case "", "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return embedOptions{Autoplay: flag("autoplay"), Muted: flag("muted"), Loop: flag("loop")}
}

type embedPage struct {
	Title, CSS, JS string
	Theme          string
	// Badge is INSTANCE_BADGE (§20 erratum 285), rendered as `data-badge` on
	// <html> and NOWHERE else on this page: the overlay is 16 px of live text on
	// somebody else's page, over a picture, and the grey span the attribute
	// produces is its whole signal. No tag beside it, ever — web/STYLE.md's
	// "no text beside it" is older than this feature and outranks it.
	Badge                        string
	ID, State, StateLabel        string
	Ready                        bool
	ProgressURL                  string
	VideoURL, PosterURL, ViewURL string
	// PageURL is where the wordmark overlay points (§20 errata 246, 248): the
	// clip's share page, same-origin — an embed is framed on somebody else's
	// page, and a relative href resolves against THIS document, which is ours.
	// It comes from urls.Builder like every other address, so when the share
	// URL moved to the root the overlay moved with it instead of keeping a
	// hand-written `/clips/` that would have cost every click a redirect.
	PageURL string
	// Mark is what the wordmark overlay READS (§20 erratum 287): this
	// instance's own public host, in the lockup form on clippet.gg and any
	// subdomain of it and as plain text anywhere else. internal/web/mark.go has
	// the whole rule and the reason.
	Mark    hostMark
	Options embedOptions
}

func (s *server) embedHandler(w http.ResponseWriter, r *http.Request) {
	c, ok := s.lookupClip(w, r)
	if !ok {
		return
	}
	m := s.clipModel(ClipView{Clip: c, URLs: s.d.URLs})
	data := embedPage{
		Title: docTitle(s.d.Cfg.InstanceBadge, c.Title),
		CSS:   samePath(s.d.URLs.Static("app.css")),
		JS:    samePath(s.d.URLs.Static("app.js")),
		Theme: themeOf(r),
		Badge: s.d.Cfg.InstanceBadge,
		ID:    c.ID, State: c.State, StateLabel: m.StateLabel,
		Ready:       m.Ready,
		ProgressURL: m.ProgressURL,
		VideoURL:    embedVideoURL(c, m, s.d.URLs), PosterURL: m.PosterURL, ViewURL: m.ViewURL,
		PageURL: samePath(s.d.URLs.Page(c.ID)),
		Mark:    markOf(s.d.URLs.SiteName()),
		Options: parseEmbedOptions(r.URL.Query()).resolve(),
	}
	s.renderEmbed(w, r, data)
}

// embedVideoURL is what the embed's <video> plays: the share page's own file
// for a video, and for an audio clip its AUDIOGRAM (§20 erratum 329) — the card
// copy at /video.mp4, the waveform filling amber under the sound — rather than
// the /audio the share page's <audio> plays. The embed is the player a card
// opens (X's twitter:player), and so it keeps the embed's one shape: the same
// <video> with the waveform poster, the overlay, its face at ready, its hiding
// while playing (erratum 274).
func embedVideoURL(c db.Clip, m clipPage, u urls.Builder) string {
	if c.IsAudio() && m.Ready {
		return samePath(u.Video(c.ID))
	}
	return m.VideoURL
}

// waveGeometry is the waveform's geometry as the page's data-wave carries it
// (§20 erratum 329), from transcode's one definition.
var waveGeometry = strings.Join([]string{
	strconv.Itoa(transcode.WaveFrameW), strconv.Itoa(transcode.WaveFrameH),
	strconv.Itoa(transcode.WaveBandX), strconv.Itoa(transcode.WaveBandY),
	strconv.Itoa(transcode.WaveBandW), strconv.Itoa(transcode.WaveBandH),
	strconv.Itoa(transcode.WaveBarW), strconv.Itoa(transcode.WaveBarGap), strconv.Itoa(transcode.WaveMinBarH),
}, ",")
