package store

import (
	"regexp"
	"strings"
	"testing"

	"clippet/internal/ids"
)

var keyCharset = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

func TestKeyShapes(t *testing.T) {
	id := "aZ3kQ9xB2mLp"
	cases := map[string]string{
		ClipPrefix("", id):                "clips/aZ3kQ9xB2mLp/",
		ClipPrefix("clippet/", id):        "clippet/clips/aZ3kQ9xB2mLp/",
		OriginalKey("", id, "mp4"):        "clips/aZ3kQ9xB2mLp/original/source.mp4",
		OriginalKey("p/", id, "bin"):      "p/clips/aZ3kQ9xB2mLp/original/source.bin",
		RenditionKey("", id, "AbCdEfGh"):  "clips/aZ3kQ9xB2mLp/renditions/main-AbCdEfGh.mp4",
		PosterKey("", id, "AbCdEfGh"):     "clips/aZ3kQ9xB2mLp/poster/poster-AbCdEfGh.jpg",
		GIFKey("", id, "AbCdEfGh"):        "clips/aZ3kQ9xB2mLp/renditions/preview-AbCdEfGh.gif",
		GIFKey("p/", id, "AbCdEfGh"):      "p/clips/aZ3kQ9xB2mLp/renditions/preview-AbCdEfGh.gif",
		SocialKey("", id, "AbCdEfGh"):     "clips/aZ3kQ9xB2mLp/renditions/social-AbCdEfGh.mp4",
		SocialKey("p/", id, "AbCdEfGh"):   "p/clips/aZ3kQ9xB2mLp/renditions/social-AbCdEfGh.mp4",
		ProbeKey("", id):                  "clips/aZ3kQ9xB2mLp/meta/probe.json",
		KeysFor("x/", id, "mov").Original: "x/clips/aZ3kQ9xB2mLp/original/source.mov",
		KeysFor("x/", id, "mov").Prefix:   "x/clips/aZ3kQ9xB2mLp/",
		KeysFor("x/", id, "mov").Probe:    "x/clips/aZ3kQ9xB2mLp/meta/probe.json",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	// Every rendition/poster key lives under the clip prefix.
	for _, k := range []string{RenditionKey("p/", id, "r"), PosterKey("p/", id, "r"), GIFKey("p/", id, "r"),
		SocialKey("p/", id, "r"), ProbeKey("p/", id), OriginalKey("p/", id, "mp4"),
		ReplacementKey("p/", id, "r", "mp4")} {
		if !strings.HasPrefix(k, ClipPrefix("p/", id)) {
			t.Errorf("%q not under clip prefix", k)
		}
	}
	// The REPLACEMENT original (ROADMAP #31, §20 erratum 135) sits beside the
	// live one under a per-attempt r8, and is never the same key: writing a
	// replacement over OriginalKey would destroy the upload the clip still is
	// before the new one had proved it can be transcoded.
	repl := map[string]string{
		ReplacementKey("", id, "AbCdEfGh", "mp4"):   "clips/aZ3kQ9xB2mLp/original/source-AbCdEfGh.mp4",
		ReplacementKey("p/", id, "AbCdEfGh", "mov"): "p/clips/aZ3kQ9xB2mLp/original/source-AbCdEfGh.mov",
	}
	for got, want := range repl {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if ReplacementKey("", id, "AbCdEfGh", "mp4") == OriginalKey("", id, "mp4") {
		t.Error("a replacement would overwrite the live original")
	}
}

func TestKeysNoTraversal(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := ids.New()
		r8 := ids.Rand(8)
		for _, ext := range []string{"mp4", "bin", SanitizeExt("../../etc/passwd"), SanitizeExt("a.M K V")} {
			for _, k := range []string{
				KeysFor("", id, ext).Original, KeysFor("clippet/", id, ext).Prefix, KeysFor("", id, ext).Probe,
				RenditionKey("", id, r8), PosterKey("clippet/", id, r8), GIFKey("", id, r8), SocialKey("", id, r8),
				ReplacementKey("", id, r8, ext),
			} {
				if !keyCharset.MatchString(k) {
					t.Fatalf("key %q outside charset", k)
				}
				if strings.Contains(k, "..") {
					t.Fatalf("key %q contains ..", k)
				}
			}
		}
	}
}

func TestSanitizeExt(t *testing.T) {
	cases := map[string]string{
		"clip.mp4":                "mp4",
		"CLIP.MP4":                "mp4",
		"clip.MoV":                "mov",
		"archive.tar.gz":          "gz",
		"noext":                   "bin",
		"":                        "bin",
		".hidden":                 "bin", // dotfile, no extension
		"..hidden":                "bin",
		".hidden.mp4":             "mp4",
		"clip.":                   "bin",
		"clip.m4v":                "m4v",
		"clip.abcdefgh":           "abcdefgh",
		"clip.abcdefghi":          "bin", // 9 chars
		"clip.mp 4":               "bin",
		"clip.mp-4":               "bin",
		"clip.мп4":                "bin",
		"../../etc/passwd":        "bin",
		`C:\Videos\clip.webm`:     "webm",
		"/tmp/x/clip.mkv":         "mkv",
		"weird.name.with.dots.ts": "ts",
		"clip.MP4?x=1":            "bin",
	}
	for in, want := range cases {
		if got := SanitizeExt(in); got != want {
			t.Errorf("SanitizeExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentTypeFor(t *testing.T) {
	cases := []struct {
		partCT, ext, want string
	}{
		{"video/mp4", "mp4", "video/mp4"},
		{"VIDEO/QuickTime; codecs=x", "mp4", "video/quicktime"},
		{"video/x-custom", "bin", "video/x-custom"},
		{"application/octet-stream", "mp4", "video/mp4"},
		{"", "m4v", "video/mp4"},
		{"", "mov", "video/quicktime"},
		{"", "webm", "video/webm"},
		{"", "mkv", "video/x-matroska"},
		{"", "avi", "video/x-msvideo"},
		{"", "ts", "video/mp2t"},
		{"", "MP4", "video/mp4"},
		{"text/plain", "bin", "application/octet-stream"},
		{"garbage;;", "xyz", "application/octet-stream"},
		{"image/jpeg", "jpg", "application/octet-stream"},
		// §20 erratum 40: a part type longer than MaxContentTypeLen is not a
		// media type any client means — it is stored, echoed in every DTO and
		// re-emitted as a response header — so it falls through to the
		// extension map exactly like a non-video type.
		{"video/" + strings.Repeat("z", MaxContentTypeLen), "mp4", "video/mp4"},
		{"video/" + strings.Repeat("z", MaxContentTypeLen), "xyz", "application/octet-stream"},
		{"video/" + strings.Repeat("z", 3000), "mov", "video/quicktime"},
		// The bound itself is inclusive, and a real type is nowhere near it.
		{"video/" + strings.Repeat("z", MaxContentTypeLen-len("video/")), "mp4", "video/" + strings.Repeat("z", MaxContentTypeLen-len("video/"))},
	}
	for _, c := range cases {
		if got := ContentTypeFor(c.partCT, c.ext); got != c.want {
			t.Errorf("ContentTypeFor(%.40q…, %q) = %.40q…, want %.40q…", c.partCT, c.ext, got, c.want)
		}
		if got := ContentTypeFor(c.partCT, c.ext); len(got) > MaxContentTypeLen {
			t.Errorf("ContentTypeFor(%.40q…, %q) returned %d bytes", c.partCT, c.ext, len(got))
		}
	}
}
