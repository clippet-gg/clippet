package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"clippet/internal/urls"
)

// DISCORD_COMMAND_NAME on the OUTBOUND side (§20 erratum 284): whatever the
// operator named the command is what Register PUTs to Discord. This is the one
// place that fact is provable — the E2E suite reaches Discord for nothing, by
// design, so the registration body has no live witness anywhere else.

func TestRegisterPublishesTheConfiguredCommandName(t *testing.T) {
	const name, upload, enrich, settings = "clip-dev", "upload-dev", "Enrich (dev)", "settings-dev"
	f := newBotFixture(t, http.StatusOK, "")
	base, _ := url.Parse("https://clips.example")
	bot := NewBot(BotDeps{
		AppID: testAppID, Token: testBotToken, CommandName: name, UploadCommandName: upload, EnrichCommandName: enrich, SettingsCommandName: settings,
		URLs:        urls.New(base, "test"),
		APIHost:     strings.TrimPrefix(f.api.URL, "http://"),
		APIScheme:   "http",
		CallTimeout: 2 * time.Second,
	})
	if bot == nil {
		t.Fatal("NewBot returned nil for a configured application")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bot.Shutdown(ctx)
	})
	if got := bot.CommandName(); got != name {
		t.Fatalf("CommandName() = %q, want %q", got, name)
	}
	if got := bot.UploadCommandName(); got != upload {
		t.Fatalf("UploadCommandName() = %q, want %q", got, upload)
	}
	if err := bot.Register(context.Background(), testGuildID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 {
		t.Fatalf("Register made %d calls, want 1", len(f.calls))
	}
	var cmds []botCommandDef
	if err := json.Unmarshal(f.calls[0].body, &cmds); err != nil || len(cmds) != 5 {
		t.Fatalf("Register sent %s (%v), want the FIVE commands in one PUT", f.calls[0].body, err)
	}
	// The fifth is `/signin` under its FIXED name: no rename touches it (§20 erratum 341).
	if cmds[4].Name != botSigninCommand {
		t.Fatalf("Register published %q fifth, want /%s", cmds[4].Name, botSigninCommand)
	}
	// The fourth is `/settings` under its configured name (§20 erratum 318).
	if cmds[3].Name != settings || bot.SettingsCommandName() != settings {
		t.Fatalf("Register published %q as the settings command, want %q", cmds[3].Name, settings)
	}
	// ONE PUT carries all three, upload first (§20 erratum 315), each under its
	// configured name.
	if cmds[0].Name != upload || cmds[1].Name != name || cmds[2].Name != enrich || cmds[2].Type != commandTypeMessage {
		t.Fatalf("Register published %+v, want %q, %q and the message command %q", cmds, upload, name, enrich)
	}
	// Everything else about the commands is untouched by the rename: ONE required
	// option each, and the upload description still names this instance.
	if len(cmds[0].Options) != 1 || cmds[0].Options[0].Name != botFileOption || !cmds[0].Options[0].Required {
		t.Fatalf("the renamed upload command changed its options: %+v", cmds[0])
	}
	if len(cmds[1].Options) != 1 || cmds[1].Options[0].Name != botLinkOption || !cmds[1].Options[0].Required {
		t.Fatalf("the renamed link command changed its options: %+v", cmds[1])
	}
	if !strings.Contains(cmds[0].Description, "clips.example") || len(cmds[0].Description) > maxDescription {
		t.Fatalf("the renamed upload command's description is %q", cmds[0].Description)
	}
}

// TestBotCommandNameDefaults: an empty BotDeps.CommandName is the shipped
// instance — the default word, not an empty one. internal/config never hands
// over a blank for a configured bot, and this is what makes that safe rather
// than merely true today.
func TestBotCommandNameDefaults(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "")
	if got := f.bot.CommandName(); got != botCommand {
		t.Fatalf("CommandName() = %q with no CommandName configured, want %q", got, botCommand)
	}
	if got := f.bot.clipCommand().Name; got != botCommand {
		t.Fatalf("the default command is published as %q, want %q", got, botCommand)
	}
	if got := f.bot.UploadCommandName(); got != botUploadCommand {
		t.Fatalf("UploadCommandName() = %q with none configured, want %q", got, botUploadCommand)
	}
	if got := f.bot.uploadCommand().Name; got != botUploadCommand {
		t.Fatalf("the default upload command is published as %q, want %q", got, botUploadCommand)
	}
	if got := f.bot.enrichCommand().Name; got != botEnrichCommand || f.bot.EnrichCommandName() != botEnrichCommand {
		t.Fatalf("the default message command is published as %q, want %q", got, botEnrichCommand)
	}
	if got := f.bot.settingsCommand().Name; got != botSettingsCommand || f.bot.SettingsCommandName() != botSettingsCommand {
		t.Fatalf("the default settings command is published as %q, want %q", got, botSettingsCommand)
	}
}

// TestRestartSentenceNamesTheConfiguredCommand: the one channel-visible sentence
// that names the command outside internal/httpapi (a job cancelled by a restart)
// follows the rename too. The job is an UPLOAD, so since §20 erratum 315 it
// names the upload command — on a default instance, `/upload`.
func TestRestartSentenceNamesTheConfiguredCommand(t *testing.T) {
	if got := sentenceRestart("upload-dev"); got != "Clippet was restarting — run /upload-dev again." {
		t.Fatalf("renamed restart sentence = %q", got)
	}
	if SentenceRestart != "Clippet was restarting — run /upload again." {
		t.Fatalf("the default restart sentence = %q", SentenceRestart)
	}
	// …and a renamed bot's cancelled job is told the name IT publishes.
	base, _ := url.Parse("https://clips.example")
	bot := NewBot(BotDeps{AppID: testAppID, Token: testBotToken, UploadCommandName: "upload-dev",
		URLs: urls.New(base, "test"), APIHost: "discord.invalid"})
	t.Cleanup(func() { _ = bot.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := bot.sentenceFor(ctx, context.Canceled); got != "Clippet was restarting — run /upload-dev again." {
		t.Fatalf("a cancelled job on a renamed bot is told %q", got)
	}
}
