package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/notify"
)

// The upload form (§20 erratum 320). Request fixtures are Discord's documented
// shapes (developers/components/reference: Text Display, Label, Text Input,
// Radio Group and their "Modal Submit Interaction Data Example";
// developers/interactions/receiving-and-responding: the MODAL callback and
// MODAL_SUBMIT) with this instance's custom_ids.

const (
	formGuild  = "888888888888888801"
	formGuild2 = "888888888888888802"
	formCDN    = "https://cdn.discordapp.com/attachments/1/2/clip.mp4"
)

var formIDShape = regexp.MustCompile(`^cb:form:[0-9a-f]{32}$`)

func newFormEnv(t *testing.T, mutate ...func(*Deps)) *botEnv {
	t.Helper()
	return newBotEnv(t, append([]func(*Deps){func(d *Deps) { d.Cfg.UploadForm = true }}, mutate...)...)
}

// openForm binds formGuild to a fresh account (once per name), runs `/upload`
// and returns the nonce, the job the endpoint handed over and the answer.
func (b *botEnv) openForm(t *testing.T, filename string) (string, notify.ClipJob, resp) {
	t.Helper()
	r := b.post(clipBody(formGuild, filename, "video/mp4", formCDN, 4096))
	if got, _ := r.json["type"].(float64); int(got) != callbackModal {
		t.Fatalf("/upload with the form on answered %s", r.body)
	}
	data, _ := r.json["data"].(map[string]any)
	cid, _ := data["custom_id"].(string)
	if !formIDShape.MatchString(cid) {
		t.Fatalf("the modal's custom_id is %q", cid)
	}
	job, ok := b.bot.last()
	if !ok {
		t.Fatal("no job was handed over behind the form")
	}
	return strings.TrimPrefix(cid, formPrefix), job, r
}

func (b *botEnv) bindFormGuild(t *testing.T, name string, guilds ...string) db.User {
	t.Helper()
	u, _ := b.newUser(name)
	for _, g := range append([]string{formGuild}, guilds...) {
		if _, err := b.db.BindGuild(context.Background(), g, u.ID, b.now()); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

// formSubmit is the form's MODAL_SUBMIT from an ordinary member (neither
// ADMINISTRATOR nor MANAGE_GUILD). ttl "" sends a null radio value.
func formSubmit(guild, nonce, title, ttl, tags string) string {
	var ttlValue any
	if ttl != "" {
		ttlValue = ttl
	}
	body, _ := json.Marshal(map[string]any{
		"type": interactionModalSubmit, "token": "int-token-submit", "guild_id": guild,
		"member": map[string]any{"permissions": permOrdinary, "user": map[string]any{"id": "424242424242424242"}},
		"data": map[string]any{
			"custom_id": formPrefix + nonce,
			"components": []any{
				map[string]any{"id": 1, "type": componentTextDisplay},
				map[string]any{"id": 2, "type": componentLabel, "component": map[string]any{"id": 3, "type": componentTextInput, "custom_id": customFormTitle, "value": title}},
				map[string]any{"id": 4, "type": componentLabel, "component": map[string]any{"id": 5, "type": componentRadioGroup, "custom_id": customFormTTL, "value": ttlValue}},
				map[string]any{"id": 6, "type": componentLabel, "component": map[string]any{"id": 7, "type": componentTextInput, "custom_id": customFormTags, "value": tags}},
			},
		},
	})
	return string(body)
}

// runIngestN makes every runIngest's bytes distinct: one account uploading the
// same bytes twice is the duplicate answer (§20 erratum 326), and these tests
// mean a new clip each time.
var runIngestN atomic.Int64

func runIngest(t *testing.T, job notify.ClipJob) string {
	t.Helper()
	body := fmt.Sprintf("fake video b%04d", runIngestN.Add(1))
	res, err := job.Ingest(context.Background(), notify.IngestSource{
		Body: strings.NewReader(body), Filename: job.Filename, ContentType: "video/mp4", Size: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return res.ClipID
}

func (b *botEnv) tags(t *testing.T, id string) []string {
	t.Helper()
	tags, err := b.db.TagsOf(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tags
}

func TestUploadFormOpensTheModalAndHandsOverAFollowupJob(t *testing.T) {
	secs := int64(30 * 86400)
	b := newFormEnv(t, func(d *Deps) { d.Cfg.DefaultTTLSeconds = &secs })
	b.bindFormGuild(t, "formowner")
	nonce, job, r := b.openForm(t, "clip.final.MP4")
	sameJSON(t, r.body, `{"type":9,"data":{
	  "custom_id":"cb:form:`+nonce+`","title":"Your clip is uploading",
	  "components":[
	    {"type":10,"content":"Cancel keeps the defaults."},
	    {"type":18,"label":"Title","component":{"type":4,"custom_id":"cb:form:title","style":1,"max_length":200,"required":false,"value":"clip.final"}},
	    {"type":18,"label":"Delete after","component":{"type":21,"custom_id":"cb:form:ttl","options":[
	      {"value":"default","label":"default · 30 days","default":true},
	      {"value":"1","label":"1 day"},{"value":"7","label":"7 days"},{"value":"30","label":"30 days"},
	      {"value":"90","label":"90 days"},{"value":"0","label":"never"}]}},
	    {"type":18,"label":"Tags","description":"comma separated; a-z, 0-9 and -","component":{"type":4,"custom_id":"cb:form:tags","style":1,"max_length":329,"required":false}}
	  ]}}`)
	if !job.Followup || !job.Uploading || job.Failed == nil || job.Ingest == nil || job.Token != "int-token-abc" || job.URL != formCDN {
		t.Fatalf("the job behind the form is %+v", job)
	}
	// Discord's limits (R&R / CR): title ≤ 45, ≤ 5 components, custom_ids ≤ 100,
	// a Label's description ≤ 100.
	if len(formModalTitle) > 45 || len(formPrefix+nonce) > 100 || len(formTagsDescription) > 100 {
		t.Fatal("the modal exceeds one of Discord's limits")
	}
}

// TestUploadFormDefaultLabelIsWhatThisUploadGets: the first option names the
// lifetime the upload gets if the form leaves it alone — the bound account's
// default when it has one, else the instance's (must 5).
func TestUploadFormDefaultLabelIsWhatThisUploadGets(t *testing.T) {
	b := newFormEnv(t) // DEFAULT_CLIP_TTL unset: never
	u := b.bindFormGuild(t, "labelowner")
	_, _, r := b.openForm(t, "a.mp4")
	if !strings.Contains(string(r.body), `"label":"default · never"`) {
		t.Fatalf("instance default not named: %s", r.body)
	}
	seven := 7
	if err := b.db.SetUserDefaults(context.Background(), u.ID, db.UserDefaults{TTLDays: &seven}); err != nil {
		t.Fatal(err)
	}
	_, _, r = b.openForm(t, "a.mp4")
	if !strings.Contains(string(r.body), `"label":"default · 7 days"`) {
		t.Fatalf("account default not named: %s", r.body)
	}
	one := 1
	if err := b.db.SetUserDefaults(context.Background(), u.ID, db.UserDefaults{TTLDays: &one}); err != nil {
		t.Fatal(err)
	}
	_, _, r = b.openForm(t, "a.mp4")
	if !strings.Contains(string(r.body), `"label":"default · 1 day"`) {
		t.Fatalf("account default of one day not named: %s", r.body)
	}
}

// TestUploadFormOmitsAPrefillDiscordCouldRefuse (must 7).
func TestUploadFormOmitsAPrefillDiscordCouldRefuse(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "prefill")
	titleInput := func(r resp) map[string]any {
		data := r.json["data"].(map[string]any)
		label := data["components"].([]any)[1].(map[string]any)
		return label["component"].(map[string]any)
	}
	// 150 emoji: 150 runes (inside maxTitleRunes), 300 UTF-16 units.
	_, _, r := b.openForm(t, strings.Repeat("😀", 150)+".mp4")
	if _, has := titleInput(r)["value"]; has {
		t.Fatalf("a title over 200 UTF-16 units was pre-filled: %s", r.body)
	}
	_, _, r = b.openForm(t, strings.Repeat("a", 200)+".mp4")
	if v, _ := titleInput(r)["value"].(string); v != strings.Repeat("a", 200) {
		t.Fatalf("a 200-character title was not pre-filled: %q", v)
	}
}

// TestUploadFormOffIsTodaysDeferral: `off` is byte-for-byte the deferral, and
// the job is the deferred kind.
func TestUploadFormOffIsTodaysDeferral(t *testing.T) {
	b := newBotEnv(t) // UploadForm false
	b.bindFormGuild(t, "offowner")
	r := b.post(clipBody(formGuild, "clip.mp4", "video/mp4", formCDN, 4096))
	if got := strings.TrimSpace(string(r.body)); got != `{"type":5}` {
		t.Fatalf("form off answered %q, want {\"type\":5}", got)
	}
	job, _ := b.bot.last()
	if job.Followup || job.Uploading || job.Failed != nil {
		t.Fatalf("form off handed over a form job: %+v", job)
	}
}

// TestUploadFormTableFullIsTodaysDeferral: 256 open forms, then the old path,
// byte for byte, and one log line.
func TestUploadFormTableFullIsTodaysDeferral(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "fullowner")
	// Each from its own address: the endpoint's per-IP `bot` bucket is not
	// what this test is about.
	for i := 0; i < formTableMax; i++ {
		r := b.post(clipBody(formGuild, "a.mp4", "video/mp4", formCDN, 4096), withRemote(fmt.Sprintf("10.0.%d.%d:4000", i/200, i%200)))
		if got, _ := r.json["type"].(float64); int(got) != callbackModal {
			t.Fatalf("form %d: %s", i, r.body)
		}
	}
	r := b.post(clipBody(formGuild, "clip.mp4", "video/mp4", formCDN, 4096), withRemote("10.9.9.9:4000"))
	if got := strings.TrimSpace(string(r.body)); got != `{"type":5}` {
		t.Fatalf("a full table answered %q, want {\"type\":5}", got)
	}
	job, _ := b.bot.last()
	if job.Followup || job.Uploading || job.Failed != nil {
		t.Fatalf("a full table handed over a form job: %+v", job)
	}
	if !strings.Contains(b.logBuf.String(), "discord bot: upload form table full; answered without the form") {
		t.Fatalf("no table-full line: %s", b.logBuf.String())
	}
}

func TestUploadFormSubmitBeforeTheClipIsAppliedAtAttach(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "beforeowner")
	nonce, job, _ := b.openForm(t, "clip.mp4")
	sub := formSubmit(formGuild, nonce, "  Match point  ", "7", "GG, clutch")
	assertEphemeral(t, b.post(sub), msgSaved)
	// One answer per form (must 2): the same submit again is expired.
	assertEphemeral(t, b.post(sub), msgFormExpired)
	id := runIngest(t, job)
	c := b.getClip(id)
	if c.Title != "Match point" {
		t.Fatalf("title = %q", c.Title)
	}
	if c.TTLSeconds == nil || *c.TTLSeconds != 7*86400 {
		t.Fatalf("ttl = %v", c.TTLSeconds)
	}
	if got := strings.Join(b.tags(t, id), ","); got != "clutch,gg" {
		t.Fatalf("tags = %q", got)
	}
}

func TestUploadFormSubmitAfterTheClipIsWrittenAtOnce(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "afterowner")
	nonce, job, _ := b.openForm(t, "clip.mp4")
	id := runIngest(t, job)
	if c := b.getClip(id); c.Title != "clip" || c.TTLSeconds != nil {
		t.Fatalf("before the submit: %+v", c)
	}
	sub := formSubmit(formGuild, nonce, "", "0", "")
	assertEphemeral(t, b.post(sub), msgSaved)
	c := b.getClip(id)
	if c.Title != "clip" {
		t.Fatalf("a blank title changed it to %q", c.Title)
	}
	if c.TTLSeconds != nil || c.ExpiresAt != nil {
		t.Fatalf("never was not applied: %v %v", c.TTLSeconds, c.ExpiresAt)
	}
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "Again", "1", "")), msgFormExpired)
	if b.getClip(id).Title != "clip" {
		t.Fatal("a second submit was applied")
	}
}

// TestUploadFormDefaultKeepsTheTTL: `default` leaves the lifetime the upload
// got.
func TestUploadFormDefaultKeepsTheTTL(t *testing.T) {
	secs := int64(30 * 86400)
	b := newFormEnv(t, func(d *Deps) { d.Cfg.DefaultTTLSeconds = &secs })
	b.bindFormGuild(t, "defaultowner")
	nonce, job, _ := b.openForm(t, "clip.mp4")
	id := runIngest(t, job)
	before := b.getClip(id)
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "T", "default", "")), msgSaved)
	after := b.getClip(id)
	if after.TTLSeconds == nil || *after.TTLSeconds != secs || *after.ExpiresAt != *before.ExpiresAt {
		t.Fatalf("default changed the lifetime: %v → %v", before.ExpiresAt, after.ExpiresAt)
	}
}

func TestUploadFormFailedUploadAnswersOnceThenExpired(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "failowner")
	nonce, job, _ := b.openForm(t, "clip.mp4")
	job.Failed()
	sub := formSubmit(formGuild, nonce, "x", "7", "")
	assertEphemeral(t, b.post(sub), notify.SentenceFailed)
	assertEphemeral(t, b.post(sub), msgFormExpired)
	// …and a submit HELD before the failure is dropped with it.
	nonce, job, _ = b.openForm(t, "clip.mp4")
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "held after all", "7", "")), msgSaved)
	job.Failed()
	// A later attach finds nothing to write: were the held submit kept, the
	// ingest's attach would apply it to a clip the person was told failed.
	if id := runIngest(t, job); b.getClip(id).Title == "held after all" {
		t.Fatal("a submit held before the failure was applied by a later attach")
	}
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "x", "7", "")), msgFormExpired)
}

// TestUploadFormBadTagsKeepTheRest (must 6): title and delete-after are
// applied, the tags are left as they were, and nothing typed is echoed.
func TestUploadFormBadTagsKeepTheRest(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "tagowner")
	nonce, job, _ := b.openForm(t, "clip.mp4")
	id := runIngest(t, job)
	if err := b.db.SetClipTags(context.Background(), id, []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	r := b.post(formSubmit(formGuild, nonce, "Good title", "90", "fine, Bad Tag!"))
	assertEphemeral(t, r, msgFormTagsGrammar)
	if strings.Contains(string(r.body), "Bad Tag") {
		t.Fatalf("the answer echoed the tag: %s", r.body)
	}
	c := b.getClip(id)
	if c.Title != "Good title" || c.TTLSeconds == nil || *c.TTLSeconds != 90*86400 {
		t.Fatalf("the rest was not applied: %q %v", c.Title, c.TTLSeconds)
	}
	if got := strings.Join(b.tags(t, id), ","); got != "keep" {
		t.Fatalf("tags = %q, want them untouched", got)
	}

	nonce, job, _ = b.openForm(t, "clip.mp4")
	id = runIngest(t, job)
	r = b.post(formSubmit(formGuild, nonce, "Eleven", "", "a,b,c,d,e,f,g,h,i,j,k"))
	// ttl "" is a null radio: that is a refusal before the tags are judged.
	assertEphemeral(t, r, msgNotAChoice)
	r = b.post(formSubmit(formGuild, nonce, "Eleven", "default", "a,b,c,d,e,f,g,h,i,j,k"))
	assertEphemeral(t, r, msgFormTagsTooMany)
	if c := b.getClip(id); c.Title != "Eleven" {
		t.Fatalf("title = %q", c.Title)
	}
	if len(b.tags(t, id)) != 0 {
		t.Fatal("eleven tags were written")
	}
}

// TestUploadFormRefusalsApplyNothingAndKeepTheForm: a forged title or a
// delete-after outside the six values applies nothing and does not consume the
// form; the person's real answer still lands.
func TestUploadFormRefusalsApplyNothingAndKeepTheForm(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "refuseowner")
	nonce, job, _ := b.openForm(t, "clip.mp4")
	id := runIngest(t, job)
	cases := []struct{ title, ttl, want string }{
		{strings.Repeat("a", 201), "7", msgFormTitleRefused},
		{"bell\x07", "7", msgFormTitleRefused},
		{"ok", "3", msgNotAChoice},
		{"ok", "default ", msgNotAChoice},
		{"ok", "", msgNotAChoice},
	}
	for _, c := range cases {
		r := b.post(formSubmit(formGuild, nonce, c.title, c.ttl, "gg"))
		assertEphemeral(t, r, c.want)
		if strings.Contains(string(r.body), "bell") {
			t.Fatalf("the refusal echoed the title: %s", r.body)
		}
	}
	if c := b.getClip(id); c.Title != "clip" || len(b.tags(t, id)) != 0 {
		t.Fatalf("a refused submit wrote something: %+v", c)
	}
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "Real", "7", "gg")), msgSaved)
	if c := b.getClip(id); c.Title != "Real" {
		t.Fatalf("title = %q", c.Title)
	}
}

func TestUploadFormUnknownMalformedAndForeignAreOneSentence(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "unknownowner", formGuild2)
	nonce, _, _ := b.openForm(t, "clip.mp4")
	for _, n := range []string{
		strings.Repeat("0", 32), // unknown
		strings.ToUpper(nonce),  // malformed (upper case)
		nonce[:31],              // malformed (short)
		nonce + "0",             // malformed (long)
		"",                      // empty
		"../" + nonce[3:],       // not hex
	} {
		assertEphemeral(t, b.post(formSubmit(formGuild, n, "x", "7", "")), msgFormExpired)
	}
	// Another server, even one bound to the same account.
	assertEphemeral(t, b.post(formSubmit(formGuild2, nonce, "x", "7", "")), msgFormExpired)
	// …and none of that consumed the form.
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "x", "7", "")), msgSaved)
	// An unbound server and a DM keep their own lines.
	assertEphemeral(t, b.post(formSubmit("999999999999999999", nonce, "x", "7", "")), msgUnbound)
	assertEphemeral(t, b.post(formSubmit("", nonce, "x", "7", "")), msgNotInGuild("upload"))
}

// TestUploadFormNeedsNoManageServerAndTheRestStillDoes.
func TestUploadFormNeedsNoManageServerAndTheRestStillDoes(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "gateowner")
	nonce, _, _ := b.openForm(t, "clip.mp4")
	// formSubmit is an ordinary member's: served.
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "x", "7", "")), msgSaved)
	// `cb:defaults` from the same member: still gated.
	assertEphemeral(t, b.post(clickBody(formGuild, permOrdinary, customDefaults)), msgNeedManage)
	// A CLICK on a `cb:form:` id is not the form's route: it falls to the gate.
	assertEphemeral(t, b.post(clickBody(formGuild, permOrdinary, formPrefix+nonce)), msgNeedManage)
}

func TestUploadFormClipGoneMeanwhile(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "goneowner")
	nonce, job, _ := b.openForm(t, "clip.mp4")
	id := runIngest(t, job)
	if err := b.db.MarkDeleting(context.Background(), id, b.now()); err != nil {
		t.Fatal(err)
	}
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "x", "7", "")), msgLinkGone)
}

// TestUploadFormLogsNoValueAndNoNonce (must 8).
func TestUploadFormLogsNoValueAndNoNonce(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "logowner")
	nonce, job, _ := b.openForm(t, "clip.mp4")
	_, access := b.postLogged(formSubmit(formGuild, nonce, "SecretTitleText", "7", "secrettag"))
	if !strings.Contains(access, "discord_prefix=cb:form:") || !strings.Contains(access, "form=saved") {
		t.Fatalf("the access log lacks the prefix or the outcome: %s", access)
	}
	runIngest(t, job)
	_, access2 := b.postLogged(formSubmit(formGuild, nonce, "SecretTitleText", "7", "Bad Tag"))
	if !strings.Contains(access2, "form=expired") {
		t.Fatalf("the second submit was not logged expired: %s", access2)
	}
	for _, leak := range []string{nonce, "SecretTitleText", "secrettag"} {
		if strings.Contains(access+access2+b.logBuf.String(), leak) {
			t.Fatalf("%q reached a log", leak)
		}
	}
}

// --- the table ---------------------------------------------------------------

func TestFormTableExpiresAtFifteenMinutes(t *testing.T) {
	tb := newFormTable()
	t0 := time.Unix(1_800_000_000, 0)
	n, ok := tb.open("g", t0)
	if !ok {
		t.Fatal("open failed")
	}
	if o, _ := tb.take(n, "g", t0.Add(formTTL), true, db.ClipPatch{}); o != formExpired {
		t.Fatalf("at 15 minutes: %v", o)
	}
	if tb.len() != 0 {
		t.Fatal("an expired entry stayed")
	}
	n, _ = tb.open("g", t0)
	if o, _ := tb.take(n, "g", t0.Add(formTTL-time.Second), true, db.ClipPatch{}); o != formHeld {
		t.Fatalf("inside 15 minutes: %v", o)
	}
	// Opening sweeps what has expired.
	tb.open("g", t0.Add(2*formTTL))
	if tb.len() != 1 {
		t.Fatalf("the sweep left %d entries", tb.len())
	}
}

// TestFormTableFailNeverFlipsADuplicate: a duplicate's entry has a clip (the
// account's existing one), so a later fail — a panic in the job's tail after a
// link POST that never answered — leaves it a duplicate, and the submit is told
// "not applied", never "could not be uploaded".
func TestFormTableFailNeverFlipsADuplicate(t *testing.T) {
	tb := newFormTable()
	t0 := time.Unix(1_800_000_000, 0)
	n, ok := tb.open("g", t0)
	if !ok {
		t.Fatal("open failed")
	}
	if tb.existing(n) {
		t.Fatal("existing reported a dropped submit with none held")
	}
	tb.fail(n)
	if o, _ := tb.take(n, "g", t0.Add(time.Second), true, db.ClipPatch{}); o != formNotApplied {
		t.Fatalf("a duplicate's submit after fail: %v, want formNotApplied", o)
	}
	// The plain entry still fails.
	n, _ = tb.open("g", t0)
	tb.fail(n)
	if o, _ := tb.take(n, "g", t0.Add(time.Second), true, db.ClipPatch{}); o != formFailed {
		t.Fatalf("a failed upload's submit: %v, want formFailed", o)
	}
}

// TestFormTableAttachAndSubmitRaceApplyOnce: whichever wins, the patch is
// written exactly once — by the submit (formWrite) or by attach.
func TestFormTableAttachAndSubmitRaceApplyOnce(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	title := "t"
	for i := 0; i < 500; i++ {
		tb := newFormTable()
		n, _ := tb.open("g", now)
		var wg sync.WaitGroup
		var held *db.ClipPatch
		var outcome formOutcome
		wg.Add(2)
		go func() { defer wg.Done(); held = tb.attach(n, "clip1") }()
		go func() { defer wg.Done(); outcome, _ = tb.take(n, "g", now, true, db.ClipPatch{Title: &title}) }()
		wg.Wait()
		writes := 0
		if held != nil {
			writes++
		}
		if outcome == formWrite {
			writes++
		}
		if writes != 1 {
			t.Fatalf("round %d: %d writes (held=%v outcome=%v)", i, writes, held != nil, outcome)
		}
		if tb.len() != 0 {
			t.Fatalf("round %d: the entry survived both", i)
		}
	}
}

// --- the whole path through a real notify.Bot ---------------------------------

// formDiscord is an httptest stand-in for Discord's API and CDN at once. Every
// POST gets its own message id (recorded at the end of its line as ` -> id`),
// and `posted` closes on the first PUBLIC one — the link or a failure sentence,
// not the ephemeral "Uploading…" (§20 erratum 325).
type formDiscord struct {
	srv       *httptest.Server
	cdnStatus int
	mu        sync.Mutex
	posts     []string
	nextID    int
	posted    chan struct{}
	once      sync.Once
	// changed is closed and replaced on every recorded call (guarded by mu).
	changed chan struct{}
}

func newFormDiscord(t *testing.T, cdnStatus int) *formDiscord {
	f := &formDiscord{cdnStatus: cdnStatus, posted: make(chan struct{}), changed: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/attachments/") {
			w.WriteHeader(f.cdnStatus)
			_, _ = io.WriteString(w, "fake video bytes")
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.nextID++
		id := fmt.Sprintf("15%016d", f.nextID)
		f.posts = append(f.posts, r.Method+" "+r.URL.Path+" "+string(body)+" -> "+id)
		close(f.changed)
		f.changed = make(chan struct{})
		f.mu.Unlock()
		if r.Method == http.MethodPost && !strings.Contains(string(body), `"flags":64`) {
			f.once.Do(func() { close(f.posted) })
		}
		_, _ = io.WriteString(w, `{"id":"`+id+`"}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// waitFor re-reads the recorded calls at every new call until ok says yes.
func (f *formDiscord) waitFor(t *testing.T, what string, ok func(posts string) bool) string {
	t.Helper()
	guard := time.NewTimer(time.Minute) // a hang guard, never a budget
	defer guard.Stop()
	for {
		f.mu.Lock()
		posts, next := strings.Join(f.posts, "\n"), f.changed
		f.mu.Unlock()
		if ok(posts) {
			return posts
		}
		select {
		case <-next:
		case <-guard.C:
			t.Fatalf("never saw %s; calls:\n%s", what, posts)
		}
	}
}

func (f *formDiscord) wait(t *testing.T) string {
	t.Helper()
	select {
	case <-f.posted:
	case <-time.After(5 * time.Second):
		t.Fatal("no follow-up was posted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.posts, "\n")
}

func newFormEnvWithRealBot(t *testing.T, f *formDiscord) *botEnv {
	b, _ := newFormEnvAndBot(t, f)
	return b
}

// newFormEnvAndBot is newFormEnvWithRealBot, handing back the bot too: the
// worker's card-ready word (§20 erratum 330) is wired in serve.go, so a test
// here says it by hand.
func newFormEnvAndBot(t *testing.T, f *formDiscord) (*botEnv, *notify.Bot) {
	host := strings.TrimPrefix(f.srv.URL, "http://")
	var bot *notify.Bot
	b := newFormEnv(t, func(d *Deps) {
		bot = notify.NewBot(notify.BotDeps{
			AppID: "123456789012345678", Token: "MTIzNDU2Nzg5MDEyMzQ1Njc4.GhIjKl.mNoPqRsTuVwXyZ0123456789abcdef",
			URLs: d.URLs, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			APIHost: host, APIScheme: "http",
			// The CDN is the same test server; the allowlist rewrites the
			// attachment's real host to it, as a test may (BotDeps' reason).
			AllowAttachment: func(raw string) (*url.URL, error) {
				u, err := url.Parse(raw)
				if err != nil {
					return nil, err
				}
				u.Scheme, u.Host = "http", host
				return u, nil
			},
			MaxBytes: 1 << 20, CallTimeout: 2 * time.Second, FetchTimeout: 2 * time.Second, JobBudget: 5 * time.Second,
		})
		d.Bot = bot
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = bot.Shutdown(ctx)
	})
	return b, bot
}

// TestUploadFormCDNFailureWithoutIngestAnswersFailed (must 3): a CDN non-200
// returns before the ingest is ever called; the table still learns it.
func TestUploadFormCDNFailureWithoutIngestAnswersFailed(t *testing.T) {
	f := newFormDiscord(t, http.StatusForbidden)
	b := newFormEnvWithRealBot(t, f)
	u := b.bindFormGuild(t, "cdnowner")
	r := b.post(clipBody(formGuild, "clip.mp4", "video/mp4", formCDN, 16))
	cid, _ := r.json["data"].(map[string]any)["custom_id"].(string)
	nonce := strings.TrimPrefix(cid, formPrefix)
	posts := f.wait(t)
	if !strings.Contains(posts, "POST /api/v10/webhooks/123456789012345678/int-token-abc ") ||
		!strings.Contains(posts, notify.SentenceFetch) || strings.Contains(posts, "PATCH") {
		t.Fatalf("the failure was not posted as a follow-up: %s", posts)
	}
	sub := formSubmit(formGuild, nonce, "x", "7", "")
	assertEphemeral(t, b.post(sub), notify.SentenceFailed)
	assertEphemeral(t, b.post(sub), msgFormExpired)
	clips, _, err := b.db.ListClips(context.Background(), db.ListQuery{ViewerID: u.ID, Mine: true, Limit: 10})
	if err != nil || len(clips) != 0 {
		t.Fatalf("a clip exists for a failed fetch: %v %v", clips, err)
	}
}

// TestUploadFormWholePath: form answered, submit held, the link posted as a
// follow-up, and the clip carries what the form said.
func TestUploadFormWholePath(t *testing.T) {
	f := newFormDiscord(t, http.StatusOK)
	b := newFormEnvWithRealBot(t, f)
	b.bindFormGuild(t, "pathowner")
	r := b.post(clipBody(formGuild, "clip.mp4", "video/mp4", formCDN, 16))
	cid, _ := r.json["data"].(map[string]any)["custom_id"].(string)
	nonce := strings.TrimPrefix(cid, formPrefix)
	// The submit races the job; either order must land.
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "From the form", "30", "gg")), msgSaved)
	posts := f.wait(t)
	m := regexp.MustCompile(`"content":"` + regexp.QuoteMeta(testOrigin) + `/([A-Za-z0-9]+)"`).FindStringSubmatch(posts)
	if m == nil {
		t.Fatalf("no link was posted: %s", posts)
	}
	c := b.getClip(m[1])
	if c.Title != "From the form" || c.TTLSeconds == nil || *c.TTLSeconds != 30*86400 {
		t.Fatalf("the posted clip is %q %v", c.Title, c.TTLSeconds)
	}
	if got := strings.Join(b.tags(t, m[1]), ","); got != "gg" {
		t.Fatalf("tags = %q", got)
	}
}

// --- §20 erratum 325: "Uploading…" and the redraw ----------------------------

// TestUploadFormRedrawOnlyForALateChangedTitle: the form asks the bot to
// redraw exactly when its submit lands AFTER the clip exists and CHANGES the
// title — never for the same title, a blank one, a refused submit or a submit
// held for the clip (that card is drawn right the first time).
func TestUploadFormRedrawOnlyForALateChangedTitle(t *testing.T) {
	b := newFormEnv(t)
	b.bindFormGuild(t, "redrawowner")

	nonce, job, _ := b.openForm(t, "clip.mp4")
	id := runIngest(t, job)
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "A new title", "default", "")), msgSaved)
	if got := b.bot.redrawn(); len(got) != 1 || got[0] != id {
		t.Fatalf("a late changed title asked for %v, want [%s]", got, id)
	}

	// The pre-filled title sent back unchanged.
	nonce, job, _ = b.openForm(t, "clip.mp4")
	runIngest(t, job)
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "clip", "7", "gg")), msgSaved)
	// A blank title (unchanged) with the other two fields.
	nonce, job, _ = b.openForm(t, "clip.mp4")
	runIngest(t, job)
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "", "30", "gg")), msgSaved)
	// A refused submit writes nothing.
	nonce, job, _ = b.openForm(t, "clip.mp4")
	runIngest(t, job)
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, strings.Repeat("a", 201), "7", "")), msgFormTitleRefused)
	// A submit BEFORE the clip: held, and written by attach.
	nonce, job, _ = b.openForm(t, "clip.mp4")
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "Early title", "default", "")), msgSaved)
	early := runIngest(t, job)
	if c := b.getClip(early); c.Title != "Early title" {
		t.Fatalf("the held title = %q", c.Title)
	}
	if got := b.bot.redrawn(); len(got) != 1 {
		t.Fatalf("redraws asked for %v; only the first submit changed a posted title", got)
	}
}

// TestUploadFormLateTitleRedrawsTheLink is the whole path through a real
// notify.Bot: the private "Uploading…" posted and deleted, the link posted
// bare, then a late submit that changes the title edits THAT message to the
// same link plus `?v=2` — content only, no embed, no component, no mention.
func TestUploadFormLateTitleRedrawsTheLink(t *testing.T) {
	f := newFormDiscord(t, http.StatusOK)
	b := newFormEnvWithRealBot(t, f)
	b.bindFormGuild(t, "lateowner")
	r := b.post(clipBody(formGuild, "clip.mp4", "video/mp4", formCDN, 16))
	cid, _ := r.json["data"].(map[string]any)["custom_id"].(string)
	nonce := strings.TrimPrefix(cid, formPrefix)

	const hook = "/api/v10/webhooks/123456789012345678/int-token-abc"
	linkLine := regexp.MustCompile(`(?m)^POST ` + regexp.QuoteMeta(hook) + ` \{"content":"` +
		regexp.QuoteMeta(testOrigin) + `/([A-Za-z0-9]+)","allowed_mentions":\{"parse":\[\]\}\} -> ([0-9]+)$`)
	posts := f.waitFor(t, "the link", linkLine.MatchString)
	m := linkLine.FindStringSubmatch(posts)
	clipID, linkMsg := m[1], m[2]

	// "Uploading…": ephemeral, posted once, deleted by its own id.
	upLine := regexp.MustCompile(`(?m)^POST ` + regexp.QuoteMeta(hook) +
		` \{"content":"` + regexp.QuoteMeta(notify.SentenceUploading) + `","flags":64,"allowed_mentions":\{"parse":\[\]\}\} -> ([0-9]+)$`)
	posts = f.waitFor(t, "the Uploading line deleted", func(s string) bool {
		u := upLine.FindStringSubmatch(s)
		return u != nil && strings.Contains(s, "DELETE "+hook+"/messages/"+u[1]+" ")
	})
	if n := len(upLine.FindAllString(posts, -1)); n != 1 {
		t.Fatalf("%d Uploading lines, want 1:\n%s", n, posts)
	}

	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "Late title", "default", "")), msgSaved)
	want := "PATCH " + hook + "/messages/" + linkMsg + ` {"content":"` + testOrigin + "/" + clipID +
		`?v=2","allowed_mentions":{"parse":[]}} -> `
	posts = f.waitFor(t, "the redraw", func(s string) bool { return strings.Contains(s, want) })
	if n := strings.Count(posts, "PATCH "); n != 1 {
		t.Fatalf("%d PATCHes, want the one redraw:\n%s", n, posts)
	}
	if c := b.getClip(clipID); c.Title != "Late title" {
		t.Fatalf("title = %q", c.Title)
	}
}

// TestUploadFormLateCardThenLateTitle (§20 erratum 330) is the whole path
// through a real notify.Bot: `/upload` posts the link before its card is ready
// (this env's clip is still queued), the worker's word that the card is ready
// edits THAT message to `?v=2`, and a late title then edits it to `?v=3` — each
// a URL Discord has not read. Two PATCHes, no more.
func TestUploadFormLateCardThenLateTitle(t *testing.T) {
	f := newFormDiscord(t, http.StatusOK)
	b, bot := newFormEnvAndBot(t, f)
	b.bindFormGuild(t, "latecardowner")
	r := b.post(clipBody(formGuild, "clip.mp4", "video/mp4", formCDN, 16))
	cid, _ := r.json["data"].(map[string]any)["custom_id"].(string)
	nonce := strings.TrimPrefix(cid, formPrefix)

	const hook = "/api/v10/webhooks/123456789012345678/int-token-abc"
	linkLine := regexp.MustCompile(`(?m)^POST ` + regexp.QuoteMeta(hook) + ` \{"content":"` +
		regexp.QuoteMeta(testOrigin) + `/([A-Za-z0-9]+)","allowed_mentions":\{"parse":\[\]\}\} -> ([0-9]+)$`)
	posts := f.waitFor(t, "the link", linkLine.MatchString)
	m := linkLine.FindStringSubmatch(posts)
	clipID, linkMsg := m[1], m[2]
	if db.CardReady(b.getClip(clipID)) {
		t.Fatal("premise: the link went out before the card was ready")
	}
	redraw := func(v string) string {
		return "PATCH " + hook + "/messages/" + linkMsg + ` {"content":"` + testOrigin + "/" + clipID +
			`?v=` + v + `","allowed_mentions":{"parse":[]}} -> `
	}
	bot.CardReady(clipID)
	f.waitFor(t, "the card's redraw", func(s string) bool { return strings.Contains(s, redraw("2")) })
	assertEphemeral(t, b.post(formSubmit(formGuild, nonce, "Late title", "default", "")), msgSaved)
	f.waitFor(t, "the title's redraw", func(s string) bool { return strings.Contains(s, redraw("3")) })
	bot.CardReady(clipID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = bot.Shutdown(ctx)
	f.mu.Lock()
	posts = strings.Join(f.posts, "\n")
	f.mu.Unlock()
	if n := strings.Count(posts, "PATCH "); n != 2 {
		t.Fatalf("%d PATCHes, want the card's and the title's:\n%s", n, posts)
	}
}
