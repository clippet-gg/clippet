package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"clippet/internal/autobot"
	"clippet/internal/db"
)

// Clippet Auto on the box (§20 erratum 333): the §30 endpoint is the second
// door a press can arrive at, and the instance side autobot is handed is
// POST /in/auto's own rule and job.

type fakeAuto struct {
	mu      sync.Mutex
	host    autobot.Host
	presses [][]byte
	started int
}

func (f *fakeAuto) SetHost(h autobot.Host) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.host = h
}

func (f *fakeAuto) Press(_ context.Context, raw []byte) (map[string]any, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presses = append(f.presses, append([]byte(nil), raw...))
	return map[string]any{"type": 4, "data": map[string]any{"content": "auto-answer", "flags": 64}}, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.started++
	}
}

func componentBody(customID string) string {
	b, _ := json.Marshal(map[string]any{"type": 3, "id": "1", "token": "t", "guild_id": afGuild,
		"data": map[string]any{"custom_id": customID, "component_type": 2}})
	return string(b)
}

// Pin: the button's custom id at the §30 endpoint goes to the ONE press
// function — its answer is the response, and start runs; any other component
// id never reaches it.
func TestAutoPressRoutedAtTheEndpoint(t *testing.T) {
	fa := &fakeAuto{}
	b := newBotEnv(t, func(d *Deps) { d.Auto = fa })
	r := b.post(componentBody(autobot.PressID))
	if r.code != 200 || !strings.Contains(string(r.body), "auto-answer") {
		t.Fatalf("press: %d %s", r.code, r.body)
	}
	fa.mu.Lock()
	n, started := len(fa.presses), fa.started
	fa.mu.Unlock()
	if n != 1 || started != 1 {
		t.Fatalf("presses=%d started=%d", n, started)
	}
	b.post(componentBody("cb:x"))
	fa.mu.Lock()
	n = len(fa.presses)
	fa.mu.Unlock()
	if n != 1 {
		t.Fatal("a panel click reached Clippet Auto")
	}
	if fa.host == nil {
		t.Fatal("NewHandler did not hand Clippet Auto the instance side")
	}
	// Without Auto the id is just an unknown component.
	off := newBotEnv(t)
	if r := off.post(componentBody(autobot.PressID)); strings.Contains(string(r.body), "auto-answer") {
		t.Fatal("answered with Auto off")
	}
}

func autoAtt(mut ...func(*autobot.Attachment)) autobot.Attachment {
	a := autobot.Attachment{ID: "1", Filename: "clip.mp4", URL: afCDN, Size: 4000, ContentType: "video/mp4"}
	for _, f := range mut {
		f(&a)
	}
	return a
}

// Pin: Bound is the binding; Applicable is POST /in/auto's rule; Upload runs
// that route's job in-process as the bound account and answers the share link.
func TestAutoHostIsTheAutoFetchPath(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	fa := &fakeAuto{}
	e, _ := afEnv(t, stub, func(d *Deps) { d.Auto = fa })
	u, _ := e.newUser("carol")
	e.bind(afGuild, u)
	host := fa.host
	ctx := context.Background()
	if ok, err := host.Bound(ctx, afGuild); !ok || err != nil {
		t.Fatalf("bound: %v %v", ok, err)
	}
	if ok, _ := host.Bound(ctx, "444444444444444444"); ok {
		t.Fatal("an unbound server reads as bound")
	}
	if !host.Applicable(autoAtt()) {
		t.Fatal("a video on the CDN is not applicable")
	}
	for name, mut := range map[string]func(*autobot.Attachment){
		"other host":   func(a *autobot.Attachment) { a.URL = "https://evil.example/attachments/1/2/a.mp4" },
		"avatar path":  func(a *autobot.Attachment) { a.URL = "https://cdn.discordapp.com/avatars/1/2.png" },
		"an image":     func(a *autobot.Attachment) { a.Filename, a.ContentType = "a.png", "image/png" },
		"too large":    func(a *autobot.Attachment) { a.Size = 1<<20 + 1 },
		"no size":      func(a *autobot.Attachment) { a.Size = 0 },
		"control char": func(a *autobot.Attachment) { a.Filename = "a\x00.mp4" },
	} {
		if host.Applicable(autoAtt(mut)) {
			t.Errorf("%s: applicable", name)
		}
	}
	link, err := host.Upload(ctx, afGuild, autoAtt())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimPrefix(link, testOrigin+"/")
	if id == link || e.getClip(id).OwnerID != u.ID {
		t.Fatalf("link %q is not a clip of the bound account", link)
	}
	if stub.count() != 1 {
		t.Fatalf("the CDN was asked %d times", stub.count())
	}
	if _, err := host.Upload(ctx, "444444444444444444", autoAtt()); err == nil {
		t.Fatal("an unbound server uploaded")
	}
}

// Pin (§20 erratum 333 amended 2026-10-19; erratum 342): the instance side is
// autobot.Scoped — a person binding answers its member's linked Discord id, a
// server binding answers server-scoped, an unbound guild an error.
func TestAutoHostAnswersTheBindingScope(t *testing.T) {
	for _, c := range []struct {
		scope  string
		person bool
		id     string
	}{{db.GuildScopePerson, true, personLinked}, {db.GuildScopeServer, false, personLinked}} {
		fa := &fakeAuto{}
		b := newBotEnv(t, func(d *Deps) { d.Auto = fa })
		b.personBound(t, c.scope)
		s, ok := fa.host.(autobot.Scoped)
		if !ok {
			t.Fatal("the instance side is not autobot.Scoped")
		}
		person, id, err := s.Person(context.Background(), personGuild)
		if err != nil || person != c.person || id != c.id {
			t.Fatalf("%s: Person = %v %q %v, want %v %q", c.scope, person, id, err, c.person, c.id)
		}
		if _, _, err := s.Person(context.Background(), "333333333333333333"); err == nil {
			t.Fatalf("%s: an unbound guild answered no error", c.scope)
		}
	}
}
