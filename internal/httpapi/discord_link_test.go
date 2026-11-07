package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/notify"
	"clippet/internal/urls"
)

// envURLs is the Builder newEnv gives the handler: the same origin, the same
// build id, so a message built here is the message the endpoint builds.
func envURLs(t *testing.T) urls.Builder {
	t.Helper()
	base, err := url.Parse(testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	return urls.New(base, "test")
}

// `/clip link:` and the message command "Enrich" (§20 erratum 315 and its
// addendum): both answer at once, on the interaction response, with the SAME
// enhanced message `/upload`'s follow-up ends in — public, never deferred, and
// never a fetch.

// linkBody is a `/clip link:` interaction (a STRING option named `link`).
func linkBody(guildID, name, link string) string {
	body, _ := json.Marshal(map[string]any{
		"type": interactionCommand, "token": "int-token-abc", "guild_id": guildID,
		"data": map[string]any{
			"name": name, "type": 1,
			"options": []any{map[string]any{"name": "link", "type": optionTypeString, "value": link}},
		},
	})
	return string(body)
}

// enrichBody is a message-command interaction on one message whose text is
// `content` (Discord's shape: data.type 3, data.target_id, the message under
// data.resolved.messages).
func enrichBody(guildID, name, content string) string {
	body, _ := json.Marshal(map[string]any{
		"type": interactionCommand, "token": "int-token-abc", "guild_id": guildID,
		"data": map[string]any{
			"name": name, "type": commandTypeMessage, "target_id": "1500000000000000001",
			"resolved": map[string]any{"messages": map[string]any{
				"1500000000000000001": map[string]any{
					"id": "1500000000000000001", "content": content, "embeds": []any{}, "attachments": []any{},
				},
			}},
		},
	})
	return string(body)
}

// assertPosted checks a PUBLIC type-4 answer carrying the message for one clip:
// its share link alone, with no embed of our own (§20 erratum 316 — the page
// declares the card Discord draws under it).
func assertPosted(t *testing.T, b *botEnv, r resp, c db.Clip) {
	t.Helper()
	if r.code != http.StatusOK {
		t.Fatalf("= %d, want 200: %s", r.code, r.body)
	}
	if got, _ := r.json["type"].(float64); int(got) != callbackMessage {
		t.Fatalf("type %v, want %d (an immediate message — no deferral): %s", r.json["type"], callbackMessage, r.body)
	}
	data, ok := r.json["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data: %s", r.body)
	}
	if _, eph := data["flags"]; eph {
		t.Fatalf("the message carries flags (it must be public, and never components v2): %s", r.body)
	}
	u := envURLs(t)
	if want := u.Page(c.ID); data["content"] != want {
		t.Fatalf("content = %v, want the share link %q alone", data["content"], want)
	}
	// Byte for byte the builder's output: the two commands and the upload
	// follow-up can never post two different messages about one clip.
	want, _ := json.Marshal(notify.ClipMessage(u, c.ID))
	got, _ := json.Marshal(data)
	var wv, gv any
	_ = json.Unmarshal(want, &wv)
	_ = json.Unmarshal(got, &gv)
	if fmt.Sprint(wv) != fmt.Sprint(gv) {
		t.Fatalf("the answer is\n%s\nwant ClipMessage's\n%s", got, want)
	}
	for _, k := range []string{"embeds", "components"} {
		if _, has := data[k]; has {
			t.Fatalf("the message carries %q (it is the link alone): %s", k, r.body)
		}
	}
	if b.bot.count() != 0 {
		t.Fatalf("a link handed a job to the bot (nothing is uploaded and nothing is fetched)")
	}
}

func TestClipLinkPostsTheMessageForEverySpellingOfOurLink(t *testing.T) {
	b := newBotEnv(t)
	const guild = "555555555555555555"
	u := b.boundGuild(t, "linkowner", guild)
	clip, _, _ := b.readyClip(u, clipOpts{title: "arc"})
	for _, raw := range []string{
		testOrigin + "/" + clip.ID,             // the address
		testOrigin + "/clips/" + clip.ID,       // the legacy address
		testOrigin + "/embed/" + clip.ID,       // the player (erratum 275(b))
		"<" + testOrigin + "/" + clip.ID + ">", // Discord's no-unfurl wrap
		"||" + testOrigin + "/" + clip.ID + "||",
		"localhost:8481/" + clip.ID, // the scheme-less form a person reads
	} {
		t.Run(raw, func(t *testing.T) {
			assertPosted(t, b, b.post(linkBody(guild, config.DefaultCommandName, raw)), clip)
		})
	}
}

// TestTheRedrawnLinkResolvesToItsClip (§20 erratum 325): the upload form's
// late title edits the bot's own message to `{origin}/{id}?v=2`, and somebody
// will run `/clip link:` or Enrich on exactly that message. Both resolve it to
// the clip and answer with the BARE link.
func TestTheRedrawnLinkResolvesToItsClip(t *testing.T) {
	b := newBotEnv(t)
	const guild = "555555555555555556"
	u := b.boundGuild(t, "redrawlink", guild)
	clip, _, _ := b.readyClip(u, clipOpts{title: "arc"})
	redrawn := testOrigin + "/" + clip.ID + "?v=2"
	// …and `?v=3`, the second redraw of one link (§20 erratum 330).
	for _, raw := range []string{redrawn, "<" + redrawn + ">", "localhost:8481/" + clip.ID + "?v=2",
		testOrigin + "/" + clip.ID + "?v=3"} {
		t.Run("link "+raw, func(t *testing.T) {
			assertPosted(t, b, b.post(linkBody(guild, config.DefaultCommandName, raw)), clip)
		})
	}
	// The bot's edited message is the link alone; a person may quote it too.
	for _, content := range []string{redrawn, "this one " + redrawn + "."} {
		t.Run("enrich "+content, func(t *testing.T) {
			assertPosted(t, b, b.post(enrichBody(guild, config.DefaultEnrichCommandName, content)), clip)
		})
	}
}

// A queued, encoding or failed clip is posted too — somebody who pastes a link
// mid-encode wants the message now — and an unlisted one answers like a listed
// one (the link is the auth, as on its page).
func TestClipLinkPostsAClipInEveryLiveState(t *testing.T) {
	b := newBotEnv(t)
	const guild = "555555555555555555"
	u := b.boundGuild(t, "stateowner", guild)
	// Seeded in the order the job queue claims them: each helper claims the
	// next job, so the queued clip (never claimed) is seeded LAST.
	processing := b.processingClip(u, clipOpts{title: "p"})
	failed := b.failedClip(u, clipOpts{title: "f"})
	unlisted, _, _ := b.readyClip(u, clipOpts{title: "u", unlisted: true})
	queued := b.getClip(b.seedClip(u, clipOpts{title: "q"}).ID)
	for _, c := range []db.Clip{queued, processing, failed, unlisted} {
		t.Run(c.State+"/"+c.Title, func(t *testing.T) {
			assertPosted(t, b, b.post(linkBody(guild, config.DefaultCommandName, testOrigin+"/"+c.ID)), c)
		})
	}
}

// TestClipLinkHoldsAReplyWhoseCardIsNotReady (§20 erratum 330): `/clip link:`
// and Enrich answer on the interaction's own response, and Discord reads the
// page right after it, once. A clip whose card is not ready yet is HELD with the
// bot (the token and the clip), so the reply is redrawn once the worker says the
// card is; a card-ready clip is never held, and neither is a refusal.
func TestClipLinkHoldsAReplyWhoseCardIsNotReady(t *testing.T) {
	b := newBotEnv(t)
	const guild = "555555555555555557"
	u := b.boundGuild(t, "holdowner", guild)
	ready, _, _ := b.readyClip(u, clipOpts{title: "r"})
	queued := b.getClip(b.seedClip(u, clipOpts{title: "q"}).ID)
	if !db.CardReady(ready) || db.CardReady(queued) {
		t.Fatal("premise: one card-ready clip and one that is not")
	}
	assertPosted(t, b, b.post(linkBody(guild, config.DefaultCommandName, testOrigin+"/"+ready.ID)), ready)
	assertPosted(t, b, b.post(enrichBody(guild, config.DefaultEnrichCommandName, testOrigin+"/"+ready.ID)), ready)
	assertEphemeral(t, b.post(linkBody(guild, config.DefaultCommandName, testOrigin+"/AbcDefGhiJkl")), msgLinkNotOurs)
	if got := b.bot.held(); len(got) != 0 {
		t.Fatalf("a card-ready clip or a refusal was held: %v", got)
	}
	assertPosted(t, b, b.post(linkBody(guild, config.DefaultCommandName, testOrigin+"/"+queued.ID)), queued)
	assertPosted(t, b, b.post(enrichBody(guild, config.DefaultEnrichCommandName, "see "+testOrigin+"/"+queued.ID)), queued)
	want := "int-token-abc " + queued.ID
	if got := b.bot.held(); len(got) != 2 || got[0] != want || got[1] != want {
		t.Fatalf("held %v, want the queued clip's reply twice (%q)", got, want)
	}
}

func TestClipLinkRefusals(t *testing.T) {
	b := newBotEnv(t)
	const guild = "555555555555555555"
	u := b.boundGuild(t, "refusalowner", guild)
	gone := b.deletingClip(u)
	expired, _, _ := b.readyClip(u, clipOpts{title: "old"})
	if err := b.db.ForceExpire(context.Background(), expired.ID, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, link, want string }{
		{"another host", "https://clips.elsewhere.test/AbcDefGhiJkl", msgLinkNotOurs},
		{"a lookalike host", "http://localhost:8481.evil.test/AbcDefGhiJkl", msgLinkNotOurs},
		{"a route that is not a clip", testOrigin + "/c/AbcDefGhiJkl", msgLinkNotOurs},
		{"a segment that is not an id", testOrigin + "/settings", msgLinkNotOurs},
		{"an id nobody minted", testOrigin + "/AbcDefGhiJkl", msgLinkNotOurs},
		{"not a link at all", "hello", msgLinkNotOurs},
		{"blank", "   ", msgLinkNotOurs},
		{"a tombstone", testOrigin + "/" + gone.ID, msgLinkGone},
		{"a clip past its expiry", testOrigin + "/" + expired.ID, msgLinkGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertEphemeral(t, b.post(linkBody(guild, config.DefaultCommandName, tc.link)), tc.want)
		})
	}
	// No option at all.
	assertEphemeral(t, b.post(`{"type":2,"token":"t","guild_id":"`+guild+`","data":{"name":"clip"}}`), msgLinkNotOurs)
	// An unbound server and a DM are refused before the link is read.
	clip, _, _ := b.readyClip(u, clipOpts{title: "x"})
	assertEphemeral(t, b.post(linkBody("111111111111111111", config.DefaultCommandName, testOrigin+"/"+clip.ID)), msgUnbound)
	assertEphemeral(t, b.post(linkBody("", config.DefaultCommandName, testOrigin+"/"+clip.ID)), msgNotInGuild(config.DefaultCommandName))
	if b.bot.count() != 0 {
		t.Fatalf("%d job(s) handed over for refused links", b.bot.count())
	}
}

// The command every server held before this build was `/clip file:`; until a
// server's commands are re-published its picker still offers that. A `/clip`
// carrying an attachment and no link is therefore an UPLOAD, never "that is
// not a link" said to somebody who attached a video.
func TestClipWithAnAttachmentFromAnOldDefinitionUploads(t *testing.T) {
	b := newBotEnv(t)
	const guild = "666666666666666666"
	b.boundGuild(t, "compatowner", guild)
	r := b.post(renameCommand(t, clipBody(guild, "clip.mp4", "video/mp4", "https://cdn.discordapp.com/x/clip.mp4", 10), config.DefaultCommandName))
	if got, _ := r.json["type"].(float64); int(got) != callbackDeferredMessage {
		t.Fatalf("an old /clip file: answered %s, want the deferral", r.body)
	}
	if job, ok := b.bot.last(); !ok || job.Ingest == nil || job.URL != "https://cdn.discordapp.com/x/clip.mp4" {
		t.Fatal("no upload job was handed over")
	}
}

// The two commands are routed by NAME, under whatever names this instance was
// given: the renamed link command answers, the default names do not.
func TestClipLinkAnswersItsConfiguredName(t *testing.T) {
	b := newBotEnvWith(t, map[string]string{config.EnvDiscordCommandName: "clip-dev", config.EnvDiscordUploadCommandName: "upload-dev"})
	const guild = "777777777777777777"
	u := b.boundGuild(t, "renamedowner", guild)
	clip, _, _ := b.readyClip(u, clipOpts{title: "arc"})
	assertPosted(t, b, b.post(linkBody(guild, "clip-dev", testOrigin+"/"+clip.ID)), clip)
	assertEphemeral(t, b.post(linkBody(guild, config.DefaultCommandName, testOrigin+"/"+clip.ID)), msgUnknown)
	// The upload command's name with a link option is an upload with no file.
	assertEphemeral(t, b.post(linkBody(guild, "upload-dev", testOrigin+"/"+clip.ID)), msgNoInput("upload-dev"))
}

// A MANAGED instance (the official bot's forwarder relays Discord's bytes)
// answers all three commands under their default names exactly as an instance
// with its own application does.
func TestManagedInstanceAnswersAllThreeCommands(t *testing.T) {
	b := newBotEnvWith(t, map[string]string{config.EnvDiscordBotToken: config.ManagedToken})
	if !b.cfg.DiscordBot.Managed() {
		t.Fatal("the env is not managed")
	}
	const guild = "888888888888888888"
	u := b.boundGuild(t, "managedowner", guild)
	clip, _, _ := b.readyClip(u, clipOpts{title: "arc"})
	assertPosted(t, b, b.post(linkBody(guild, config.DefaultCommandName, testOrigin+"/"+clip.ID)), clip)
	assertPosted(t, b, b.post(enrichBody(guild, config.DefaultEnrichCommandName, "look "+testOrigin+"/"+clip.ID)), clip)
	r := b.post(clipBody(guild, "clip.mp4", "video/mp4", "https://cdn.discordapp.com/x/clip.mp4", 10))
	if got, _ := r.json["type"].(float64); int(got) != callbackDeferredMessage {
		t.Fatalf("a managed /upload answered %s, want the deferral", r.body)
	}
}

// --- Enrich (§20 erratum 315's addendum) ------------------------------------

func TestEnrichPostsTheFirstLinkOfOursInTheMessage(t *testing.T) {
	b := newBotEnv(t)
	const guild = "999999999999999991"
	u := b.boundGuild(t, "enrichowner", guild)
	clip, _, _ := b.readyClip(u, clipOpts{title: "arc"})
	other, _, _ := b.readyClip(u, clipOpts{title: "second"})
	link := testOrigin + "/" + clip.ID
	for _, tc := range []struct{ name, content string }{
		{"a bare link", link},
		{"a link in a sentence", "look at this " + link + " gg"},
		{"a link that ends the sentence", "look at this: " + link + "."},
		{"a wrapped link", "no preview: <" + link + ">"},
		{"a spoilered link", "||" + link + "||"},
		{"a masked link", "[watch](" + link + ")"},
		{"another host first, then ours", "https://elsewhere.test/AbcDefGhiJkl and " + link},
		{"an unminted id of ours first, then ours", testOrigin + "/AbcDefGhiJkl " + link},
		{"ours twice: the FIRST wins", link + " " + testOrigin + "/" + other.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The content is the FIRST clip's link (assertPosted checks it).
			assertPosted(t, b, b.post(enrichBody(guild, config.DefaultEnrichCommandName, tc.content)), clip)
		})
	}
}

func TestEnrichRefusals(t *testing.T) {
	b := newBotEnv(t)
	const guild = "999999999999999992"
	u := b.boundGuild(t, "enrichrefused", guild)
	gone := b.deletingClip(u)
	for _, tc := range []struct{ name, content, want string }{
		{"no link at all", "gg wp", msgEnrichNoLink},
		{"an empty message (an image alone)", "", msgEnrichNoLink},
		{"a link of another host", "https://clips.elsewhere.test/AbcDefGhiJkl", msgEnrichNoLink},
		{"an id nobody minted", testOrigin + "/AbcDefGhiJkl", msgEnrichNoLink},
		{"a tombstone", "rip " + testOrigin + "/" + gone.ID, msgLinkGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertEphemeral(t, b.post(enrichBody(guild, config.DefaultEnrichCommandName, tc.content)), tc.want)
		})
	}
	// The picked message missing from `resolved` (not Discord's shape, but a body
	// is a body) is the same line.
	body := `{"type":2,"token":"t","guild_id":"` + guild + `","data":{"name":"Enrich","type":3,"target_id":"1"}}`
	assertEphemeral(t, b.post(body), msgEnrichNoLink)
	// An unbound server is refused before the message is read.
	assertEphemeral(t, b.post(enrichBody("111111111111111111", config.DefaultEnrichCommandName, testOrigin+"/x")), msgUnbound)
	// A message command is routed by TYPE: a slash command of the same name is
	// not Enrich, and a message command of another name is unknown.
	assertEphemeral(t, b.post(`{"type":2,"token":"t","guild_id":"`+guild+`","data":{"name":"Enrich","type":1}}`), msgUnknown)
	assertEphemeral(t, b.post(enrichBody(guild, "clip", testOrigin+"/x")), msgUnknown)
}

// The message's text is read for the link and nothing of it is logged: the
// access log carries the command and the clip id it found, never the content.
func TestEnrichLogsTheClipAndNotTheMessage(t *testing.T) {
	b := newBotEnv(t)
	const guild = "999999999999999993"
	u := b.boundGuild(t, "enrichlog", guild)
	clip, _, _ := b.readyClip(u, clipOpts{title: "arc"})
	_, logs := b.postLogged(enrichBody(guild, config.DefaultEnrichCommandName, "secret words "+testOrigin+"/"+clip.ID))
	if !strings.Contains(logs, "discord=enrich") || !strings.Contains(logs, "clip="+clip.ID) {
		t.Fatalf("the access log lacks the command or the clip:\n%s", logs)
	}
	if strings.Contains(logs, "secret words") {
		t.Fatalf("the message's text reached the log:\n%s", logs)
	}
}
