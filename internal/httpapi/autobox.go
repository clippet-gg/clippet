package httpapi

import (
	"context"
	"errors"

	"clippet/internal/autobot"
	"clippet/internal/db"
	"clippet/internal/notify"
)

// Clippet Auto on the box (§20 erratum 333, DEPLOY §30.10): the instance side
// internal/autobot is handed. Three answers, each one this package already
// gives elsewhere —
//
//   - Bound: the guild binding `admin bot bind` writes, the one the §30 bot
//     and POST /in/auto/{token} are gated on; Person, that row's scope.
//   - Applicable: POST /in/auto's own rule, in the order that route applies
//     it: the instruction's grammar, the attachment URL (config's CDN
//     allowlist, then the attachment path), a video or a sound, under
//     MAX_UPLOAD_BYTES.
//   - Upload: the SAME job that route runs (fetchAttachment: the bot's
//     fetcher, the declared size as the truth, the account's link defaults),
//     in-process, as the bound account, and charged to that account's upload
//     bucket — this door has no route limiter in front of it.

// autoUploadTag is what the fetch job's log lines name for this door (the HTTP
// door names its token row id).
const autoUploadTag = "gateway"

type autoHost struct{ h *handler }

func (x autoHost) Bound(ctx context.Context, guildID string) (bool, error) {
	if x.h.d.DB == nil {
		return false, nil
	}
	_, err := x.h.d.DB.GuildBinding(ctx, guildID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// Person is autobot.Scoped: the same binding row, its scope and the bound
// account's linked Discord id (§20 erratum 342) — on a person binding Auto
// acts only on that member's videos (erratum 333 amended 2026-10-19).
func (x autoHost) Person(ctx context.Context, guildID string) (bool, string, error) {
	if x.h.d.DB == nil {
		return false, "", errors.New("unavailable")
	}
	b, err := x.h.d.DB.GuildBinding(ctx, guildID)
	if err != nil {
		return false, "", err
	}
	return b.Person(), b.DiscordID, nil
}

// instruction is the attachment as POST /in/auto would have been told it.
func instruction(guildID string, a autobot.Attachment) autoFetchInstruction {
	return autoFetchInstruction{GuildID: guildID, URL: a.URL, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size}
}

func (x autoHost) Applicable(a autobot.Attachment) bool {
	// The grammar wants a guild id; any well-formed one stands in, since the
	// binding is checked by Bound and again by Upload.
	if !instruction("100000000000000000", a).Valid() {
		return false
	}
	if _, ok := discordAttachmentURL(a.URL); !ok {
		return false
	}
	return isMedia(discordAttachment{Filename: a.Filename, ContentType: a.ContentType}) &&
		a.Size <= x.h.d.Cfg.MaxUploadBytes && x.h.d.Bot != nil
}

// autoLine is an upload refusal carrying the sentence the bot's own paths say.
type autoLine string

func (e autoLine) Error() string { return string(e) }
func (e autoLine) Line() string  { return string(e) }

func (x autoHost) Upload(ctx context.Context, guildID string, a autobot.Attachment) (string, error) {
	h := x.h
	if h.d.Bot == nil || h.d.DB == nil {
		return "", errors.New("unavailable")
	}
	in := instruction(guildID, a)
	if !in.Valid() {
		return "", autoLine(notify.SentenceNotVideo)
	}
	u, ok := discordAttachmentURL(a.URL)
	if !ok {
		return "", autoLine(notify.SentenceFetch)
	}
	binding, err := h.d.DB.GuildBinding(ctx, guildID)
	if err != nil {
		return "", errors.New("not bound")
	}
	ch, err := h.fetchAttachment(binding.UserID, u, in, autoUploadTag, true, h.captureWait)
	if err != nil {
		return "", errors.New("restarting")
	}
	var out notify.FetchOutcome
	select {
	case out = <-ch:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if out.Err != nil {
		var ie *notify.IngestError
		if errors.As(out.Err, &ie) && ie.Sentence != "" {
			return "", autoLine(ie.Sentence)
		}
		return "", errors.New("failed")
	}
	return h.d.URLs.Page(out.Result.ClipID), nil
}
