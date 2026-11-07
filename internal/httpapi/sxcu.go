package httpapi

import "clippet/internal/urls"

// The ShareX custom-uploader document Clippet emits (§17; §20 errata 59, 60).
//
// It can only be built at MINT time: the raw token is shown exactly once
// (§14), so a `GET /clippet.sxcu` route could never carry a credential. The
// mint response therefore carries the finished file as an `sxcu` object and
// the /settings page turns it into a download in the browser — the raw
// token never reaches a GET URL, an access log or a server-side file.
//
// There are TWO mints (§20 erratum 308): this package's `POST /api/auth/tokens`
// and `clippet admin tokens mint --sxcu`, which writes the same file to stdout
// for an operator whose shell is the only way in. Both go through NewSXCU
// below, because two generators of one file is how a field stops agreeing with
// itself: the CLI writes the file a person imports and the route writes the one
// the browser downloads, and a difference between them would show up as a
// capture tool that works from one door and not the other.
//
// Field names are ShareX's own (PascalCase) and the values are its `{…}`
// parser syntax, so the object is the file: writing it out verbatim produces
// an importable clippet.sxcu.
const (
	sxcuVersion = "14.1.0"
	sxcuName    = "Clippet"
	// FileUploader ONLY. ShareX's import offers to activate the destinations
	// a file declares, so naming ImageUploader here would quietly make
	// Clippet the screenshot destination — and every screenshot would come
	// back 415 not_a_video from the ffprobe gate (§8.3 step 6). Clippet
	// stores videos; a clip is a file, and that is the one destination it
	// should ever claim (§20 erratum 59).
	sxcuDestinationType = "FileUploader"
	sxcuRequestMethod   = "POST"
	sxcuBody            = "MultipartFormData"
	sxcuFileFormName    = "file"
	sxcuURL             = "{json:url}" // the share page: what ShareX puts on the clipboard
	sxcuErrorMessage    = "{json:message}"
)

// SXCU is one ShareX custom-uploader file.
//
// Two fields are deliberately absent. ThumbnailURL: ShareX expects an image
// URL there and the only per-clip URLs this endpoint returns are HTML pages,
// so it would render broken on every upload. Arguments: a `title={filename}`
// argument looks helpful but ShareX expands `{filename}` WITH the extension,
// so every card would read "2026-09-04_18-22-13.mp4"; sending no title at all
// lets the server's own fallback name the clip from the filename without it
// (§20 erratum 60). Both are omitted from the JSON rather than emitted empty.
type SXCU struct {
	Version         string            `json:"Version"`
	Name            string            `json:"Name"`
	DestinationType string            `json:"DestinationType"`
	RequestMethod   string            `json:"RequestMethod"`
	RequestURL      string            `json:"RequestURL"`
	Headers         map[string]string `json:"Headers"`
	Body            string            `json:"Body"`
	FileFormName    string            `json:"FileFormName"`
	Arguments       map[string]string `json:"Arguments,omitempty"`
	URL             string            `json:"URL"`
	ErrorMessage    string            `json:"ErrorMessage"`
}

// NewSXCU builds the uploader file for one freshly minted raw token. It is
// exported for `clippet admin tokens mint --sxcu`, which is a mint with no
// browser session behind it (§20 erratum 308); nothing else outside this
// package may build one, because nothing else holds a raw token.
func NewSXCU(u urls.Builder, rawToken string) *SXCU {
	return &SXCU{
		Version:         sxcuVersion,
		Name:            sxcuName,
		DestinationType: sxcuDestinationType,
		RequestMethod:   sxcuRequestMethod,
		RequestURL:      u.Abs("/api/upload"),
		Headers:         map[string]string{"Authorization": "Bearer " + rawToken},
		Body:            sxcuBody,
		FileFormName:    sxcuFileFormName,
		URL:             sxcuURL,
		ErrorMessage:    sxcuErrorMessage,
	}
}
