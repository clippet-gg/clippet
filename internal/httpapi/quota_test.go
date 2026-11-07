package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"clippet/internal/notify"
)

// The opt-in hard cap (STORAGE_BUDGET_HARD=1; ROADMAP #40, §20 erratum 277).
//
// Four claims are pinned here and nothing else in the product pins any of them:
// the cap is OFF unless it is armed, it refuses AT the line and not one byte
// under, it refuses a NEW clip and nothing else (a replacement is not an
// upload), and it says the same sentence on every surface.

// quotaEnv is an env with the cap armed at `limit` bytes.
func quotaEnv(t *testing.T, limit int64) *env {
	t.Helper()
	return newEnv(t, func(d *Deps) {
		d.Cfg.StorageBudgetBytes = limit
		d.Cfg.StorageBudgetHard = true
	})
}

// TestHardQuotaRefusesAtTheLineAndNotOneByteUnder is the boundary, asserted
// from both sides with one byte between them.
//
// A budget of N bytes is N bytes of ROOM: an instance holding N−1 has one byte
// left and takes the upload; one holding N has none and refuses. (The advisory
// WARN keeps erratum 157's `used > budget` edge, which fires one byte later —
// each is right about its own question, and DESIGN §20 erratum 277 says so.)
func TestHardQuotaRefusesAtTheLineAndNotOneByteUnder(t *testing.T) {
	video := testVideo(4_000)
	// The stored total after one upload is the source size and nothing else
	// (no worker runs in this package), so the line can be drawn to the byte.
	const size = 4_000

	// ONE BYTE UNDER. The line is size+1, so after one upload the instance
	// holds size — one byte of room — and the next upload is taken. The cap
	// asks "is there room", not "does THIS file fit": what arrives is a
	// stream whose length nobody knows until it has arrived, and a cap that
	// pretended otherwise would refuse on a guess.
	under := quotaEnv(t, size+1)
	_, cookie := under.newUser("carol")
	if r := under.upload(`{"title":"one"}`, "a.mp4", video, withCookie(cookie)); r.code != 201 {
		t.Fatalf("the first upload into an empty capped instance: %d %s", r.code, r.body)
	}
	if used, _ := under.db.StorageUsed(context.Background()); used != size {
		t.Fatalf("the instance holds %d bytes, want one byte under the %d line", used, size+1)
	}
	if r := under.upload(`{"title":"two"}`, "b.mp4", video, withCookie(cookie)); r.code != 201 {
		t.Fatalf("an upload with one byte of room left: %d %s", r.code, r.body)
	}
	// …and now it is well past the line, so the third is refused.
	if r := under.upload(`{"title":"three"}`, "c.mp4", video, withCookie(cookie)); r.code != 507 {
		t.Fatalf("an upload past the line: %d %s", r.code, r.body)
	}

	// EXACTLY AT THE LINE. The budget is size, the first upload fills it
	// exactly, and the second has no room at all.
	at := quotaEnv(t, size)
	_, cookie2 := at.newUser("carol")
	if r := at.upload(`{"title":"one"}`, "a.mp4", video, withCookie(cookie2)); r.code != 201 {
		t.Fatalf("the first upload into an empty capped instance: %d %s", r.code, r.body)
	}
	r := at.upload(`{"title":"two"}`, "b.mp4", video, withCookie(cookie2))
	if r.code != 507 {
		t.Fatalf("an upload at exactly the line: %d %s", r.code, r.body)
	}
	if got := r.errCode(); got != "storage_quota" {
		t.Errorf("the refusal's error code = %q, want storage_quota (%s)", got, r.body)
	}
	if got := r.str("message"); got != quotaRefusal {
		t.Errorf("the refusal's sentence = %q, want %q", got, quotaRefusal)
	}
	// It refuses BEFORE anything is written: no object in the bucket and no
	// row, so the stored total is exactly the accepted clip's.
	if n := len(at.fake.Keys()); n != 1 {
		t.Errorf("the refused upload left %d objects in the bucket, want only the accepted clip's 1", n)
	}
	if used, err := at.db.StorageUsed(context.Background()); err != nil || used != size {
		t.Errorf("the refused upload changed what is stored: %d bytes (err %v), want %d", used, err, size)
	}
}

// TestNoHardQuotaWithoutTheFlag: the budget alone refuses nothing, which is the
// promise every version before erratum 277 made and the default still makes.
func TestNoHardQuotaWithoutTheFlag(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.StorageBudgetBytes = 1 }) // one byte: everything is over it
	_, cookie := e.newUser("carol")
	for i, title := range []string{"one", "two", "three"} {
		if r := e.upload(`{"title":"`+title+`"}`, "a.mp4", testVideo(4_000), withCookie(cookie)); r.code != 201 {
			t.Fatalf("upload %d over an ADVISORY budget: %d %s", i, r.code, r.body)
		}
	}
}

// TestHardQuotaDoesNotRefuseAReplacement: a replacement does not add a clip —
// it changes the file behind a link people already have, and usually makes the
// instance smaller. Refusing it would strand the one action that could get an
// operator back under their own line (§20 erratum 277).
func TestHardQuotaDoesNotRefuseAReplacement(t *testing.T) {
	e := quotaEnv(t, 1) // one byte: this instance is past its line from the start
	u, cookie := e.newUser("carol")
	clip, _, _ := e.readyClip(u, clipOpts{})
	// A new clip is refused, which is what makes the rest of this test mean
	// something.
	if r := e.upload(`{"title":"new"}`, "a.mp4", testVideo(4_000), withCookie(cookie)); r.code != 507 {
		t.Fatalf("the cap is not armed: %d %s", r.code, r.body)
	}
	// The replacement is not: it adds no clip, it is the file behind a link
	// people already have, and it is usually how an operator gets back UNDER
	// their own line.
	if rep := e.replaceReq(clip.ID, "video", "b.mp4", testVideo(3_000), withCookie(cookie)); rep.code != 202 {
		t.Fatalf("a replacement while past the quota: %d %s", rep.code, rep.body)
	}
	// Nor is a trim: same clip, same link, no new row.
	if tr := e.doJSON(http.MethodPost, "/api/clips/"+clip.ID+"/trim",
		map[string]any{"start_ms": 0, "end_ms": 2000}, withCookie(cookie)); tr.code != 202 {
		t.Fatalf("a trim while past the quota: %d %s", tr.code, tr.body)
	}
}

// TestHardQuotaRefusalIsLogged: an operator reading the app log must be able to
// tell "somebody's upload was refused" from "somebody's upload failed", with
// the two numbers that explain it.
func TestHardQuotaRefusalIsLogged(t *testing.T) {
	e := quotaEnv(t, 4_000)
	_, cookie := e.newUser("carol")
	e.upload(`{"title":"one"}`, "a.mp4", testVideo(4_000), withCookie(cookie))
	e.logBuf.Reset()
	if r := e.upload(`{"title":"two"}`, "b.mp4", testVideo(4_000), withCookie(cookie)); r.code != 507 {
		t.Fatalf("the refused upload: %d %s", r.code, r.body)
	}
	log := e.logBuf.String()
	for _, want := range []string{"upload refused: the instance is at its storage quota", "used_bytes=4000", "budget_bytes=4000"} {
		if !strings.Contains(log, want) {
			t.Errorf("the refusal log lacks %q:\n%s", want, log)
		}
	}
}

// TestHardQuotaSentenceIsTheSameEverywhere: the bot's `/clip` says exactly what
// the 507 says. Two people hitting the same line two ways compare notes and
// hear one thing (§20 erratum 277).
func TestHardQuotaSentenceIsTheSameEverywhere(t *testing.T) {
	if notify.SentenceQuota != quotaRefusal {
		t.Errorf("the bot says %q and the HTTP refusal says %q", notify.SentenceQuota, quotaRefusal)
	}
	b := newBotEnv(t, func(d *Deps) {
		d.Cfg.StorageBudgetBytes = 5
		d.Cfg.StorageBudgetHard = true
	})
	_, ingest := b.boundIngest(t, "quotaowner", "555555555555555555")
	// The first `/clip` fills the five-byte line…
	if _, err := ingest(context.Background(), notify.IngestSource{
		Body: strings.NewReader("first"), Filename: "a.mp4", ContentType: "video/mp4", Size: 5,
	}); err != nil {
		t.Fatalf("the first /clip into an empty capped instance: %v", err)
	}
	// …and the second is refused with the sentence, through the same ingest
	// error path every other bot refusal uses.
	_, err := ingest(context.Background(), notify.IngestSource{
		Body: strings.NewReader("second"), Filename: "b.mp4", ContentType: "video/mp4", Size: 6,
	})
	var ie *notify.IngestError
	if !errors.As(err, &ie) || ie.Sentence != notify.SentenceQuota {
		t.Fatalf("the second /clip = %v, want the quota sentence", err)
	}
}
