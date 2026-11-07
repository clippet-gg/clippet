package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"time"
)

// staticCacheControl is §9's policy for embedded assets: an hour, with
// ?v=buildID busting in the URLs and a content ETag for revalidation.
const staticCacheControl = "public, max-age=3600"

// staticVersionToken is what a static file writes where it references ANOTHER
// static file by URL — today the two @font-face `src`s in app.css. A URL a
// template renders carries ?v=buildID (urls.Builder.Static), but a URL inside
// a stylesheet has no renderer, so a replaced woff2 would be served from a
// browser's cache for the hour staticCacheControl grants. The token is
// substituted once, here, at boot, and the ETag is taken from the result.
const staticVersionToken = "?v=__V__"

// staticHandler serves the embedded web/static files. The ETag map doubles
// as the allow-list: only regular files of the FS exist, at the top level or
// under fonts/ (§20 erratum 240 — the self-hosted faces), and a name the map
// does not hold is the HTML 404 page. bodies holds the few files whose
// content is rewritten at boot (the version token); everything else is served
// straight out of the embedded FS.
type staticHandler struct {
	fsys   fs.FS
	etags  map[string]string
	bodies map[string][]byte
}

func newStaticHandler(fsys fs.FS, buildID string) (*staticHandler, error) {
	h := &staticHandler{fsys: fsys, etags: make(map[string]string), bodies: make(map[string][]byte)}
	err := fs.WalkDir(fsys, ".", func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("web: read static assets: %w", err)
		}
		if !e.Type().IsRegular() {
			return nil
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("web: read static %s: %w", name, err)
		}
		// The token is substituted before the ETag is taken, so the hash is
		// always the hash of what a browser is handed. An empty build id (a
		// test, a `go run`) drops the query the way urls.Static does.
		if bytes.Contains(b, []byte(staticVersionToken)) {
			v := ""
			if buildID != "" {
				v = "?v=" + url.QueryEscape(buildID)
			}
			b = bytes.ReplaceAll(b, []byte(staticVersionToken), []byte(v))
			h.bodies[name] = b
		}
		sum := sha256.Sum256(b)
		h.etags[name] = `"` + hex.EncodeToString(sum[:])[:32] + `"`
		return nil
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}

// staticTypes are the media types this package SPELLS for the embedded FS,
// rather than leaving to whatever `mime.TypeByExtension` answers where the
// binary happens to be running.
//
// Two of the rows are there because Go's compiled-in table genuinely has no
// answer (`.woff2`, `.ttf`): without them, and with no /etc/mime.types in the
// container image, a font is served as application/octet-stream — which
// browsers tolerate, but a font is a font. The rest (`.txt`, `.ico`) ARE in
// that table, with these same values, and are written out anyway: Go loads the
// system mime database over its builtins at init, so an image that ships an
// /etc/mime.types saying `.ico` is image/x-icon would change what this server
// puts on the wire. A row here makes the served type a property of this file —
// which is what `clippet-pass` `guard/scripts/manifest.mjs` mirrors, and what
// a test can pin.
var staticTypes = map[string]string{
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
	// The poster label's face (§20 erratum 287). It is in fonts/ because it is
	// the same family and the same licence as the two woff2 files beside it, and
	// the OFL.txt that covers all three lives there; the image installs a copy of
	// it for ffmpeg, and this row is what keeps the SERVED one a font rather than
	// an octet-stream. Nothing on any page requests it.
	".ttf": "font/ttf",
	// The favicon (§20 erratum 304). `image/vnd.microsoft.icon` is the IANA
	// registration; `image/x-icon` is the older spelling, which every browser
	// also accepts and which plenty of system mime databases still carry. One
	// of the two has to be THE answer for both `/favicon.ico` and
	// `/static/favicon.ico`, and the guard's manifest records it for a Pass
	// name, so it is written here rather than inherited: Go's own table happens
	// to agree today (go1.27 `mime.builtinTypesLower`), and this row is what
	// makes that a decision instead of a coincidence. Belt and braces, like
	// `.ttf` — deleting it does not break the wire today, and it is what stops
	// a toolchain bump or an image with an /etc/mime.types from moving it.
	".ico": "image/vnd.microsoft.icon",
}

// staticFile serves /static/{file...}; unknown names get the HTML 404 page.
func (s *server) staticFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	etag, ok := s.static.etags[name]
	if !ok {
		s.notFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", staticCacheControl)
	h.Set("ETag", etag)
	if ct, ok := staticTypes[path.Ext(name)]; ok {
		h.Set("Content-Type", ct)
	}
	// A rewritten body (the version token) is served from memory; ServeContent
	// answers HEAD, Range and If-None-Match against the ETag above exactly as
	// ServeFileFS does, and the zero modtime keeps Last-Modified off, as the
	// embedded FS's own zero times do.
	if body, ok := s.static.bodies[name]; ok {
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
		return
	}
	// ServeFileFS sets Content-Type by extension (unless set above), handles
	// HEAD, Range and If-None-Match against the ETag, and reads only the
	// embedded FS.
	http.ServeFileFS(w, r, s.static.fsys, name)
}
