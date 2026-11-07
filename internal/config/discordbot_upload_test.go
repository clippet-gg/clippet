package config

import (
	"strings"
	"testing"
)

// DISCORD_UPLOAD_COMMAND_NAME (§20 erratum 315): the bot publishes TWO commands
// — `/upload file:` and `/clip link:` — and the upload command's name is the
// twin of DISCORD_COMMAND_NAME: its own default, the same shape rule, the same
// orphan rule, the same managed rule. And the two must differ, because one
// Discord server holds one command per name.

func TestUploadCommandNameDefaultsToUpload(t *testing.T) {
	c := mustLoad(t, botEnv(t))
	if got := c.DiscordBot.UploadCommandName(); got != DefaultUploadCommandName {
		t.Fatalf("UploadCommandName() = %q with %s unset, want %q", got, EnvDiscordUploadCommandName, DefaultUploadCommandName)
	}
	if got := c.DiscordBot.CommandName(); got != DefaultCommandName {
		t.Fatalf("the link command moved off its default: %q", got)
	}
	if got := mustLoad(t, nil).DiscordBot.UploadCommandName(); got != "" {
		t.Fatalf("an unconfigured bot names an upload command: %q", got)
	}
}

func TestUploadCommandNameIsASetting(t *testing.T) {
	m := botEnv(t)
	m[EnvDiscordUploadCommandName] = "upload-dev"
	m[EnvDiscordCommandName] = "clip-dev"
	c := mustLoad(t, m)
	if c.DiscordBot.UploadCommandName() != "upload-dev" || c.DiscordBot.CommandName() != "clip-dev" {
		t.Fatalf("the two names = %q, %q", c.DiscordBot.UploadCommandName(), c.DiscordBot.CommandName())
	}
}

func TestUploadCommandNameRejectsWhatDiscordRejects(t *testing.T) {
	for _, name := range []string{"Upload", "up load", "/upload", "upload!", strings.Repeat("u", maxCommandNameChars+1)} {
		m := botEnv(t)
		m[EnvDiscordUploadCommandName] = name
		err := loadErr(t, m)
		if !strings.Contains(err.Error(), EnvDiscordUploadCommandName) || !strings.Contains(err.Error(), "lowercase") {
			t.Fatalf("%q was refused without naming the variable and the rule: %v", name, err)
		}
		if strings.Contains(err.Error(), testBotToken) {
			t.Fatalf("the rejection echoed the bot token: %v", err)
		}
	}
	m := botEnv(t)
	m[EnvDiscordUploadCommandName] = "upload "
	if err := loadErr(t, m); !strings.Contains(err.Error(), "whitespace") || !strings.Contains(err.Error(), EnvDiscordUploadCommandName) {
		t.Fatalf("a trailing space = %v, want the whitespace rule naming the variable", err)
	}
}

func TestUploadCommandNameWithTheBotOffIsABootError(t *testing.T) {
	err := loadErr(t, map[string]string{EnvDiscordUploadCommandName: "upload"})
	for _, want := range []string{EnvDiscordUploadCommandName, EnvDiscordAppID, EnvDiscordPublicKey, EnvDiscordBotToken, "means nothing with the bot off"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the orphan error does not name %s: %v", want, err)
		}
	}
	// It names only the variable that was set: an operator who set the upload
	// name alone is not told about the link name.
	if strings.Contains(err.Error(), EnvDiscordCommandName+":") || strings.Contains(err.Error(), EnvDiscordCommandName+" ") {
		t.Fatalf("the orphan error names DISCORD_COMMAND_NAME, which was not set: %v", err)
	}
}

// The two names must differ, and the refusal names BOTH variables and the name
// they share. The sentence is the one an operator reads in `docker compose logs`.
func TestTheTwoCommandNamesMustDiffer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		upload, link string
		shared       string
	}{
		{"the upload command named like the link default", "clip", "", "clip"},
		{"the link command named like the upload default", "", "upload", "upload"},
		{"both renamed to one word", "dev", "dev", "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := botEnv(t)
			if tc.upload != "" {
				m[EnvDiscordUploadCommandName] = tc.upload
			}
			if tc.link != "" {
				m[EnvDiscordCommandName] = tc.link
			}
			err := loadErr(t, m)
			for _, want := range []string{EnvDiscordUploadCommandName, EnvDiscordCommandName, "must differ", "/" + tc.shared, "§30.2"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the collision refusal does not carry %q: %v", want, err)
				}
			}
		})
	}
	// A name that is REFUSED falls back to its default, and that fallback is
	// not a collision anybody typed: only the shape rule is reported.
	m := botEnv(t)
	m[EnvDiscordCommandName] = "Bad!"
	m[EnvDiscordUploadCommandName] = "clip"
	err := loadErr(t, m)
	if !strings.Contains(err.Error(), "lowercase") {
		t.Fatalf("the bad link name is not reported: %v", err)
	}
	if strings.Contains(err.Error(), "must differ") {
		t.Fatalf("a refused name's fallback was reported as a collision: %v", err)
	}
}

func TestUploadCommandNameOnAManagedInstanceMustBeTheOfficialOne(t *testing.T) {
	m := botEnv(t)
	m[EnvDiscordBotToken] = ManagedToken
	m[EnvDiscordUploadCommandName] = "upload-dev"
	err := loadErr(t, m)
	for _, want := range []string{EnvDiscordUploadCommandName, EnvDiscordBotToken, ManagedToken, `"` + DefaultUploadCommandName + `"`, "/" + DefaultUploadCommandName, "/" + DefaultCommandName, "§30.8"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the managed refusal does not name %s: %v", want, err)
		}
	}
	m[EnvDiscordUploadCommandName] = DefaultUploadCommandName
	c := mustLoad(t, m)
	if !c.DiscordBot.Managed() || c.DiscordBot.UploadCommandName() != DefaultUploadCommandName || c.DiscordBot.CommandName() != DefaultCommandName {
		t.Fatalf("managed with the defaults spelled gave managed=%v %q/%q", c.DiscordBot.Managed(), c.DiscordBot.UploadCommandName(), c.DiscordBot.CommandName())
	}
}

// DISCORD_ENRICH_COMMAND_NAME (§20 erratum 315's addendum): a MESSAGE command's
// name follows Discord's type-3 rule — mixed case and spaces allowed, 1-32
// characters — so only the length is checked, plus this loader's whitespace,
// orphan and managed rules. It may share a word with a chat command: Discord
// keeps the two kinds in separate namespaces.
func TestEnrichCommandName(t *testing.T) {
	if got := mustLoad(t, botEnv(t)).DiscordBot.EnrichCommandName(); got != DefaultEnrichCommandName {
		t.Fatalf("the default = %q, want %q", got, DefaultEnrichCommandName)
	}
	for _, name := range []string{"Enrich (dev)", "Post as card", "clip", strings.Repeat("E", maxCommandNameChars), "Ånrich ✨"} {
		m := botEnv(t)
		m[EnvDiscordEnrichCommandName] = name
		if got := mustLoad(t, m).DiscordBot.EnrichCommandName(); got != name {
			t.Fatalf("%q gave %q", name, got)
		}
	}
	m := botEnv(t)
	m[EnvDiscordEnrichCommandName] = strings.Repeat("E", maxCommandNameChars+1)
	if err := loadErr(t, m); !strings.Contains(err.Error(), EnvDiscordEnrichCommandName) || !strings.Contains(err.Error(), "1-32 characters") {
		t.Fatalf("a 33-character name = %v", err)
	}
	m[EnvDiscordEnrichCommandName] = "Enrich "
	if err := loadErr(t, m); !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("a trailing space = %v", err)
	}
	if err := loadErr(t, map[string]string{EnvDiscordEnrichCommandName: "Enrich"}); !strings.Contains(err.Error(), "means nothing with the bot off") {
		t.Fatalf("the orphan = %v", err)
	}
	m = botEnv(t)
	m[EnvDiscordBotToken] = ManagedToken
	m[EnvDiscordEnrichCommandName] = "Enrich (dev)"
	if err := loadErr(t, m); !strings.Contains(err.Error(), EnvDiscordEnrichCommandName) || !strings.Contains(err.Error(), "§30.8") {
		t.Fatalf("a renamed managed message command = %v", err)
	}
}
