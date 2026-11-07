package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/notify"
	"clippet/internal/urls"
	"clippet/internal/web"
)

// `/settings` on the instance (§20 erratum 318; run 4's panel contract §4/§5).
// The request fixtures below are Discord's documented shapes, read through the
// docs MCP (developers/components/reference: Label, Radio Group, Checkbox,
// String Select, each with its "Modal Submit Interaction Data Example";
// developers/interactions/receiving-and-responding: MESSAGE_COMPONENT,
// MODAL_SUBMIT, the MODAL callback) with this instance's custom_ids — and the
// RESPONSES are pinned byte-for-byte (as JSON) against goldens, so a drift in
// either direction fails here rather than in a client.

const (
	settingsGuild   = "777777777777777701"
	permManage      = "32"         // MANAGE_GUILD alone
	permAdmin       = "8"          // ADMINISTRATOR alone
	permOrdinary    = "2147601473" // invite, reactions, view, send, embed, attach, history, use commands: neither bit 3 nor bit 5
	permBothAndMore = "1099511627775"
)

// settingsCommandBody is `/settings` as Discord sends it: a CHAT_INPUT command
// with no options, from a member whose permission bitfield is `perms` ("" omits
// the member object, as a DM would).
func settingsCommandBody(guild, name, perms string) string {
	v := map[string]any{
		"type": interactionCommand, "token": "int-token-settings", "guild_id": guild,
		"data": map[string]any{"name": name, "type": 1},
	}
	if perms != "" {
		v["member"] = map[string]any{"permissions": perms, "user": map[string]any{"id": "1", "username": "someone"}}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// componentClickFixture is a MESSAGE_COMPONENT interaction: a Button (component
// type 2) clicked on the panel — the documented "Message Interaction Response
// Example" plus the fields the contract's §5 names.
const componentClickFixture = `{
  "type": 3,
  "token": "int-token-click",
  "application_id": "123456789012345678",
  "guild_id": "%GUILD%",
  "member": {"permissions": "%PERMS%", "user": {"id": "1", "username": "someone"}},
  "authorizing_integration_owners": {"0": "%GUILD%"},
  "message": {"id": "1", "flags": 32832, "components": []},
  "data": {"component_type": 2, "id": 2, "custom_id": "%CID%"}
}`

// modalSubmitFixture is a MODAL_SUBMIT carrying the contract's shape: three
// Labels (type 18), each with its one `component` — a Radio Group's string
// `value`, a Checkbox's boolean `value`, a Radio Group again — exactly as the
// three "Modal Submit Interaction Data Example"s of the reference spell them.
const modalSubmitFixture = `{
  "type": 5,
  "token": "int-token-modal",
  "application_id": "123456789012345678",
  "guild_id": "%GUILD%",
  "member": {"permissions": "%PERMS%", "user": {"id": "1", "username": "someone"}},
  "data": {
    "custom_id": "cb:defaults:modal",
    "components": [
      {"id": 1, "type": 18, "component": {"id": 2, "type": 21, "custom_id": "cb:defaults:ttl", "value": %TTL%}},
      {"id": 3, "type": 18, "component": {"id": 4, "type": 23, "custom_id": "cb:defaults:unlisted", "value": %UNLISTED%}},
      {"id": 5, "type": 18, "component": {"id": 6, "type": 21, "custom_id": "cb:defaults:link", "value": %LINK%}}
    ]
  }
}`

// modalSubmitSelectsFixture is the FALLBACK shape's submit: the same Labels
// with String Selects (type 3) answering `values`, as the String Select's
// interaction response structure spells a modal's ("in modal interaction
// responses `type` will be returned").
const modalSubmitSelectsFixture = `{
  "type": 5,
  "token": "int-token-modal",
  "guild_id": "%GUILD%",
  "member": {"permissions": "32"},
  "data": {
    "custom_id": "cb:defaults:modal",
    "components": [
      {"id": 1, "type": 18, "component": {"id": 2, "type": 3, "custom_id": "cb:defaults:ttl", "values": [%TTL%]}},
      {"id": 3, "type": 18, "component": {"id": 4, "type": 3, "custom_id": "cb:defaults:unlisted", "values": [%UNLISTED%]}},
      {"id": 5, "type": 18, "component": {"id": 6, "type": 3, "custom_id": "cb:defaults:link", "values": [%LINK%]}}
    ]
  }
}`

func fill(tpl string, kv ...string) string {
	return strings.NewReplacer(kv...).Replace(tpl)
}

func clickBody(guild, perms, cid string) string {
	return fill(componentClickFixture, "%GUILD%", guild, "%PERMS%", perms, "%CID%", cid)
}

func submitBody(guild, perms, ttl, unlisted, link string) string {
	return fill(modalSubmitFixture, "%GUILD%", guild, "%PERMS%", perms, "%TTL%", ttl, "%UNLISTED%", unlisted, "%LINK%", link)
}

// sameJSON compares two JSON documents structurally.
func sameJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("the response is not JSON: %s", got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("the golden is not JSON: %v", err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("response\n  %s\nwant\n  %s", gb, wb)
	}
}

// settingsPanelGolden is the box's `/settings` answer (contract §4 last bullet,
// §5): type 4, flags IS_COMPONENTS_V2 | EPHEMERAL, one Container.
const settingsPanelGolden = `{"type":4,"data":{"flags":32832,"components":[{"type":17,"components":[
  {"type":9,"components":[{"type":10,"content":"**Link defaults**"}],
   "accessory":{"type":2,"style":2,"label":"Edit","custom_id":"cb:defaults"}},
  {"type":10,"content":"**Who may clip**\nServer Settings → Integrations → Clippet."}]}]}}`

func TestSettingsCommandAnswersTheOneRowPanel(t *testing.T) {
	b := newBotEnv(t)
	b.boundGuild(t, "panelowner", settingsGuild)
	for _, perms := range []string{permManage, permAdmin, permBothAndMore} {
		r := b.post(settingsCommandBody(settingsGuild, config.DefaultSettingsCommandName, perms))
		if r.code != http.StatusOK {
			t.Fatalf("/settings = %d: %s", r.code, r.body)
		}
		sameJSON(t, r.body, settingsPanelGolden)
	}
	if b.bot.count() != 0 {
		t.Fatal("/settings handed work to the bot")
	}
}

// The panel sits inside the contract's STRICT reading of Components V2's limits
// (dev-tests T4): ≤ 10 top-level, ≤ 40 in all, ≤ 4,000 characters of text,
// every custom_id ≤ 100 and unique.
func TestSettingsPanelIsInsideTheStrictLimits(t *testing.T) {
	var p map[string]any
	raw, _ := json.Marshal(settingsPanel())
	_ = json.Unmarshal(raw, &p)
	top := p["data"].(map[string]any)["components"].([]any)
	total, chars := 0, 0
	ids := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		total++
		if s, ok := m["content"].(string); ok {
			chars += utf8.RuneCountInString(s)
		}
		if id, ok := m["custom_id"].(string); ok {
			if len(id) > 100 || ids[id] {
				t.Fatalf("custom_id %q is too long or repeated", id)
			}
			ids[id] = true
		}
		for _, k := range []string{"components"} {
			if cs, ok := m[k].([]any); ok {
				for _, c := range cs {
					walk(c)
				}
			}
		}
		if a, ok := m["accessory"]; ok {
			walk(a)
		}
	}
	for _, c := range top {
		walk(c)
	}
	if len(top) > 10 || total > 40 || chars > 4000 {
		t.Fatalf("the panel is %d top-level, %d in all, %d characters", len(top), total, chars)
	}
}

func TestSettingsCommandAnswersTheConfiguredName(t *testing.T) {
	b := newBotEnvWith(t, map[string]string{config.EnvDiscordSettingsCommandName: "settings-dev"})
	b.boundGuild(t, "devowner", settingsGuild)
	sameJSON(t, b.post(settingsCommandBody(settingsGuild, "settings-dev", permManage)).body, settingsPanelGolden)
	// The default name is not published here, so it is not answered either.
	assertEphemeral(t, b.post(settingsCommandBody(settingsGuild, config.DefaultSettingsCommandName, permManage)), msgUnknown)
}

func TestSettingsCommandNameMatchesTheRegistration(t *testing.T) {
	if got := notify.SettingsCommandName(); got != config.DefaultSettingsCommandName {
		t.Fatalf("internal/notify registers %q by default, internal/config's default is %q", got, config.DefaultSettingsCommandName)
	}
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := testBotConfig(t, pub, "").SettingsCommandName(); got != config.DefaultSettingsCommandName {
		t.Fatalf("a bot configured without %s answers %q", config.EnvDiscordSettingsCommandName, got)
	}
}

// Proof A and the binding, on EVERY `/settings` interaction: the command, the
// click and the submit. Unbound first (the brief's order), then the bits.
func TestSettingsInteractionsNeedABoundServerAndManageServer(t *testing.T) {
	b := newBotEnv(t)
	const unbound = "777777777777777702"
	b.boundGuild(t, "gateowner", settingsGuild)
	bodies := func(guild, perms string) map[string]string {
		return map[string]string{
			"command": settingsCommandBody(guild, config.DefaultSettingsCommandName, perms),
			"click":   clickBody(guild, perms, customDefaults),
			"submit":  submitBody(guild, perms, `"7"`, "true", `"memorable"`),
		}
	}
	for kind, body := range bodies(unbound, permManage) {
		t.Run("unbound "+kind, func(t *testing.T) { assertEphemeral(t, b.post(body), msgUnbound) })
	}
	for _, perms := range []string{"0", permOrdinary, "not-a-number", "-32", "16", "4"} {
		for kind, body := range bodies(settingsGuild, perms) {
			t.Run("perms "+perms+" "+kind, func(t *testing.T) { assertEphemeral(t, b.post(body), msgNeedManage) })
		}
	}
	// No member object at all (a DM-shaped body in a guild): refused too.
	noMember := `{"type":3,"token":"t","guild_id":"` + settingsGuild + `","data":{"component_type":2,"custom_id":"cb:defaults"}}`
	assertEphemeral(t, b.post(noMember), msgNeedManage)
	// …and nothing was written by any refused submit.
	def := mustDefaults(t, b, "gateowner")
	if def.TTLDays != nil || def.Unlisted || def.LinkStyleOr() != db.LinkStyleCompact {
		t.Fatalf("a refused submit wrote the defaults: %+v", def)
	}
}

// A custom_id that is not `cb:` is not this instance's (the claim plane's `cf:`
// never reaches a box), and an unknown `cb:` one or a wrong pairing of type and
// id is the generic line.
func TestPanelInteractionsOutsideTheContractAreUnknown(t *testing.T) {
	b := newBotEnv(t)
	b.boundGuild(t, "unknownowner", settingsGuild)
	for _, body := range []string{
		clickBody(settingsGuild, permManage, "cf:settings:connect"),
		clickBody("777777777777777703", permManage, "cf:pass:refresh"), // unbound, still just unknown
		clickBody(settingsGuild, permManage, "cb:nothing"),
		clickBody(settingsGuild, permManage, customDefaultsModal), // a modal's id on a click
		strings.Replace(submitBody(settingsGuild, permManage, `"7"`, "true", `"compact"`), `"cb:defaults:modal"`, `"cb:defaults"`, 1),
		`{"type":3,"token":"t","guild_id":"` + settingsGuild + `","member":{"permissions":"32"},"data":{"component_type":2}}`,
	} {
		assertEphemeral(t, b.post(body), msgUnknown)
	}
}

// defaultsModalGolden is the modal for an account on the shipped defaults (no
// TTL preference, listed, compact) on an instance whose DEFAULT_CLIP_TTL is
// `never`: the contract's shape, the settings page's words.
const defaultsModalGolden = `{"type":9,"data":{"custom_id":"cb:defaults:modal","title":"Link defaults","components":[
  {"type":18,"label":"Delete after","component":{"type":21,"custom_id":"cb:defaults:ttl","options":[
    {"value":"default","label":"server default · never","default":true},
    {"value":"1","label":"1 day"},{"value":"7","label":"7 days"},{"value":"30","label":"30 days"},
    {"value":"90","label":"90 days"},{"value":"0","label":"never"}]}},
  {"type":18,"label":"Unlisted","description":"New clips are unlisted unless you say otherwise.",
   "component":{"type":23,"custom_id":"cb:defaults:unlisted","default":false}},
  {"type":18,"label":"Link","component":{"type":21,"custom_id":"cb:defaults:link","options":[
    {"value":"compact","label":"Compact","default":true},{"value":"memorable","label":"Memorable"}]}}]}}`

func TestEditOpensTheModalPrefilledFromTheAccount(t *testing.T) {
	b := newBotEnv(t, func(d *Deps) { d.Cfg.DefaultTTLSeconds = nil })
	b.boundGuild(t, "modalowner", settingsGuild)
	r := b.post(clickBody(settingsGuild, permManage, customDefaults))
	if r.code != http.StatusOK {
		t.Fatalf("Edit = %d: %s", r.code, r.body)
	}
	sameJSON(t, r.body, defaultsModalGolden)

	// Every legal default is the one marked: each TTL state, both Unlisted
	// states, both grammars.
	u := mustUser(t, b, "modalowner")
	for _, ttl := range []*int{nil, intp(0), intp(1), intp(7), intp(30), intp(90)} {
		for _, unlisted := range []bool{false, true} {
			for _, style := range []string{db.LinkStyleCompact, db.LinkStyleMemorable} {
				if err := b.db.SetUserDefaults(context.Background(), u.ID, db.UserDefaults{TTLDays: ttl, Unlisted: unlisted, LinkStyle: style}); err != nil {
					t.Fatal(err)
				}
				m := modalOf(t, b.post(clickBody(settingsGuild, permManage, customDefaults)))
				wantTTL := "default"
				if ttl != nil {
					wantTTL = itoa(*ttl)
				}
				if got := m.marked(customDefaultsTTL); got != wantTTL {
					t.Fatalf("ttl %v: marked %q, want %q", ttl, got, wantTTL)
				}
				if got := m.checkbox(customDefaultsUnlisted); got != unlisted {
					t.Fatalf("unlisted %v: the checkbox says %v", unlisted, got)
				}
				if got := m.marked(customDefaultsLink); got != style {
					t.Fatalf("style %s: marked %q", style, got)
				}
			}
		}
	}
}

// The modal is within Discord's documented modal limits: ≤ 5 components, a
// title and labels ≤ 45, descriptions and option texts ≤ 100, 2-10 radio
// options, every custom_id ≤ 100.
func TestDefaultsModalIsInsideDiscordsLimits(t *testing.T) {
	for _, shape := range []modalShape{modalRadios, modalSelects} {
		m := defaultsModal(db.UserDefaults{}, "90 days", shape)
		comps := m["components"].([]any)
		if len(comps) > 5 || utf8.RuneCountInString(m["title"].(string)) > 45 || len(m["custom_id"].(string)) > 100 {
			t.Fatalf("shape %d: %d components / title %q", shape, len(comps), m["title"])
		}
		for _, c := range comps {
			l := c.(map[string]any)
			if utf8.RuneCountInString(l["label"].(string)) > 45 {
				t.Fatalf("label %q", l["label"])
			}
			if d, ok := l["description"].(string); ok && utf8.RuneCountInString(d) > 100 {
				t.Fatalf("description %q", d)
			}
			in := l["component"].(map[string]any)
			if len(in["custom_id"].(string)) > 100 {
				t.Fatalf("custom_id %q", in["custom_id"])
			}
			if opts, ok := in["options"].([]any); ok {
				if len(opts) < 2 || len(opts) > 10 {
					t.Fatalf("%s has %d options", in["custom_id"], len(opts))
				}
				for _, o := range opts {
					if utf8.RuneCountInString(o.(map[string]any)["label"].(string)) > 100 {
						t.Fatalf("option %v", o)
					}
				}
			}
		}
	}
}

// The FALLBACK shape (dev-tests T5): the same three Labels with String Selects.
const defaultsModalSelectsGolden = `{"custom_id":"cb:defaults:modal","title":"Link defaults","components":[
  {"type":18,"label":"Delete after","component":{"type":3,"custom_id":"cb:defaults:ttl","options":[
    {"value":"default","label":"server default · 90 days"},
    {"value":"1","label":"1 day"},{"value":"7","label":"7 days","default":true},{"value":"30","label":"30 days"},
    {"value":"90","label":"90 days"},{"value":"0","label":"never"}]}},
  {"type":18,"label":"Unlisted","description":"New clips are unlisted unless you say otherwise.",
   "component":{"type":3,"custom_id":"cb:defaults:unlisted","options":[
    {"value":"false","label":"listed in the library"},{"value":"true","label":"not in the library","default":true}]}},
  {"type":18,"label":"Link","component":{"type":3,"custom_id":"cb:defaults:link","options":[
    {"value":"compact","label":"Compact"},{"value":"memorable","label":"Memorable","default":true}]}}]}`

func TestDefaultsModalFallbackShape(t *testing.T) {
	raw, _ := json.Marshal(defaultsModal(db.UserDefaults{TTLDays: intp(7), Unlisted: true, LinkStyle: db.LinkStyleMemorable}, "90 days", modalSelects))
	sameJSON(t, raw, defaultsModalSelectsGolden)
	if defaultsModalShape != modalRadios {
		t.Logf("NOTE: the shipped shape is the String Select fallback")
	}
}

func TestModalSubmitWritesThroughTheSettingsWriterAndSaysSaved(t *testing.T) {
	b := newBotEnv(t)
	b.boundGuild(t, "saveowner", settingsGuild)
	for _, tc := range []struct {
		ttl, unlisted, link string
		wantTTL             *int
		wantUnlisted        bool
		wantStyle           string
	}{
		{`"7"`, "true", `"memorable"`, intp(7), true, db.LinkStyleMemorable},
		{`"0"`, "false", `"compact"`, intp(0), false, db.LinkStyleCompact},
		{`"default"`, "true", `"compact"`, nil, true, db.LinkStyleCompact},
		{`"90"`, "false", `"memorable"`, intp(90), false, db.LinkStyleMemorable},
	} {
		r := b.post(submitBody(settingsGuild, permManage, tc.ttl, tc.unlisted, tc.link))
		sameJSON(t, r.body, `{"type":4,"data":{"content":"Changes saved.","flags":64}}`)
		def := mustDefaults(t, b, "saveowner")
		if !sameTTL(def.TTLDays, tc.wantTTL) || def.Unlisted != tc.wantUnlisted || def.LinkStyle != tc.wantStyle {
			t.Fatalf("%s/%s/%s wrote %+v", tc.ttl, tc.unlisted, tc.link, def)
		}
	}
	// The fallback shape saves through the same path.
	r := b.post(fill(modalSubmitSelectsFixture, "%GUILD%", settingsGuild, "%TTL%", `"30"`, "%UNLISTED%", `"true"`, "%LINK%", `"compact"`))
	assertEphemeral(t, r, msgSaved)
	if def := mustDefaults(t, b, "saveowner"); !sameTTL(def.TTLDays, intp(30)) || !def.Unlisted || def.LinkStyle != db.LinkStyleCompact {
		t.Fatalf("the select-shaped submit wrote %+v", def)
	}
}

func TestModalSubmitOutsideTheChoicesIsRefusedAndWritesNothing(t *testing.T) {
	b := newBotEnv(t)
	b.boundGuild(t, "badowner", settingsGuild)
	for name, body := range map[string]string{
		"ttl 5":             submitBody(settingsGuild, permManage, `"5"`, "true", `"compact"`),
		"ttl null":          submitBody(settingsGuild, permManage, `null`, "true", `"compact"`),
		"ttl a number":      submitBody(settingsGuild, permManage, `7`, "true", `"compact"`),
		"ttl empty":         submitBody(settingsGuild, permManage, `""`, "true", `"compact"`),
		"link words":        submitBody(settingsGuild, permManage, `"7"`, "true", `"words"`),
		"link null":         submitBody(settingsGuild, permManage, `"7"`, "true", `null`),
		"unlisted a string": submitBody(settingsGuild, permManage, `"7"`, `"yes"`, `"compact"`),
		"unlisted null":     submitBody(settingsGuild, permManage, `"7"`, `null`, `"compact"`),
		"a field missing": strings.Replace(submitBody(settingsGuild, permManage, `"7"`, "true", `"compact"`),
			`"cb:defaults:unlisted"`, `"cb:defaults:other"`, 1),
		"no components":         `{"type":5,"token":"t","guild_id":"` + settingsGuild + `","member":{"permissions":"32"},"data":{"custom_id":"cb:defaults:modal"}}`,
		"select two values":     fill(modalSubmitSelectsFixture, "%GUILD%", settingsGuild, "%TTL%", `"1","7"`, "%UNLISTED%", `"true"`, "%LINK%", `"compact"`),
		"select unlisted maybe": fill(modalSubmitSelectsFixture, "%GUILD%", settingsGuild, "%TTL%", `"1"`, "%UNLISTED%", `"TRUE"`, "%LINK%", `"compact"`),
	} {
		t.Run(name, func(t *testing.T) { assertEphemeral(t, b.post(body), msgNotAChoice) })
	}
	def := mustDefaults(t, b, "badowner")
	if def.TTLDays != nil || def.Unlisted || def.LinkStyleOr() != db.LinkStyleCompact {
		t.Fatalf("a refused submit wrote the defaults: %+v", def)
	}
}

// The words are the settings page's, and the writer is the page's: the modal's
// TTL and link labels are read off the RENDERED /settings page, and a value
// saved from Discord is what that page then shows selected.
func TestModalWordsAndWritesAreTheSettingsPages(t *testing.T) {
	for _, ttlSecs := range []*int64{nil, i64p(7 * 86400), i64p(86400)} {
		b := newBotEnv(t, func(d *Deps) { d.Cfg.DefaultTTLSeconds = ttlSecs })
		owner, cookie := b.newUser("pageowner")
		if _, err := b.db.BindGuild(context.Background(), settingsGuild, owner.ID, b.now()); err != nil {
			t.Fatal(err)
		}
		page := renderSettings(t, b, cookie)

		sel := regexp.MustCompile(`(?s)<select id="d-ttl"[^>]*>(.*?)</select>`).FindStringSubmatch(page)
		if sel == nil {
			t.Fatalf("the settings page has no TTL select:\n%s", page)
		}
		var pageLabels []string
		for _, m := range regexp.MustCompile(`<option value="([^"]*)"[^>]*>([^<]*)</option>`).FindAllStringSubmatch(sel[1], -1) {
			pageLabels = append(pageLabels, m[2])
		}
		m := modalOf(t, b.post(clickBody(settingsGuild, permManage, customDefaults)))
		if got := m.labels(customDefaultsTTL); strings.Join(got, "|") != strings.Join(pageLabels, "|") {
			t.Fatalf("the modal's TTL words %q are not the page's %q", got, pageLabels)
		}
		for _, w := range m.labels(customDefaultsLink) {
			if !strings.Contains(page, "<span>"+w+"</span>") {
				t.Fatalf("the link label %q is not on the settings page", w)
			}
		}
		if !strings.Contains(page, `<label for="d-ttl">`+labelTTL+`</label>`) {
			t.Fatalf("the modal's TTL label %q is not the page's", labelTTL)
		}

		// Save from Discord; the PAGE shows it.
		assertEphemeral(t, b.post(submitBody(settingsGuild, permManage, `"30"`, "true", `"memorable"`)), msgSaved)
		page = renderSettings(t, b, cookie)
		if !regexp.MustCompile(`<option value="30" selected="selected">`).MatchString(page) ||
			!strings.Contains(page, `data-defaults-unlisted checked="checked"`) ||
			!regexp.MustCompile(`value="memorable" checked`).MatchString(page) {
			t.Fatal("the settings page does not show what Discord saved")
		}
	}
}

// The access log records the interaction's kind and the custom_id's PREFIX —
// never a submitted value and never the full custom_id.
func TestPanelInteractionsLogThePrefixAndNoValue(t *testing.T) {
	b := newBotEnv(t)
	b.boundGuild(t, "logowner", settingsGuild)
	_, logs := b.postLogged(submitBody(settingsGuild, permManage, `"90"`, "true", `"memorable"`))
	if !strings.Contains(logs, "discord=modal") || !strings.Contains(logs, "discord_prefix=cb:") {
		t.Fatalf("the access log lacks the kind or the prefix:\n%s", logs)
	}
	for _, leak := range []string{"memorable", "cb:defaults", "permissions", "someone"} {
		if strings.Contains(logs, leak) {
			t.Fatalf("the access log carries %q:\n%s", leak, logs)
		}
	}
	_, logs = b.postLogged(clickBody(settingsGuild, permManage, "cf:settings:connect"))
	if !strings.Contains(logs, "discord=component") || !strings.Contains(logs, "discord_prefix=other") || strings.Contains(logs, "cf:") {
		t.Fatalf("a foreign click's log line:\n%s", logs)
	}
}

// --- helpers ----------------------------------------------------------------

func i64p(n int64) *int64    { return &n }
func itoa(n int) string      { b, _ := json.Marshal(n); return string(b) }
func sameTTL(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func mustUser(t *testing.T, b *botEnv, name string) db.User {
	t.Helper()
	u, err := b.db.GetUserByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustDefaults(t *testing.T, b *botEnv, name string) db.UserDefaults {
	t.Helper()
	def, err := b.db.GetUserDefaults(context.Background(), mustUser(t, b, name).ID)
	if err != nil {
		t.Fatal(err)
	}
	return def
}

// renderSettings renders the REAL /settings page for an account, through
// internal/web over the same database (the cross-package seam library_link_test
// already uses: a test may import internal/web; production may not).
func renderSettings(t *testing.T, b *botEnv, cookie *http.Cookie) string {
	t.Helper()
	base, _ := url.Parse(testOrigin)
	pages, err := web.New(web.Deps{
		DB: b.db, Auth: &auth.Resolver{DB: b.db, Cfg: b.cfg, Now: time.Now},
		URLs: urls.New(base, "test"), Cfg: b.cfg, Log: slog.New(slog.DiscardHandler), Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(cookie)
	pages.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d", rec.Code)
	}
	return rec.Body.String()
}

type modalView struct{ inputs map[string]map[string]any }

func modalOf(t *testing.T, r resp) modalView {
	t.Helper()
	if got, _ := r.json["type"].(float64); int(got) != callbackModal {
		t.Fatalf("type %v, want %d (MODAL): %s", r.json["type"], callbackModal, r.body)
	}
	v := modalView{inputs: map[string]map[string]any{}}
	for _, c := range r.json["data"].(map[string]any)["components"].([]any) {
		in := c.(map[string]any)["component"].(map[string]any)
		v.inputs[in["custom_id"].(string)] = in
	}
	return v
}

func (m modalView) options(id string) []map[string]any {
	var out []map[string]any
	for _, o := range m.inputs[id]["options"].([]any) {
		out = append(out, o.(map[string]any))
	}
	return out
}

func (m modalView) marked(id string) string {
	got := ""
	for _, o := range m.options(id) {
		if d, _ := o["default"].(bool); d {
			if got != "" {
				return "MORE THAN ONE"
			}
			got = o["value"].(string)
		}
	}
	return got
}

func (m modalView) labels(id string) []string {
	var out []string
	for _, o := range m.options(id) {
		out = append(out, o["label"].(string))
	}
	return out
}

func (m modalView) checkbox(id string) bool {
	d, _ := m.inputs[id]["default"].(bool)
	return d
}
