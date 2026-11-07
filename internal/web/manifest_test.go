package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestWebManifest is ROADMAP #27's static half (§20 erratum 137): the document
// that makes the app installable, and that declares the share target the
// phone's share sheet then offers.
//
// It is asserted as PARSED JSON, not as a substring: a manifest with a typo in
// `share_target` is not a manifest a browser will act on, and the whole point
// of the feature is what a browser does with this file.
func TestWebManifest(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	rec := get(h, "/manifest.webmanifest")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/manifest+json; charset=utf-8" {
		t.Errorf("Content-Type %q, want the registered manifest type", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != staticCacheControl {
		t.Errorf("Cache-Control %q, want %q", cc, staticCacheControl)
	}
	var m struct {
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		StartURL  string `json:"start_url"`
		Scope     string `json:"scope"`
		Display   string `json:"display"`
		Icons     []struct {
			Src, Sizes, Type string
		} `json:"icons"`
		ShareTarget struct {
			Action, Method, Enctype string
			Params                  struct {
				Title, Text, URL string
				Files            []struct {
					Name   string
					Accept []string
				}
			}
		} `json:"share_target"`
		ServiceWorker any `json:"service_worker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("the manifest is not JSON: %v\n%s", err, rec.Body.String())
	}
	if m.Name != "Clippet" || m.ShortName != "Clippet" || m.StartURL != "/" || m.Scope != "/" || m.Display != "standalone" {
		t.Fatalf("manifest identity = %+v", m)
	}
	// Android wants a 192 and a 512 before it will install; both must be
	// files this server actually serves.
	sizes := map[string]bool{}
	for _, ic := range m.Icons {
		sizes[ic.Sizes] = true
		if ic.Type != "image/png" {
			t.Errorf("icon %q type = %q", ic.Src, ic.Type)
		}
		if !strings.HasPrefix(ic.Src, "/static/") {
			t.Errorf("icon src %q is not same-origin and root-relative", ic.Src)
		}
		if r := get(h, ic.Src); r.Code != 200 {
			t.Errorf("GET %s = %d; the manifest names an icon this server does not serve", ic.Src, r.Code)
		}
	}
	if !sizes["192x192"] || !sizes["512x512"] {
		t.Errorf("icon sizes = %v, want 192x192 and 512x512", sizes)
	}
	// The share target itself: a POST of multipart/form-data — the only shape
	// that can carry a file — to the session-guarded route, with the file part
	// named `file`, the same name clippet.sxcu uses.
	st := m.ShareTarget
	if st.Action != "/api/share" || st.Method != "POST" || st.Enctype != "multipart/form-data" {
		t.Fatalf("share_target = %+v", st)
	}
	if st.Params.Title != "title" || len(st.Params.Files) != 1 || st.Params.Files[0].Name != "file" {
		t.Fatalf("share_target params = %+v", st.Params)
	}
	// video/* and, since §20 erratum 328, audio/*: a phone's recorder is where
	// most sound comes from.
	if acc := st.Params.Files[0].Accept; len(acc) != 2 || acc[0] != "video/*" || acc[1] != "audio/*" {
		t.Errorf("share_target accepts %v, want [video/* audio/*]", acc)
	}
	// No service worker is declared, and none is shipped: nothing here caches
	// and nothing here is offline (§20 erratum 137).
	if m.ServiceWorker != nil {
		t.Errorf("the manifest declares a service worker: %v", m.ServiceWorker)
	}
	// It carries the page headers like every other route, HEAD works, and
	// anything else is a 405.
	assertNonEmbedHeaders(t, rec)
	headRec := do(h, http.MethodHead, "/manifest.webmanifest")
	if headRec.Code != 200 || headRec.Body.Len() != 0 || headRec.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD: %d body=%d len=%q", headRec.Code, headRec.Body.Len(), headRec.Header().Get("Content-Length"))
	}
	for _, meth := range []string{http.MethodPost, http.MethodDelete} {
		if r := do(h, meth, "/manifest.webmanifest"); r.Code != 405 || r.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d Allow=%q", meth, r.Code, r.Header().Get("Allow"))
		}
	}
}

// TestPagesLinkTheManifest: a manifest nothing links to is a manifest no
// browser reads, so the <link> is on every page base.html renders.
func TestPagesLinkTheManifest(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	// (A signed-in visitor is redirected away from /login, so the pages here
	// are the ones that render for this principal.)
	for _, path := range []string{"/", "/" + f.ready.ID, "/settings"} {
		rec := get(h, path)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `<link rel="manifest" href="/manifest.webmanifest">`) {
			t.Errorf("%s does not link the manifest", path)
		}
	}
	// The EMBED page is the exception this asserts rather than assumes: it is
	// a chrome-less player in somebody else's iframe, and an install prompt
	// belongs to the site, not to the embed.
	if rec := get(h, "/embed/"+f.ready.ID); strings.Contains(rec.Body.String(), "manifest") {
		t.Error("the embed page links the manifest")
	}
}
