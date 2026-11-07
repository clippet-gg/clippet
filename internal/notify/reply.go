package notify

import (
	"clippet/internal/urls"
)

// The bot's MESSAGE about a clip (ROADMAP #33, DESIGN.md §20 errata 268, 315,
// 316).
//
// "Clippet is thinking…" is Discord's own fixed text for a deferred reply: the
// message that replaces it is the only voice this feature has. Until erratum
// 268 it was the bare link; 268 made it two lines; erratum 315 made it the link
// plus one embed of our own — and Discord, measured three times on the dev
// server, never unfurls a link in a bot message that carries an embed, so the
// embed hid the playable card it was meant to sit above. Since erratum 316 the
// clip's PAGE declares its own branded card (a Discord component embed: the
// mark, the host, the title, the length, the playable video, the state's
// colour, an Open button), so the message is the share link ALONE again and
// Discord draws that card under it — the same card a person who pastes the link
// gets. One builder, ClipMessage, for all three: the follow-up `/upload` edits
// into place, and the reply `/clip link:` and Enrich post.
//
// No embed, no component and no flag: §15 stands — nothing the bot says is a
// CTA — and SUPPRESS_EMBEDS is never set, because the unfurl IS the card.

// Message is one bot message, in Discord's Message shape: what the follow-up
// edit PATCHes and what `/clip link:`'s type-4 response carries as `data`. No
// `flags` field at all — not ephemeral (the channel is meant to see it) and not
// IS_COMPONENTS_V2 (which would forbid `content`).
type Message struct {
	Content         string          `json:"content"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

// ClipMessage builds the message for one clip (§20 errata 315, 316): its share
// link and nothing else. It carries exactly ONE link, ours (erratum 268's rule),
// and no text anybody else wrote — the title is the page's to render now, inside
// the card Discord draws from it — so there is nothing to escape and nothing to
// cut.
func ClipMessage(u urls.Builder, clipID string) Message {
	return Message{
		Content:         u.Page(clipID),
		AllowedMentions: allowedMentions{Parse: []string{}},
	}
}

// reply is the follow-up's message for a successful `/upload`.
func (b *Bot) reply(res IngestResult) Message {
	return ClipMessage(b.urls, res.ClipID)
}
