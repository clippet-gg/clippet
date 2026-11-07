package httpapi

import (
	"net/http"
	"path"
	"strings"
	"time"

	"clippet/internal/db"
	"clippet/internal/httpx"
)

// Audio clips over HTTP (DESIGN.md §20 erratum 328).
//
// ONE new route, GET /api/clips/{id}/audio: the audio clip's best playable tier
// — the stored original when it plays (MP3, AAC, Opus, Vorbis, FLAC), the Full
// otherwise (a FLAC, or an AAC in an MP4) — under its REAL media type, with the
// Range support every media route has. It exists so a FLAC is never served
// under a name that says `.mp4`: `/video.mp4` and `/light.mp4` keep serving the
// card copy, an H.264 + AAC MP4 of the clip's picture under its sound, because
// that is what every card tag and every embedded player already fetches.
//
// The verification plane's guard (clippet-pass) pins every route to its
// manifest; until its next release lists this one, a Pass name answers it 404
// (a nit sweep, run 8).

// notForAudio is the one sentence the picture-only routes answer an audio clip
// with: trim, the preview GIF (both routes), the filmstrip, the chosen card
// frame and the replacement.
const notForAudio = "Not for audio."

// refuseAudio answers 409 not_for_audio for an audio clip and reports whether it
// did. It runs after the route's own lookup or ownership check, so a stranger's
// trim of somebody's audio clip is still the 403 it always was.
func refuseAudio(w http.ResponseWriter, c db.Clip) bool {
	if !c.IsAudio() {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteError(w, http.StatusConflict, "not_for_audio", notForAudio)
	return true
}

func (h *handler) audio(w http.ResponseWriter, r *http.Request) { h.serveMedia(w, r, mediaAudio) }

// audioObject is the audio route's object: db.PlayObject — the stored original
// when it plays, the Full when it does not — under the media type it was
// stored as. A video clip has no audio object: ok=false with notAudio=true,
// which the route answers 404.
func audioObject(c db.Clip) (o object, notAudio, ok bool) {
	if !c.IsAudio() {
		return object{}, true, false
	}
	key, size, etag, ok := db.PlayObject(c)
	if !ok {
		return object{}, false, false
	}
	ct, mod := db.AudioPlayType(c), c.UpdatedAt
	if key == c.SourceKey {
		mod = c.CreatedAt
	}
	return object{key: key, size: size, etag: etag, contentType: ct,
		modTime: time.Unix(mod, 0), redirectable: true}, false, true
}

// audioFilename is the name an audio clip's `?download=1` lands as: the title
// slugged, and the extension the served object really has.
func audioFilename(c db.Clip, key string) string {
	ext := strings.TrimPrefix(path.Ext(key), ".")
	if ext == "" {
		ext = "bin"
	}
	return titleSlug(c.Title, c.ID) + "." + ext
}

// audioAttachmentName is an audio clip's ORIGINAL download name: the person's
// own filename with the extension of the file the strip stored, so a `.mov`
// voice memo that is stored as an `.m4a` downloads as one.
func audioAttachmentName(c db.Clip) string {
	name := attachmentName(c.SourceFilename, c.SourceKey)
	ext := path.Ext(c.SourceKey)
	if ext == "" {
		return name
	}
	return strings.TrimSuffix(name, path.Ext(name)) + ext
}
