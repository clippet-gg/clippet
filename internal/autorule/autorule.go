// Package autorule is the Clippet Auto instruction and its rule in ONE place
// (§20 errata 324, 333, 334): the JSON body POST /in/auto/{token} takes, the
// decoder that route runs, and "would the box take this file" — the grammar,
// the attachment URL (config's CDN allowlist, then the attachment path), a
// video or a sound, under a size cap.
//
// Three callers, one rule: the route itself (internal/httpapi/autofetch.go),
// the box's own gateway (internal/httpapi/autobox.go, erratum 333), and the
// HOSTED Clippet Auto (cmd/clippet-auto, erratum 334), which encodes the
// instruction a box decodes here. It imports config and store's two pure
// media-type helpers and nothing that opens a socket.
package autorule

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"clippet/internal/config"
	"clippet/internal/store"
)

// MaxBody bounds the instruction: five short fields (and the optional sixth).
const MaxBody = 4 << 10

// DuplicateAllow is the duplicate check's opt-out value (§20 erratum 326).
const DuplicateAllow = "allow"

// Instruction is the whole body: it names one Discord attachment and nothing
// else. Unknown fields are refused.
//
// Duplicate is the duplicate check's opt-out (§20 erratum 326), optional:
// `allow` makes this a new clip even when the account already holds one of
// exactly these bytes; absent or empty leaves the check on.
type Instruction struct {
	GuildID     string `json:"guild_id"`
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Duplicate   string `json:"duplicate,omitempty"`
}

// snowflake is a Discord id as the route accepts one.
var snowflake = regexp.MustCompile(`^[0-9]{17,20}$`)

// attachmentPath is the only path the route fetches on a CDN host (§20
// erratum 324's review item 1): /attachments/{channel}/{attachment}/{name}.
// config.ValidAttachmentURL stays shared and untouched — `/upload` is handed
// Discord-signed URLs and may legitimately see other paths.
var attachmentPath = regexp.MustCompile(`^/attachments/[0-9]{17,20}/[0-9]{17,20}/[^/]{1,512}$`)

// Valid is the instruction's grammar.
func (in Instruction) Valid() bool {
	return snowflake.MatchString(in.GuildID) &&
		in.URL != "" && len(in.URL) <= 2048 &&
		cleanField(in.Filename, MaxBody, false) &&
		cleanField(in.ContentType, 127, true) &&
		in.Size > 0 &&
		(in.Duplicate == "" || in.Duplicate == DuplicateAllow)
}

// cleanField: valid UTF-8, no control character, at most max bytes, and
// non-empty unless empty is allowed.
func cleanField(s string, max int, empty bool) bool {
	if len(s) > max || (s == "" && !empty) || !utf8.ValidString(s) {
		return false
	}
	return strings.IndexFunc(s, unicode.IsControl) < 0
}

// ErrInvalid is every refusal Decode makes that is not the body's size.
var ErrInvalid = errors.New("instruction must be JSON: guild_id, url, filename, content_type, size")

// Decode is THE decoder the route runs: one JSON object, no unknown field,
// nothing after it, and the grammar. A read error that is the body's cap
// (*http.MaxBytesError) comes back as it is, so the route can answer 413.
func Decode(r io.Reader) (Instruction, error) {
	var in Instruction
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return Instruction{}, err
		}
		return Instruction{}, ErrInvalid
	}
	if _, err := dec.Token(); err != io.EOF || !in.Valid() {
		return Instruction{}, ErrInvalid
	}
	return in, nil
}

// AttachmentURL is the whole URL rule: config's allowlist (https, no
// userinfo, one of the two CDN hosts exactly), then the attachment path, whose
// last segment may not be `.` or `..` in any spelling.
func AttachmentURL(raw string) (*url.URL, bool) {
	u, err := config.ValidAttachmentURL(raw)
	if err != nil {
		return nil, false
	}
	p := u.EscapedPath()
	if !attachmentPath.MatchString(p) {
		return nil, false
	}
	last, err := url.PathUnescape(p[strings.LastIndexByte(p, '/')+1:])
	if err != nil || last == "." || last == ".." || strings.Contains(last, "/") {
		return nil, false
	}
	return u, true
}

// IsMedia is the cheap first opinion on an attachment: Discord's own content
// type, or the filename's extension — the same pair the ingest stores — naming
// a video or, since §20 erratum 328, a sound. It is not the gate; ffprobe is,
// and it runs on the bytes (§8.3 step 6).
func IsMedia(filename, contentType string) bool {
	ct := store.ContentTypeFor(contentType, store.SanitizeExt(filename))
	return strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/")
}

// Applicable is "the box would take this file", in the order the route checks
// it: the grammar, the attachment URL, a video or a sound, at most maxBytes.
// The binding is not part of it: that is the box's own answer.
func Applicable(in Instruction, maxBytes int64) bool {
	if !in.Valid() {
		return false
	}
	if _, ok := AttachmentURL(in.URL); !ok {
		return false
	}
	return IsMedia(in.Filename, in.ContentType) && in.Size <= maxBytes
}
