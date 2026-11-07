package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/notify"
)

// The upload form (§20 erratum 320; ROADMAP #33 "Wish (3)"; the owner's ask of
// 2026-09-15: while the bot is thinking, set the title and tags).
//
// With DISCORD_UPLOAD_FORM on (the default), `/upload` is answered with a
// MODAL instead of a deferral, and the upload starts at once behind it. The
// modal carries Title (pre-filled from the file name), Delete after and Tags.
// Nobody waits on the form: the job posts the link as a follow-up message
// whenever the clip is ready, whether the form was sent, closed or is still
// open.
//
// Since §20 erratum 325 (the owner's form test on the dev instance) the job also posts a private
// "Uploading…" while it runs and deletes it at the end (a closed form is
// otherwise silent), and a submit that lands AFTER the link was posted and
// changes the title asks the bot to redraw that link's card (Bot.Redraw: the
// link edited to `?v=2`, so Discord fetches the page again). The interaction
// token and the message id for that live in internal/notify, never here.
//
// The submit is tied to the upload by a 128-bit nonce in the modal's
// custom_id (`cb:form:<32 hex>`), held in an in-memory table for fifteen
// minutes — the interaction token's life. The nonce IS the credential: Discord
// shows a modal only to the person who ran the command, the nonce is never in a
// channel or a log, and anyone else holding it got it from that person. So the
// member is not read for it (only Enrich on a video compares the member's id
// with the message's author, §20 erratum 323);
// the submit must come from the same server, and a form takes ONE answer — the
// box checks no timestamp window on signed bodies, so a captured submit must
// not be replayable.
//
// Nothing typed into the form is logged, and no answer echoes it back.

// The custom_ids. The modal's own carries the nonce; its three inputs are
// fixed. Every one starts with panelPrefix, so the official bot's forwarder
// relays the submit to the box exactly as it relays `/settings`' clicks.
const (
	formPrefix      = panelPrefix + "form:"
	customFormTitle = formPrefix + "title"
	customFormTTL   = formPrefix + "ttl"
	customFormTags  = formPrefix + "tags"
)

// Discord's Text Input component (type 4), short style (1). Pinned by the
// fixtures in discord_form_test.go, as the other component types are.
const (
	componentTextInput = 4
	textInputShort     = 1
)

// The form's limits: fifteen minutes (the interaction token's life — past it
// nothing can be delivered anyway), 256 open forms (a full table answers the
// old way, never an error), and the inputs' max lengths: the title's rule, and
// the web's widest legal tag value (329, the settings of erratum 320's review).
const (
	formTTL          = 15 * time.Minute
	formTableMax     = 256
	formTagsMaxChars = 329
)

// The words (settled by the design review of erratum 320).
const (
	formModalTitle      = "Your clip is uploading"
	formHint            = "Cancel keeps the defaults."
	formLabelTitle      = "Title"
	formLabelTags       = "Tags"
	formTagsDescription = "comma separated; a-z, 0-9 and -"
	msgFormExpired      = "That form has expired."
	msgFormTagsGrammar  = "Saved without the tags. A tag must be 1-32 characters of a-z, 0-9 and -, starting with a letter or a digit."
	msgFormTagsTooMany  = "Saved without the tags. A clip may carry at most 10 tags."
	msgFormTitleRefused = "Not saved: a title is at most 200 characters, with no control characters."
	// formDefaultPrefix is the first Delete-after option's lead: "default ·
	// {what this upload gets}". Not "server default": inside Discord "server"
	// reads as the guild, and an account default may be what applies.
	formDefaultPrefix = "default · "
)

// formEntry is one open form. It holds no interaction token and nothing about
// the member: the server it was opened in, the clip once the ingest made it,
// and a submit that arrived first.
type formEntry struct {
	guildID   string
	clipID    string        // "" until the ingest attaches the clip
	pending   *db.ClipPatch // a submit held for the clip (user text: memory only)
	submitted bool          // the one answer has been taken
	failed    bool          // the job ended without a clip
	dup       bool          // the upload was a duplicate: nothing is ever applied (§20 erratum 326)
	expires   time.Time
}

// formTable is the box's in-memory record of open forms. Lost on a restart by
// design: a restart also ends the job, which says so in the channel.
type formTable struct {
	mu sync.Mutex
	m  map[string]*formEntry
}

func newFormTable() *formTable { return &formTable{m: map[string]*formEntry{}} }

// open mints a nonce and records a form for guildID, or reports that the table
// is full. Expired entries are swept first.
func (t *formTable) open(guildID string, now time.Time) (string, bool) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", false
	}
	nonce := hex.EncodeToString(raw[:])
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, e := range t.m {
		if !now.Before(e.expires) {
			delete(t.m, k)
		}
	}
	if len(t.m) >= formTableMax {
		return "", false
	}
	t.m[nonce] = &formEntry{guildID: guildID, expires: now.Add(formTTL)}
	return nonce, true
}

// attach records the clip the ingest created and hands back a submit that
// arrived before it (the caller writes it, outside the lock). A held submit
// consumes the entry.
func (t *formTable) attach(nonce, clipID string) *db.ClipPatch {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[nonce]
	if !ok {
		return nil
	}
	if e.submitted {
		delete(t.m, nonce)
		return e.pending
	}
	e.clipID = clipID
	return nil
}

// existing records that the upload was a duplicate (§20 erratum 326): the clip
// was already the account's, so the form applies to nothing. A submit already
// held is dropped with the entry, and existing reports true: that person was
// answered "Changes saved." and only the job can tell them otherwise. Otherwise the
// entry stays so a later submit is answered, not written — by its own answer
// alone (run 8: the line is heard once).
func (t *formTable) existing(nonce string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[nonce]
	if !ok {
		return false
	}
	if e.submitted {
		delete(t.m, nonce)
		return true
	}
	e.dup = true
	return false
}

// fail records that the job ended without a clip. A submit already held for it
// is dropped (the channel gets the failure sentence). An entry that already has
// a clip — attached, or a duplicate's existing one (§20 erratum 326) — is never
// marked: a panic after the link (a duplicate's link POST that failed, then a
// fault in the job's tail) must not tell a later submit there is no clip.
func (t *formTable) fail(nonce string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[nonce]
	if !ok || e.clipID != "" || e.dup {
		return
	}
	if e.submitted {
		delete(t.m, nonce)
		return
	}
	e.failed = true
}

// formOutcome is what take decided.
type formOutcome int

const (
	formExpired    formOutcome = iota // unknown, expired, another server, already answered
	formFailed                        // the upload failed; the entry is gone now
	formRefused                       // the body failed validation; the entry stays
	formWrite                         // the clip exists: write it now; the entry is gone
	formHeld                          // held for the clip; attach applies it
	formNotApplied                    // the upload was a duplicate: nothing is written; the entry is gone
)

// take is the submit's one locked step: it decides, and consumes the entry for
// every outcome but a refusal.
func (t *formTable) take(nonce, guildID string, now time.Time, valid bool, patch db.ClipPatch) (formOutcome, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[nonce]
	if !ok || e.submitted || e.guildID != guildID {
		return formExpired, ""
	}
	if !now.Before(e.expires) {
		delete(t.m, nonce)
		return formExpired, ""
	}
	if e.failed {
		delete(t.m, nonce)
		return formFailed, ""
	}
	if e.dup {
		delete(t.m, nonce)
		return formNotApplied, ""
	}
	if !valid {
		return formRefused, ""
	}
	if e.clipID != "" {
		delete(t.m, nonce)
		return formWrite, e.clipID
	}
	p := patch
	e.pending = &p
	e.submitted = true
	return formHeld, ""
}

// len is for the tests.
func (t *formTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

// validNonce is the shape a nonce has: 32 lower-case hex digits.
func validNonce(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// formDefaultLabel is what this upload gets if the form leaves Delete after
// alone, in the settings page's words: the bound account's own default when it
// has a legal one, else DEFAULT_CLIP_TTL — the order applyDefaultsCtx applies.
func (h *handler) formDefaultLabel(ctx context.Context, userID string) string {
	def := h.userDefaults(ctx, userID)
	if def.TTLDays != nil {
		if _, err := config.ParseTTLDays(def.TTLDays, nil); err == nil {
			return ttlLabelOf(*def.TTLDays)
		}
	}
	return ttlLabelOf(instanceDefaultTTLDays(h.d.Cfg.DefaultTTLSeconds))
}

// utf16Len counts a string in UTF-16 code units, the unit a browser's
// maxlength counts in; Discord's reference does not say which it uses, so the
// pre-fill is held to the stricter of the two.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// uploadForm is the modal's `data`: a Text Display, then three Labels. Inside
// Discord's limits by construction: 4 of 5 components, a 22-character title,
// custom_ids of at most 40 characters, 6 options in a group that allows 10.
func uploadForm(nonce, filename, defaultLabel string) map[string]any {
	title := map[string]any{
		"type": componentTextInput, "custom_id": customFormTitle, "style": textInputShort,
		"max_length": maxTitleRunes, "required": false,
	}
	// The pre-fill only when it fits the input's own limit however Discord
	// counts it: a modal Discord rejects leaves the command unanswered, and then
	// the link could never be posted. Blank means "unchanged" — the clip keeps
	// the same file-name title anyway.
	if v := defaultTitle(filename); utf16Len(v) <= maxTitleRunes {
		title["value"] = v
	}
	ttl := make([]choice, 0, len(defaultsTTLChoices)+1)
	ttl = append(ttl, choice{ttlFollowInstance, formDefaultPrefix + defaultLabel})
	for _, c := range defaultsTTLChoices {
		ttl = append(ttl, choice{c.Value, c.Label})
	}
	return map[string]any{
		"custom_id": formPrefix + nonce,
		"title":     formModalTitle,
		"components": []any{
			map[string]any{"type": componentTextDisplay, "content": formHint},
			map[string]any{"type": componentLabel, "label": formLabelTitle, "component": title},
			map[string]any{"type": componentLabel, "label": labelTTL, "component": radioInput(customFormTTL, ttl, ttlFollowInstance)},
			map[string]any{"type": componentLabel, "label": formLabelTags, "description": formTagsDescription, "component": map[string]any{
				"type": componentTextInput, "custom_id": customFormTags, "style": textInputShort,
				"max_length": formTagsMaxChars, "required": false,
			}},
		},
	}
}

// openUploadForm answers `/upload` with the form and starts the job behind it,
// or reports false (the table is full) so the caller answers the old way.
func (h *handler) openUploadForm(w http.ResponseWriter, r *http.Request, in discordInteraction, binding db.GuildBinding, att discordAttachment) bool {
	label := h.formDefaultLabel(r.Context(), binding.UserID)
	nonce, ok := h.forms.open(in.GuildID, h.now())
	if !ok {
		h.d.Log.Warn("discord bot: upload form table full; answered without the form")
		return false
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"type": callbackModal,
		"data": uploadForm(nonce, att.Filename, label),
	})
	forms := h.forms
	h.d.Bot.Clip(notify.ClipJob{
		Token: in.Token, URL: att.URL, Filename: att.Filename,
		ContentType: att.ContentType, Size: att.Size, GuildID: in.GuildID,
		Ingest: h.botIngestWith(binding.UserID, func(ctx context.Context, clipID string, existing bool) bool {
			return h.applyHeldForm(ctx, nonce, clipID, existing)
		}),
		Command:   jobCommand(in.Data),
		Followup:  true,
		Form:      true, // a duplicate that dropped a held submit posts "not applied" (§20 erratum 326)
		Uploading: true, // the private "Uploading…" the modal's silence needs (§20 erratum 325)
		Failed:    func() { forms.fail(nonce) },
	})
	return true
}

// applyHeldForm is the ingest's hook, right after the clip exists: it attaches
// the clip and writes a submit that arrived first.
//
// A duplicate (§20 erratum 326) writes NOTHING: the clip was the account's
// before this upload, and a second person's title, lifetime and tags must not
// rewrite it. The entry is marked so a submit that arrives later is answered
// SentenceFormNotApplied and not written; a submit already held is dropped,
// and only then is the answer true (the job tells that person).
func (h *handler) applyHeldForm(ctx context.Context, nonce, clipID string, existing bool) bool {
	if existing {
		return h.forms.existing(nonce)
	}
	p := h.forms.attach(nonce, clipID)
	if p == nil {
		return false
	}
	if _, err := h.d.DB.UpdateClipMeta(ctx, clipID, *p, h.unixNow()); err != nil {
		h.d.Log.Warn("discord bot: upload form not applied", "clip", clipID, "err", err)
	}
	return false
}

// formTagsFault says why the tags were left off.
type formTagsFault int

const (
	formTagsOK formTagsFault = iota
	formTagsGrammar
	formTagsTooMany
)

// formPatch reads the submit. A title or a delete-after outside its rules is
// a refusal (both reachable only by a forged body: Discord enforces the
// input's max_length and the radio's closed set); bad tags are not — the rest
// is still applied and the tags are left as they are.
func formPatch(comps []discordModalComponent) (patch db.ClipPatch, refusal string, tags formTagsFault) {
	leaves := modalLeaves(comps)

	title, ok := textValue(leaves[customFormTitle])
	if !ok {
		return db.ClipPatch{}, msgFormTitleRefused, formTagsOK
	}
	if strings.TrimSpace(title) != "" {
		clean, err := cleanText(title, maxTitleRunes)
		if err != nil {
			return db.ClipPatch{}, msgFormTitleRefused, formTagsOK
		}
		patch.Title = &clean
	}

	ttl, ok := singleChoice(leaves[customFormTTL])
	if !ok {
		return db.ClipPatch{}, msgNotAChoice, formTagsOK
	}
	if ttl != ttlFollowInstance {
		legal := false
		for _, c := range defaultsTTLChoices {
			legal = legal || c.Value == ttl
		}
		if !legal {
			return db.ClipPatch{}, msgNotAChoice, formTagsOK
		}
		days, _ := strconv.Atoi(ttl)
		secs, err := config.ParseTTLDays(&days, nil)
		if err != nil {
			return db.ClipPatch{}, msgNotAChoice, formTagsOK
		}
		patch.TTLSeconds = &secs
	}

	raw, ok := textValue(leaves[customFormTags])
	if !ok {
		return patch, "", formTagsGrammar
	}
	if parts := db.SplitTags(raw); len(parts) > 0 {
		clean, err := db.CleanTags(parts)
		var many *db.TooManyTagsError
		switch {
		case errors.As(err, &many):
			return patch, "", formTagsTooMany
		case err != nil:
			return patch, "", formTagsGrammar
		}
		patch.Tags = &clean
	}
	return patch, "", formTagsOK
}

// textValue reads a Text Input's string `value`. An input that is absent reads
// as blank (the inputs are not required); one of another type, or a value that
// is not a string, is not ok.
func textValue(c discordModalComponent) (string, bool) {
	if c.Type == 0 && c.CustomID == "" {
		return "", true
	}
	if c.Type != componentTextInput {
		return "", false
	}
	if len(c.Value) == 0 || string(c.Value) == "null" {
		return "", true
	}
	var s string
	if err := json.Unmarshal(c.Value, &s); err != nil {
		return "", false
	}
	return s, true
}

// discordFormSubmit answers the form's MODAL_SUBMIT. No permission gate: the
// nonce is the credential (single use, fifteen minutes, this server only).
// Every answer is ephemeral, and none of them repeats what was typed.
func (h *handler) discordFormSubmit(w http.ResponseWriter, r *http.Request, in discordInteraction) {
	httpx.SetLogField(r, "discord_prefix", formPrefix)
	if _, ok := h.guildBinding(w, r, in, h.d.Cfg.DiscordBot.UploadCommandName()); !ok {
		return
	}
	nonce := strings.TrimPrefix(in.Data.CustomID, formPrefix)
	if !validNonce(nonce) {
		httpx.SetLogField(r, "form", "expired")
		writeEphemeral(w, msgFormExpired)
		return
	}
	patch, refusal, tagsFault := formPatch(in.Data.Components)
	outcome, clipID := h.forms.take(nonce, in.GuildID, h.now(), refusal == "", patch)
	switch outcome {
	case formExpired:
		httpx.SetLogField(r, "form", "expired")
		writeEphemeral(w, msgFormExpired)
		return
	case formFailed:
		httpx.SetLogField(r, "form", "failed")
		writeEphemeral(w, notify.SentenceFailed)
		return
	case formRefused:
		httpx.SetLogField(r, "form", "refused")
		writeEphemeral(w, refusal)
		return
	case formNotApplied:
		// The duplicate (§20 erratum 326): answered, never written - and so
		// never redrawn.
		httpx.SetLogField(r, "form", "not_applied")
		writeEphemeral(w, notify.SentenceFormNotApplied)
		return
	case formWrite:
		// The title the card may already show, read before the write: a redraw
		// is asked for only when the form CHANGED it (§20 erratum 325).
		before, readErr := h.d.DB.GetClip(r.Context(), clipID)
		if _, err := h.d.DB.UpdateClipMeta(r.Context(), clipID, patch, h.unixNow()); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				httpx.SetLogField(r, "form", "refused")
				writeEphemeral(w, msgLinkGone)
				return
			}
			h.d.Log.Warn("discord bot: upload form not applied", "clip", clipID, "err", err)
			httpx.SetLogField(r, "form", "refused")
			writeEphemeral(w, msgUnavailable)
			return
		}
		if patch.Title != nil && readErr == nil && *patch.Title != before.Title && h.d.Bot != nil {
			// The clip existed before this submit, so its link may be in the
			// channel already, drawn with the old title. The bot redraws it if it
			// posted it (never blocking; the answer below is unchanged). A held
			// submit (formHeld) never gets here: attach writes it before the link
			// is posted, so that card is drawn right the first time.
			h.d.Bot.Redraw(clipID)
		}
	case formHeld:
		// attach writes it when the clip exists.
	}
	switch tagsFault {
	case formTagsGrammar:
		httpx.SetLogField(r, "form", "saved_without_tags")
		writeEphemeral(w, msgFormTagsGrammar)
	case formTagsTooMany:
		httpx.SetLogField(r, "form", "saved_without_tags")
		writeEphemeral(w, msgFormTagsTooMany)
	default:
		httpx.SetLogField(r, "form", "saved")
		writeEphemeral(w, msgSaved)
	}
}
