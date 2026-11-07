package autorule

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const goodURL = "https://cdn.discordapp.com/attachments/1419000000000000003/1419000000000000004/a.mp4"

func body(extra string) string {
	return `{"guild_id":"1419000000000000002","url":"` + goodURL + `","filename":"a.mp4","content_type":"video/mp4","size":12` + extra + `}`
}

// The route's decoder: one object, no unknown field, nothing after it, the
// grammar; the body's cap comes back as the cap.
func TestDecode(t *testing.T) {
	in, err := Decode(strings.NewReader(body("")))
	if err != nil || in.GuildID != "1419000000000000002" || in.Size != 12 || in.Duplicate != "" {
		t.Fatalf("%+v %v", in, err)
	}
	if in, err := Decode(strings.NewReader(body(`,"duplicate":"allow"`))); err != nil || in.Duplicate != DuplicateAllow {
		t.Fatalf("duplicate: %+v %v", in, err)
	}
	for _, bad := range []string{body(`,"at":1`), body("") + "{}", `{"guild_id":"1"}`, "", "[]", body(`,"duplicate":"no"`),
		strings.Replace(body(""), `"size":12`, `"size":0`, 1), strings.Replace(body(""), "a.mp4\",\"content", "a\\u0000.mp4\",\"content", 1)} {
		if _, err := Decode(strings.NewReader(bad)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted %q: %v", bad, err)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body("")+strings.Repeat(" ", MaxBody)))
	_, err = Decode(http.MaxBytesReader(w, r.Body, 64))
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Fatalf("the cap came back as %v", err)
	}
}

func TestApplicable(t *testing.T) {
	in := Instruction{GuildID: "1419000000000000002", URL: goodURL, Filename: "a.mp4", ContentType: "video/mp4", Size: 100}
	if !Applicable(in, 1000) {
		t.Fatal("a video under the cap")
	}
	a := in
	a.ContentType, a.Filename = "audio/ogg", "voice.ogg"
	if !Applicable(a, 1000) {
		t.Fatal("a sound")
	}
	for name, mut := range map[string]func(*Instruction){
		"over the cap":  func(i *Instruction) { i.Size = 1001 },
		"not media":     func(i *Instruction) { i.ContentType, i.Filename = "image/png", "a.png" },
		"another host":  func(i *Instruction) { i.URL = strings.Replace(goodURL, "cdn.discordapp.com", "evil.example", 1) },
		"an avatar":     func(i *Instruction) { i.URL = "https://cdn.discordapp.com/avatars/1419000000000000003/abc.png" },
		"a dot segment": func(i *Instruction) { i.URL = strings.Replace(goodURL, "a.mp4", "%2e%2e", 1) },
	} {
		b := in
		mut(&b)
		if Applicable(b, 1000) {
			t.Fatalf("%s: applicable", name)
		}
	}
}
