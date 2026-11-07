package notify

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"testing"

	"clippet/internal/urls"
)

// The bot's message about a clip (§20 errata 268, 315). Pure functions, so a
// table is most of the test; what the BOT does with the message is in
// bot_test.go, against httptest.

func testURLs(t *testing.T) urls.Builder {
	t.Helper()
	base, err := url.Parse("https://clips.example")
	if err != nil {
		t.Fatal(err)
	}
	return urls.New(base, "test")
}

// messageJSON is the message as Discord receives it: a generic map, so a key
// that should not exist can be seen to be absent.
func messageJSON(t *testing.T, m Message) map[string]any {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestClipMessageIsTheLinkAlone pins the message (§20 erratum 316, amending
// 315): `content` is the share link and nothing else, mentions are parsed for
// nobody, and there is no `embeds`, `components` or `flags` key at all — the
// clip's page declares the card Discord draws under it.
func TestClipMessageIsTheLinkAlone(t *testing.T) {
	v := messageJSON(t, ClipMessage(testURLs(t), "abc123XYZ789"))
	if got := keys(v); strings.Join(got, ",") != "allowed_mentions,content" {
		t.Fatalf("message keys %v, want exactly allowed_mentions,content", got)
	}
	if v["content"] != "https://clips.example/abc123XYZ789" {
		t.Fatalf("content = %v, want the share link alone", v["content"])
	}
	am, _ := v["allowed_mentions"].(map[string]any)
	if parse, ok := am["parse"].([]any); !ok || len(parse) != 0 {
		t.Fatalf("allowed_mentions = %v, want an empty parse list", v["allowed_mentions"])
	}
}

// TestUploadDescriptionNamesTheHostAndFits is the bigger-file pointer (A):
// Discord refuses a too-big attachment in the CLIENT, so the command picker is
// one of the only two places our words reach a person before that happens. It
// must name this instance — and it must never exceed Discord's hundred
// characters, because a description past the limit is a registration the API
// REJECTS, which would leave the server with no command at all.
func TestUploadDescriptionNamesTheHostAndFits(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		wantHost   bool
	}{
		{"a short host", "clippet.gg", true},
		{"a lane's host and port", "localhost:8500", true},
		{"the longest host that still fits", strings.Repeat("a", descMaxHost), true},
		{"one character too long", strings.Repeat("a", descMaxHost+1), false},
		{"a 40-character host", strings.Repeat("a", 36) + ".com", false},
		{"no host at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := uploadDescription(tc.host)
			if len(got) > maxDescription {
				t.Fatalf("the description is %d characters, past Discord's %d: %q", len(got), maxDescription, got)
			}
			if !strings.HasPrefix(got, "Upload a clip. You get a link that plays in Discord.") {
				t.Fatalf("the description does not open with the settled sentence: %q", got)
			}
			if named := tc.host != "" && strings.Contains(got, tc.host); named != tc.wantHost {
				t.Fatalf("the description names the host = %v, want %v: %q", named, tc.wantHost, got)
			}
			if !tc.wantHost && !strings.HasSuffix(got, " Bigger files go on the site.") {
				t.Fatalf("the fallback dropped the pointer as well as the host: %q", got)
			}
		})
	}
}

// TestTheTwoCommandsCarryOneRequiredOptionEach: `/upload` takes the video and
// nothing else (§20 erratum 286), `/clip` the link and nothing else (erratum
// 315), and both options are `required` — Discord then refuses an empty command
// in its own client instead of letting one reach the endpoint.
func TestTheTwoCommandsCarryOneRequiredOptionEach(t *testing.T) {
	f := newBotFixture(t, 200, "")
	for _, tc := range []struct {
		cmd               botCommandDef
		name, opt         string
		optType           int
		optDesc, cmdStart string
	}{
		{f.bot.uploadCommand(), botUploadCommand, botFileOption, optionTypeAttachment, "The video or audio to upload", "Upload a clip."},
		{f.bot.clipCommand(), botCommand, botLinkOption, optionTypeString, "A link to a clip on this Clippet", "Post a clip of this Clippet as a card."},
	} {
		if tc.cmd.Name != tc.name || tc.cmd.Type != commandTypeChatInput || !strings.HasPrefix(tc.cmd.Description, tc.cmdStart) {
			t.Fatalf("the command is %+v", tc.cmd)
		}
		if len(tc.cmd.Options) != 1 {
			t.Fatalf("/%s has %d options, want one", tc.name, len(tc.cmd.Options))
		}
		got := tc.cmd.Options[0]
		if got.Name != tc.opt || got.Type != tc.optType || !got.Required || got.Description != tc.optDesc {
			t.Errorf("/%s's option is %+v", tc.name, got)
		}
	}
}
