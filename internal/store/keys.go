package store

import (
	"mime"
	"path"
	"strings"
)

// Key layout (§6). keyPrefix is S3_KEY_PREFIX: "" or ending in "/". These
// functions are the ONLY place keys are built for writing; the database
// stores the exact keys for reading.

// ClipPrefix is "{P}clips/{id}/" — the DeletePrefix target.
func ClipPrefix(keyPrefix, clipID string) string {
	return keyPrefix + "clips/" + clipID + "/"
}

// OriginalKey is the immutable upload: "{P}clips/{id}/original/source.{ext}".
func OriginalKey(keyPrefix, clipID, ext string) string {
	return ClipPrefix(keyPrefix, clipID) + "original/source." + ext
}

// ReplacementKey is the per-attempt original of a REPLACEMENT upload
// (ROADMAP #31, §20 erratum 135): "…/original/source-{r8}.{ext}".
//
// A replacement cannot be written to OriginalKey. That key is what the live
// row points at, the file `/original` is serving and the file the worker
// re-downloads when its scratch copy is gone; overwriting it would destroy
// the upload the clip still IS, before the replacement had proved it can be
// transcoded at all — the opposite of "upload is sacred". So the new original
// lands beside it under a fresh key, and the old one is deleted only after the
// commit that points the row at the new rendition (§20 erratum 135).
func ReplacementKey(keyPrefix, clipID, r8, ext string) string {
	return ClipPrefix(keyPrefix, clipID) + "original/source-" + r8 + "." + ext
}

// RenditionKey is the per-attempt progressive MP4: "…/renditions/main-{r8}.mp4".
func RenditionKey(keyPrefix, clipID, r8 string) string {
	return ClipPrefix(keyPrefix, clipID) + "renditions/main-" + r8 + ".mp4"
}

// AudioRenditionKey is an audio clip's per-attempt Full (§20 erratum 328):
// "…/renditions/main-{r8}.flac" or ".m4a" — the extension is the file's own, so
// the bucket never holds a FLAC under a name that says mp4.
func AudioRenditionKey(keyPrefix, clipID, r8, ext string) string {
	return ClipPrefix(keyPrefix, clipID) + "renditions/main-" + r8 + "." + ext
}

// PosterKey is the per-attempt poster JPEG: "…/poster/poster-{r8}.jpg".
func PosterKey(keyPrefix, clipID, r8 string) string {
	return ClipPrefix(keyPrefix, clipID) + "poster/poster-" + r8 + ".jpg"
}

// PNGPosterKey is an AUDIO clip's poster, the drawn waveform (§20 erratum
// 329): "…/poster/poster-{r8}.png", beside where a video's JPEG would be.
func PNGPosterKey(keyPrefix, clipID, r8 string) string {
	return ClipPrefix(keyPrefix, clipID) + "poster/poster-" + r8 + ".png"
}

// SocialKey is the per-attempt social rendition: "…/renditions/social-{r8}.mp4"
// (§6, §17 #3). It is another rendition of the clip — derived, replaceable
// and re-derivable — so it lives beside the main MP4 under the same clip
// prefix, which is what makes the clip delete, `admin orphans` and the trim's
// supersede list cover it without any of them learning a new place to look.
func SocialKey(keyPrefix, clipID, r8 string) string {
	return ClipPrefix(keyPrefix, clipID) + "renditions/social-" + r8 + ".mp4"
}

// GIFKey is the per-attempt preview GIF: "…/renditions/preview-{r8}.gif"
// (§6, §17 #6). It is a rendition of the clip — derived, replaceable and
// re-derivable — so it lives beside the MP4 rather than under poster/.
func GIFKey(keyPrefix, clipID, r8 string) string {
	return ClipPrefix(keyPrefix, clipID) + "renditions/preview-" + r8 + ".gif"
}

// FilmstripKey is the per-attempt trim-scrubber sprite sheet:
// "…/filmstrip/strip-{r8}.jpg" (§6, §20 erratum 220).
//
// It gets a directory of its own rather than joining `renditions/` (where the
// preview GIF lives) or `poster/`: it is neither a rendition of the clip nor
// the picture a card shows, it is one control's tool, and a name that says so
// is what stops the next reader of `admin ls` from mistaking a
// 3840-pixel-wide JPEG for the card image. It is under the clip prefix like
// everything else, so the clip delete, `admin orphans` and the trim's
// supersede list cover it without learning a new place to look.
func FilmstripKey(keyPrefix, clipID, r8 string) string {
	return ClipPrefix(keyPrefix, clipID) + "filmstrip/strip-" + r8 + ".jpg"
}

// ProbeKey is the never-served probe/plan JSON: "…/meta/probe.json".
func ProbeKey(keyPrefix, clipID string) string {
	return ClipPrefix(keyPrefix, clipID) + "meta/probe.json"
}

// Keys bundles the fixed keys of one clip (the attempt-scoped rendition and
// poster keys take an r8 and are built separately).
type Keys struct {
	Prefix   string
	Original string
	Probe    string
}

// ReservedDir is the one prefix under S3_KEY_PREFIX that is the INSTANCE's own
// rather than a clip's (§20 erratum 343): "{P}_clippet/". It sits beside
// "{P}clips/", never inside it, so every reader of the clips prefix — `admin
// orphans`, the sweeper's DeletePrefix of one clip, `admin stats`' bucket line
// — cannot reach it by construction, and IsReserved says so in words where a
// caller lists more widely than that.
const ReservedDir = "_clippet/"

// ReservedPrefix is "{P}_clippet/".
func ReservedPrefix(keyPrefix string) string { return keyPrefix + ReservedDir }

// SnapshotKey is the database snapshot that follows the clips into the bucket
// (§20 erratum 343): "{P}_clippet/db.sqlite". One object, overwritten in place.
func SnapshotKey(keyPrefix string) string { return ReservedPrefix(keyPrefix) + "db.sqlite" }

// SnapshotSumKey is its sidecar, the lowercase hex sha256 of the file and
// nothing else, written AFTER the database object so a reader that finds the
// two agreeing knows it holds a whole one.
func SnapshotSumKey(keyPrefix string) string { return SnapshotKey(keyPrefix) + ".sha256" }

// SnapshotMetaKey is the small JSON beside it: when it was written, by which
// commit, and how many clips it carries.
func SnapshotMetaKey(keyPrefix string) string { return SnapshotKey(keyPrefix) + ".meta" }

// IsReserved reports whether key lies under this instance's reserved prefix.
func IsReserved(keyPrefix, key string) bool {
	return strings.HasPrefix(key, ReservedPrefix(keyPrefix))
}

// KeysFor returns the fixed keys for a clip whose original has extension ext
// (already sanitised).
func KeysFor(keyPrefix, clipID, ext string) Keys {
	return Keys{
		Prefix:   ClipPrefix(keyPrefix, clipID),
		Original: OriginalKey(keyPrefix, clipID, ext),
		Probe:    ProbeKey(keyPrefix, clipID),
	}
}

const (
	minExtLen = 1
	maxExtLen = 8
	// DefaultExt is used when the filename's extension is unusable.
	DefaultExt = "bin"
)

// SanitizeExt returns the lowercase extension of filename when it matches
// [a-z0-9]{1,8}, else "bin". The result is the only user-influenced text
// that ever reaches a key.
func SanitizeExt(filename string) string {
	// Windows clients may send backslash paths; take the last segment either way.
	if i := strings.LastIndexAny(filename, `\/`); i >= 0 {
		filename = filename[i+1:]
	}
	// A leading dot is not an extension separator: ".hidden" has no
	// extension even though path.Ext would report ".hidden".
	filename = strings.TrimLeft(filename, ".")
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(filename), "."))
	if len(ext) < minExtLen || len(ext) > maxExtLen {
		return DefaultExt
	}
	for i := 0; i < len(ext); i++ {
		c := ext[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return DefaultExt
		}
	}
	return ext
}

// extContentTypes is the extension's first opinion. Since §20 erratum 328 it
// knows the audio spellings too (round-2 research §5.9); for an audio clip the
// probe has the LAST word — the strip writes the file into a container of its
// choosing and stores the media type that container is (transcode.StripTarget).
var extContentTypes = map[string]string{
	"mp4":  "video/mp4",
	"m4v":  "video/mp4",
	"mov":  "video/quicktime",
	"webm": "video/webm",
	"mkv":  "video/x-matroska",
	"avi":  "video/x-msvideo",
	"ts":   "video/mp2t",
	"mp3":  "audio/mpeg",
	"m4a":  "audio/mp4",
	"flac": "audio/flac",
	"wav":  "audio/wav",
	"ogg":  "audio/ogg",
	"oga":  "audio/ogg",
	"opus": "audio/ogg",
	"weba": "audio/webm",
	"aif":  "audio/aiff",
	"aiff": "audio/aiff",
	"caf":  "audio/x-caf",
	"amr":  "audio/amr",
	"wv":   "audio/x-wavpack",
}

// MaxContentTypeLen bounds the media type a multipart part may dictate (§20
// erratum 40). The part header is client-controlled and Go's mime/multipart
// accepts megabytes of it, while the accepted value is stored in
// clips.source_content_type, echoed in every ClipDTO (the shared library
// listing included), sent as the object's Content-Type metadata and emitted
// verbatim as the response header of GET /original. The longest registered
// video/* type is far under this bound, so §6's "the part's Content-Type if
// it begins with video/" rule is unchanged for every real client.
const MaxContentTypeLen = 127

// ContentTypeFor picks the stored Content-Type of an original: the multipart
// part's media type when it begins with "video/" or "audio/" and is no longer
// than MaxContentTypeLen, else by extension, else application/octet-stream (§6;
// "audio/" since §20 erratum 328, whose probe then decides for an audio clip:
// browsers send `audio/x-wav`, `audio/vnd.wave`, `audio/x-m4a`, even
// `video/ogg` and `video/mp4` for files with no picture in them).
func ContentTypeFor(partCT, ext string) string {
	if mt, _, err := mime.ParseMediaType(partCT); err == nil {
		mt = strings.ToLower(mt)
		if (strings.HasPrefix(mt, "video/") || strings.HasPrefix(mt, "audio/")) && len(mt) <= MaxContentTypeLen {
			return mt
		}
	}
	if ct, ok := extContentTypes[strings.ToLower(ext)]; ok {
		return ct
	}
	return "application/octet-stream"
}
