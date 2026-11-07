package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"clippet/internal/db"
	"clippet/internal/httpx"
)

// `/settings` on the instance (§20 erratum 318; run 4's panel contract §4/§5):
// the bound account's LINK DEFAULTS, from Discord.
//
// Three interactions, and every one of them is answered on this response:
//
//  1. The box's own `/settings` command (type 2, DISCORD_SETTINGS_COMMAND_NAME)
//     → an ephemeral Components V2 panel: one Section — "Link defaults" with an
//     `Edit` button — and the "Who may clip" line. On a MANAGED instance the
//     official bot's forwarder composes its own, larger panel and never relays
//     the command here; this answer is for a self-hosted application, and the
//     sane answer if one arrives anyway.
//  2. A click on `Edit` (type 3, custom_id `cb:defaults`) — from that panel or
//     from the forwarder's, which relays every `cb:` click here exactly as it
//     relays `/clip` → a MODAL pre-filled from the bound account's defaults.
//  3. The modal saved (type 5, custom_id `cb:defaults:modal`) → validated
//     against the product's own closed sets and written through changeDefaults,
//     the ONE writer the settings page's PATCH uses → `Changes saved.`, ephemeral.
//
// Before any of it: the server is bound (msgUnbound otherwise) and the member
// holds ADMINISTRATOR or MANAGE_GUILD on the interaction's own guild-computed
// permission bitfield (proof A). The command is published with
// `default_member_permissions: "32"`, so Discord already hides it from
// everybody else — but a server's admins may widen it, and a click is a body
// anybody holding a signature could have sent, so it is re-checked on every
// interaction here (defence in depth, as the contract asks).
//
// Nothing here is logged about a person: the access log carries the
// interaction's kind and the custom_id's PREFIX, never a submitted value, a
// member or the custom_id beyond its prefix.

// The custom_ids this instance owns. The `cb:` prefix is the contract's: the
// forwarder routes every custom_id that starts with it to the box (§2), and the
// claim plane owns `cf:`.
const (
	panelPrefix         = "cb:"
	customDefaults      = "cb:defaults"       // the Edit button
	customDefaultsModal = "cb:defaults:modal" // the modal it opens
	// The modal's three inputs. Not user-facing; each ≤ 100 characters.
	customDefaultsTTL      = "cb:defaults:ttl"
	customDefaultsUnlisted = "cb:defaults:unlisted"
	customDefaultsLink     = "cb:defaults:link"
)

// Discord's component TYPE values this file writes and reads (the components
// reference; pinned by the fixtures in discord_settings_test.go).
const (
	componentButton       = 2
	componentStringSelect = 3
	componentSection      = 9
	componentTextDisplay  = 10
	componentContainer    = 17
	componentLabel        = 18
	componentRadioGroup   = 21
	componentCheckbox     = 23
	buttonSecondary       = 2
)

// flagComponentsV2 is IS_COMPONENTS_V2 (1 << 15): a message made of components
// alone. The panel sets it beside the ephemeral flag.
const flagComponentsV2 = 1 << 15

// Proof A's two bits (Discord's permission flags): ADMINISTRATOR and
// MANAGE_GUILD. A channel overwrite cannot touch MANAGE_GUILD (dev-tests T1),
// so the bitfield on the interaction is the guild's own answer.
const (
	permAdministrator = 1 << 3
	permManageGuild   = 1 << 5
)

// The words. Every one is the contract's (§6), or the settings page's
// own for the same choice — this file adds none.
const (
	msgNeedManage = "You need Manage Server here."
	msgNotAChoice = "That is not a choice here."
	msgSaved      = "Changes saved."
	// The label alone, as the official bot's row draws it (run 14f, settingsui).
	panelDefaults   = "**Link defaults**"
	panelWhoMayClip = "**Who may clip**\nServer Settings → Integrations → Clippet."
	labelEdit       = "Edit"
	modalTitle      = "Link defaults"
	// The modal's three labels. `Delete after` is the settings page's own label
	// for the same choice (§20 erratum 244: "it's not keep for never"); the
	// contract's first draft for it was withdrawn.
	labelTTL            = "Delete after"
	labelUnlisted       = "Unlisted"
	descriptionUnlisted = "New clips are unlisted unless you say otherwise."
	labelLink           = "Link"
)

// The TTL choices, in the settings page's order and with its exact words
// (internal/web's ttlOptions and its "server default · …" entry; pinned
// against the rendered page by a test, since §3 forbids the import). The first
// is the account's THIRD state — no preference, follow DEFAULT_CLIP_TTL — and
// its value is a word because a radio's value may not be empty.
const ttlFollowInstance = "default"

var defaultsTTLChoices = []struct{ Value, Label string }{
	{"1", "1 day"},
	{"7", "7 days"},
	{"30", "30 days"},
	{"90", "90 days"},
	{"0", "never"},
}

// The link grammars, with the settings page's labels.
var defaultsLinkChoices = []struct{ Value, Label string }{
	{db.LinkStyleCompact, "Compact"},
	{db.LinkStyleMemorable, "Memorable"},
}

// The String Select fallback's two Unlisted choices (a select cannot be a
// checkbox): the clip page's own words for the two states of the same switch.
var defaultsUnlistedChoices = []struct{ Value, Label string }{
	{"false", "listed in the library"},
	{"true", "not in the library"},
}

// modalShape is the modal's control set. The contract's shape is a Radio Group
// for each choice and a Checkbox for Unlisted; the fallback (dev-tests T5) is
// the same three Labels with String Selects, for a client that cannot draw the
// first. defaultsModalShape is the ONE line that picks between them — and the
// submit parser reads EITHER shape, so a modal opened before a flip still saves.
type modalShape int

const (
	modalRadios modalShape = iota
	modalSelects
)

const defaultsModalShape = modalRadios

// discordModalComponent is one component of a MODAL_SUBMIT's `data.components`:
// a Label (type 18) carrying its one `component`, or — for a client that
// still answers the legacy shape — an Action Row (type 1) carrying
// `components`. A leaf's answer is `value` (a Radio Group's string or null, a
// Checkbox's bool, a Text Input's string; kept RAW so a wrong type is a refusal
// rather than a decode failure of the whole body) or `values` (a select's).
type discordModalComponent struct {
	Type       int                     `json:"type"`
	CustomID   string                  `json:"custom_id"`
	Value      json.RawMessage         `json:"value"`
	Values     []string                `json:"values"`
	Component  *discordModalComponent  `json:"component"`
	Components []discordModalComponent `json:"components"`
}

// discordPanel answers a component click (type 3) or a modal submit (type 5).
func (h *handler) discordPanel(w http.ResponseWriter, r *http.Request, in discordInteraction) {
	kind := "component"
	if in.Type == interactionModalSubmit {
		kind = "modal"
	}
	httpx.SetLogField(r, "discord", kind)
	if !strings.HasPrefix(in.Data.CustomID, panelPrefix) {
		// Not ours: the claim plane's `cf:` never reaches a box, and nothing
		// else is published. The prefix is all the log gets.
		httpx.SetLogField(r, "discord_prefix", "other")
		writeEphemeral(w, msgUnknown)
		return
	}
	httpx.SetLogField(r, "discord_prefix", panelPrefix)
	if in.Type == interactionModalSubmit && strings.HasPrefix(in.Data.CustomID, formPrefix) {
		// The upload form's submit (§20 erratum 320): the uploader needs no
		// Manage Server — the form's nonce is the credential. Everything else
		// under `cb:`, a click on a `cb:form:` id included, is gated below.
		h.discordFormSubmit(w, r, in)
		return
	}
	binding, ok := h.panelGate(w, r, in)
	if !ok {
		return
	}
	switch {
	case in.Type == interactionComponent && in.Data.CustomID == customDefaults:
		def, err := h.d.DB.GetUserDefaults(r.Context(), binding.UserID)
		if err != nil {
			h.d.Log.Warn("discord bot: read the bound account's defaults", "err", err)
			writeEphemeral(w, msgUnavailable)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"type": callbackModal,
			"data": defaultsModal(def, ttlLabelOf(instanceDefaultTTLDays(h.d.Cfg.DefaultTTLSeconds)), defaultsModalShape),
		})
	case in.Type == interactionModalSubmit && in.Data.CustomID == customDefaultsModal:
		h.saveDefaultsFromModal(w, r, in, binding)
	default:
		writeEphemeral(w, msgUnknown)
	}
}

// discordSettingsCommand is the box's own `/settings`: the one-row panel.
func (h *handler) discordSettingsCommand(w http.ResponseWriter, r *http.Request, in discordInteraction, command string) {
	if _, ok := h.guildBinding(w, r, in, command); !ok {
		return
	}
	if !hasManagePermission(in.Member) {
		httpx.SetLogField(r, "reject", "no_manage_permission")
		writeEphemeral(w, msgNeedManage)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, settingsPanel())
}

// panelGate is what every `cb:` interaction checks first, in the brief's order:
// the server is bound, then proof A. It answers the refusal itself.
func (h *handler) panelGate(w http.ResponseWriter, r *http.Request, in discordInteraction) (db.GuildBinding, bool) {
	binding, ok := h.guildBinding(w, r, in, h.d.Cfg.DiscordBot.SettingsCommandName())
	if !ok {
		return db.GuildBinding{}, false
	}
	if !hasManagePermission(in.Member) {
		httpx.SetLogField(r, "reject", "no_manage_permission")
		writeEphemeral(w, msgNeedManage)
		return db.GuildBinding{}, false
	}
	return binding, true
}

// hasManagePermission is proof A: the member's guild-computed permission
// bitfield (a decimal string) carries ADMINISTRATOR or MANAGE_GUILD. No member
// (a DM), no bitfield or one that is not a number → no.
func hasManagePermission(m *discordMember) bool {
	if m == nil {
		return false
	}
	// Fails CLOSED, deliberately kept (run 8): were Discord's bitfield ever to
	// pass 64 bits, ParseUint refuses it and the member reads as no manager —
	// on both gates that read it — rather than as one.
	bits, err := strconv.ParseUint(strings.TrimSpace(m.Permissions), 10, 64)
	if err != nil {
		return false
	}
	return bits&(permAdministrator|permManageGuild) != 0
}

// settingsPanel is the box's `/settings` answer (contract §4, last bullet; §5):
// type 4, ephemeral, Components V2 — ONE Container holding the "Link defaults"
// Section with its `Edit` button and the "Who may clip" line. No accent: the
// colour is the forwarder's connection signal, and this panel states no
// connection. Two top-level components, five in all.
func settingsPanel() map[string]any {
	return map[string]any{
		"type": callbackMessage,
		"data": map[string]any{
			"flags": flagComponentsV2 | ephemeralFlag,
			"components": []any{map[string]any{
				"type": componentContainer,
				"components": []any{
					map[string]any{
						"type":       componentSection,
						"components": []any{map[string]any{"type": componentTextDisplay, "content": panelDefaults}},
						"accessory": map[string]any{
							"type": componentButton, "style": buttonSecondary,
							"label": labelEdit, "custom_id": customDefaults,
						},
					},
					map[string]any{"type": componentTextDisplay, "content": panelWhoMayClip},
				},
			}},
		},
	}
}

// ttlLabelOf renders DEFAULT_CLIP_TTL (in days, 0 = never) the way the settings
// page's "server default · …" entry does (internal/web's defaultTTLLabel).
func ttlLabelOf(days int) string {
	switch days {
	case 0:
		return "never"
	case 1:
		return "1 day"
	}
	return strconv.Itoa(days) + " days"
}

// selectedTTL is the account's TTL default as a choice value.
func selectedTTL(def db.UserDefaults) string {
	if def.TTLDays == nil {
		return ttlFollowInstance
	}
	return strconv.Itoa(*def.TTLDays)
}

// defaultsModal is the MODAL's `data` (contract §4/§5): title, custom_id and
// three Labels, each pre-filled from the account's current defaults. Within
// Discord's limits by construction: 3 of 5 components, a ≤ 45-character title
// and labels, ≤ 100-character descriptions and option texts, 6 options in a
// group that allows 10.
func defaultsModal(def db.UserDefaults, instanceTTLLabel string, shape modalShape) map[string]any {
	ttl := make([]choice, 0, len(defaultsTTLChoices)+1)
	ttl = append(ttl, choice{ttlFollowInstance, "server default · " + instanceTTLLabel})
	for _, c := range defaultsTTLChoices {
		ttl = append(ttl, choice{c.Value, c.Label})
	}
	link := make([]choice, 0, len(defaultsLinkChoices))
	for _, c := range defaultsLinkChoices {
		link = append(link, choice{c.Value, c.Label})
	}
	style := def.LinkStyleOr()

	var ttlInput, unlistedInput, linkInput map[string]any
	switch shape {
	case modalSelects:
		unlisted := make([]choice, 0, len(defaultsUnlistedChoices))
		for _, c := range defaultsUnlistedChoices {
			unlisted = append(unlisted, choice{c.Value, c.Label})
		}
		ttlInput = selectInput(customDefaultsTTL, ttl, selectedTTL(def))
		unlistedInput = selectInput(customDefaultsUnlisted, unlisted, strconv.FormatBool(def.Unlisted))
		linkInput = selectInput(customDefaultsLink, link, style)
	default:
		ttlInput = radioInput(customDefaultsTTL, ttl, selectedTTL(def))
		unlistedInput = map[string]any{"type": componentCheckbox, "custom_id": customDefaultsUnlisted, "default": def.Unlisted}
		linkInput = radioInput(customDefaultsLink, link, style)
	}
	return map[string]any{
		"custom_id": customDefaultsModal,
		"title":     modalTitle,
		"components": []any{
			map[string]any{"type": componentLabel, "label": labelTTL, "component": ttlInput},
			map[string]any{"type": componentLabel, "label": labelUnlisted, "description": descriptionUnlisted, "component": unlistedInput},
			map[string]any{"type": componentLabel, "label": labelLink, "component": linkInput},
		},
	}
}

type choice struct{ value, label string }

// radioInput is a Radio Group (type 21) with the current value marked
// `default`; a stored value outside the set marks none, and the group's
// `required` (Discord's default) makes the person pick one.
func radioInput(id string, choices []choice, selected string) map[string]any {
	return map[string]any{"type": componentRadioGroup, "custom_id": id, "options": options(choices, selected)}
}

// selectInput is the fallback: a single-choice String Select (type 3).
func selectInput(id string, choices []choice, selected string) map[string]any {
	return map[string]any{"type": componentStringSelect, "custom_id": id, "options": options(choices, selected)}
}

func options(choices []choice, selected string) []any {
	out := make([]any, 0, len(choices))
	for _, c := range choices {
		o := map[string]any{"value": c.value, "label": c.label}
		if c.value == selected {
			o["default"] = true
		}
		out = append(out, o)
	}
	return out
}

// saveDefaultsFromModal reads the three answers, refuses anything outside the
// product's closed sets with the one sentence, and writes through
// changeDefaults — the settings page's own writer.
func (h *handler) saveDefaultsFromModal(w http.ResponseWriter, r *http.Request, in discordInteraction, binding db.GuildBinding) {
	change, ok := defaultsFromModal(in.Data.Components)
	if !ok {
		httpx.SetLogField(r, "reject", "not_a_choice")
		writeEphemeral(w, msgNotAChoice)
		return
	}
	_, err := h.changeDefaults(r.Context(), binding.UserID, change)
	switch {
	case errors.Is(err, errDefaultsTTL), errors.Is(err, errDefaultsLinkStyle):
		httpx.SetLogField(r, "reject", "not_a_choice")
		writeEphemeral(w, msgNotAChoice)
		return
	case err != nil:
		h.d.Log.Warn("discord bot: write the bound account's defaults", "err", err)
		writeEphemeral(w, msgUnavailable)
		return
	}
	writeEphemeral(w, msgSaved)
}

// defaultsFromModal turns a MODAL_SUBMIT's components into a defaultsChange.
// All three answers must be present and each must be one of its choices; the
// shape of each may be either modalShape's (a Radio Group or a select for the
// two choices, a Checkbox or a select for Unlisted).
func defaultsFromModal(comps []discordModalComponent) (defaultsChange, bool) {
	leaves := modalLeaves(comps)

	var change defaultsChange
	ttl, ok := singleChoice(leaves[customDefaultsTTL])
	if !ok {
		return defaultsChange{}, false
	}
	change.TTL.set = true
	if ttl == ttlFollowInstance {
		change.TTL.null = true
	} else {
		legal := false
		for _, c := range defaultsTTLChoices {
			legal = legal || c.Value == ttl
		}
		if !legal {
			return defaultsChange{}, false
		}
		change.TTL.days, _ = strconv.Atoi(ttl)
	}

	unlisted, ok := checkedOrChoice(leaves[customDefaultsUnlisted])
	if !ok {
		return defaultsChange{}, false
	}
	change.Unlisted = &unlisted

	style, ok := singleChoice(leaves[customDefaultsLink])
	if !ok || !db.ValidLinkStyle(style) {
		return defaultsChange{}, false
	}
	change.LinkStyle = &style
	return change, true
}

// modalLeaves walks a MODAL_SUBMIT's components — Label-wrapped or in the
// legacy Action Rows — into its leaves by custom_id. Both modals use it: the
// link defaults and the upload form (§20 erratum 320).
func modalLeaves(comps []discordModalComponent) map[string]discordModalComponent {
	leaves := map[string]discordModalComponent{}
	var walk func([]discordModalComponent)
	walk = func(cs []discordModalComponent) {
		for _, c := range cs {
			if c.Component != nil {
				walk([]discordModalComponent{*c.Component})
			}
			if len(c.Components) > 0 {
				walk(c.Components)
			}
			if c.CustomID != "" {
				leaves[c.CustomID] = c
			}
		}
	}
	walk(comps)
	return leaves
}

// singleChoice reads one Radio Group's `value` (a string; null is no choice) or
// one select's single `values` entry.
func singleChoice(c discordModalComponent) (string, bool) {
	switch c.Type {
	case componentRadioGroup:
		var s *string
		if err := json.Unmarshal(c.Value, &s); err != nil || s == nil {
			return "", false
		}
		return *s, true
	case componentStringSelect:
		if len(c.Values) != 1 {
			return "", false
		}
		return c.Values[0], true
	}
	return "", false
}

// checkedOrChoice reads a Checkbox's boolean `value`, or the fallback select's
// "true"/"false".
func checkedOrChoice(c discordModalComponent) (bool, bool) {
	switch c.Type {
	case componentCheckbox:
		var b *bool
		if err := json.Unmarshal(c.Value, &b); err != nil || b == nil {
			return false, false
		}
		return *b, true
	case componentStringSelect:
		if len(c.Values) != 1 {
			return false, false
		}
		b, err := strconv.ParseBool(c.Values[0])
		if err != nil || (c.Values[0] != "true" && c.Values[0] != "false") {
			return false, false
		}
		return b, true
	}
	return false, false
}
