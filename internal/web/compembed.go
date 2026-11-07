package web

import (
	"bytes"
	"encoding/json"
	"html/template"
	"strconv"
	"strings"

	"clippet/internal/db"
	"clippet/web/static"
)

// The share page's own Discord card: a "component embed" (DESIGN.md §20
// erratum 316, behind CARD_COMPONENTS).
//
// Discord's Link Previews documentation (its unmerged pull request #8606 on
// discord-api-docs) lets a PAGE declare the card Discord draws for a pasted link:
// one `<script id="discord:component-embed"
// type="application/vnd.discord.component-embed+json">` in <head>, holding one
// Container built from the same components an app's message uses, at most
// 3,000 bytes. When Discord cannot draw it, it draws the ordinary card from the
// Open Graph tags — which is why §9.1's inventory stays on the page untouched in
// both states of the switch: it IS the fallback.
//
// What a Clippet page declares is the card it already has, in the layout that
// format allows: the accent is web.CardColor (287(c)), a Section carries the
// host (287(a)), the title as the link to the page and the card's own
// description line (287(b)) beside the mark as a Thumbnail (306, applied from
// the icon every install serves — never redrawn), a Media Gallery plays the
// SAME social.mp4 og:video names (or shows the §9.1 placeholder short of
// ready), and one link Button reads "Open".

// Component types and the one button style the format allows (Discord's
// components reference).
const (
	ceActionRow    = 1
	ceButton       = 2
	ceSection      = 9
	ceTextDisplay  = 10
	ceThumbnail    = 11
	ceMediaGallery = 12
	ceContainer    = 17
	ceStyleLink    = 5
)

// ComponentEmbedMaxBytes is the format's payload limit: "a 3,000-byte limit.
// Escape sequences like < also count toward it at their full length." A
// payload past it is not emitted at all, so the page carries the ordinary card
// alone — the documented fallback — rather than one Discord refuses.
const ComponentEmbedMaxBytes = 3000

// componentEmbedOpen is the button's label (erratum 316), and the
// only string on this card that is not already on the page's other card.
const componentEmbedOpen = "Open"

// markIcon is the mark the card's Thumbnail shows: the striped clapper of
// erratum 306 at 192 pixels on its rounded ink tile, the PNG a phone's home
// screen already gets, applied from its one source (scripts/lib/mkicons.py); no
// `?v=`, the mark does not change with the build. A badged instance gets the
// same drawing with the GREY span — the rule the dev Discord application's icon
// and INSTANCE_BADGE (erratum 285) already follow: green span = production, grey
// span = any other instance (the owner's pick (b), 2026-10-05, after a card on the
// dev box showed the production logo instead of the dev one). The choice is
// static.MarkIcon192For, which an audio clip's poster takes too.
func markIcon(badge string) string {
	return "/static/" + static.MarkIcon192For(badge)
}

type ceMedia struct {
	URL string `json:"url"`
}

type ceComponent struct {
	Type        int           `json:"type"`
	AccentColor *int          `json:"accent_color,omitempty"`
	Components  []ceComponent `json:"components,omitempty"`
	Content     string        `json:"content,omitempty"`
	Accessory   *ceComponent  `json:"accessory,omitempty"`
	Media       *ceMedia      `json:"media,omitempty"`
	Items       []ceItem      `json:"items,omitempty"`
	Style       int           `json:"style,omitempty"`
	URL         string        `json:"url,omitempty"`
	Label       string        `json:"label,omitempty"`
}

type ceItem struct {
	Media ceMedia `json:"media"`
}

type componentEmbed struct {
	Component ceComponent `json:"component"`
}

// BuildComponentEmbed renders the clip's component embed as the exact bytes the
// page carries between the <script> tags, or "" when the payload would pass
// ComponentEmbedMaxBytes. It is pure: the row and the URL builder decide it.
//
// The JSON is encoded with `<`, `>` and `&` as \u escapes (encoding/json's own
// HTML escaping), so nothing a title holds can close the <script> element it
// sits in; html/template is handed it as template.HTML for that reason and only
// that one.
func BuildComponentEmbed(v ClipView, badge string) template.HTML {
	c, u := v.Clip, v.URLs
	page := u.Page(c.ID)

	var lines []string
	lines = append(lines, "-# "+discordText(siteNameOf(u.SiteName())))
	if t := titleLine(c.Title, page); t != "" {
		lines = append(lines, t)
	}
	media := u.Static(placeholderName)
	if c.State == db.ClipReady {
		d := cardDescription(c)
		if looksLikeALink(d) {
			// The owner's words carried a link: the line keeps the length and the
			// quality clause and loses only them.
			bare := c
			bare.Description = ""
			d = cardDescription(bare)
		}
		lines = append(lines, descriptionLine(d, page))
		media = u.Social(c.ID)
	} else {
		desc := statusProcessing
		if c.State == db.ClipFailed {
			desc = statusFailed
		}
		lines = append(lines, desc)
	}

	accent := cardColorInt(c.State)
	e := componentEmbed{Component: ceComponent{
		Type:        ceContainer,
		AccentColor: &accent,
		Components: []ceComponent{
			{
				Type:       ceSection,
				Components: []ceComponent{{Type: ceTextDisplay, Content: strings.Join(lines, "\n")}},
				Accessory:  &ceComponent{Type: ceThumbnail, Media: &ceMedia{URL: u.Abs(markIcon(badge))}},
			},
			{Type: ceMediaGallery, Items: []ceItem{{Media: ceMedia{URL: media}}}},
			{Type: ceActionRow, Components: []ceComponent{
				{Type: ceButton, Style: ceStyleLink, URL: page, Label: componentEmbedOpen},
			}},
		},
	}}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(e); err != nil {
		return ""
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	if len(out) > ComponentEmbedMaxBytes {
		return ""
	}
	return template.HTML(out)
}

// titleLine is the card's heading (§20 erratum 316, the owner's real-channel test).
//
// Inside a masked link's LABEL Discord does not consume a backslash escape: the
// file-name title `Arc_Raiders_2026…` rendered as `Arc\_Raiders\_2026…` on that
// card. And a label cannot be left unescaped either, because a `]` ends it, `*`
// `_` `~` `|` and a backtick format inside it, and `<@…>` is a mention: no
// backslash-free encoding keeps a label both faithful and inert. So the title is
// a LINK only when it holds no character that can act INSIDE a label
// (labelActives) — then the label is the title's own characters and there is
// nothing in it to smuggle — and otherwise it is a plain heading, escaped, where
// Discord does consume the escapes; the card still links the page twice (the
// description's "at this link" and the Open button). A title that reads as a
// link is dropped, as 315 drops it from the bot's embed.
//
// Amended by §20 erratum 317: the test used to be "no markdown active at all",
// the whole of discordMarkdownActives — which made every timestamp title
// (`2025-06-24 00:23`) a plain heading for its hyphens. A `-` and a `#` act only
// at the START of a line (a list item, a heading), and a label never starts one:
// it follows `### [`. So they are inert in a label and do not count here; the
// escape table for every text outside a label is unchanged.
func titleLine(title, page string) string {
	t := discordText(title)
	if t == "" || looksLikeALink(t) {
		return ""
	}
	if collapsed := strings.Join(strings.Fields(title), " "); !strings.ContainsAny(collapsed, labelActives) {
		return "### [" + collapsed + "](" + page + ")"
	}
	return "### " + t
}

// labelActives are the characters that can act inside a masked link's label
// (§20 erratum 317): the backslash, `* _ ~` and the backtick format, `|`
// spoilers, `< >` a mention or a custom emoji, `[ ] ( )` the link's own
// syntax, and `@` a mention's start. discordMarkdownActives without the two
// line-start actives `#` and `-`, plus `@`.
const labelActives = "\\*_~`|<>[]()@"

// descriptionLine is the card's description, escaped, with the quality clause's
// closing words — "at this link" — made the link they name (the owner's second
// finding). Only those words: the rest of the line is the og:description's own
// text, and the label holds no markdown-active character.
func descriptionLine(d, page string) string {
	if head, ok := strings.CutSuffix(d, " "+qualityLinkWords); ok {
		return discordText(head) + " [" + qualityLinkWords + "](" + page + ")"
	}
	return discordText(d)
}

// cardColorInt is web.CardColor's `#RRGGBB` as the integer a Container's
// `accent_color` takes — one source for the bar, the bot's embed and this.
func cardColorInt(state string) int {
	n, err := strconv.ParseInt(strings.TrimPrefix(CardColor(state), "#"), 16, 32)
	if err != nil {
		return 0
	}
	return int(n)
}

// discordMarkdownActives is the set erratum 268 escapes in a bot message
// (internal/notify's markdownActives), spelled again because §3 gives
// internal/web no import of internal/notify; TestDiscordTextEscapesLikeTheBot
// pins the two spellings to one table. The backslash is first: the escape IS a
// backslash.
const discordMarkdownActives = "\\*_~`|<>[]()#-"

// discordText renders stored text as one line of Discord markdown that shows
// what the page shows: whitespace collapsed to single spaces, every markdown
// active escaped with a backslash (internal/notify's plainTitle, the same rule).
func discordText(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	space := false
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			space = out.Len() > 0
			continue
		case space:
			out.WriteByte(' ')
			space = false
		}
		if r < 0x80 && strings.ContainsRune(discordMarkdownActives, r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// looksLikeALink is internal/notify's rule (erratum 268): a scheme separator or
// a `www.` anywhere. No backslash suppresses Discord's auto-linker, so such a
// line is dropped rather than printed as a second link on the card.
func looksLikeALink(s string) bool {
	return strings.Contains(s, "://") || strings.Contains(s, "www.")
}
