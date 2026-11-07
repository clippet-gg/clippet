package config

import (
	"strings"
	"testing"
)

// DISCORD_SETTINGS_COMMAND_NAME (§20 erratum 318): `/settings` is a chat
// command like `/upload` and `/clip`, held to the same default, shape, orphan
// and managed rules, and to the must-differ rule against both of them.

func TestSettingsCommandNameDefaultsToSettings(t *testing.T) {
	if got := mustLoad(t, botEnv(t)).DiscordBot.SettingsCommandName(); got != DefaultSettingsCommandName {
		t.Fatalf("SettingsCommandName() = %q with %s unset, want %q", got, EnvDiscordSettingsCommandName, DefaultSettingsCommandName)
	}
	if DefaultSettingsCommandName != "settings" {
		t.Fatalf("the default moved off the contract's word: %q", DefaultSettingsCommandName)
	}
	if got := mustLoad(t, nil).DiscordBot.SettingsCommandName(); got != "" {
		t.Fatalf("an unconfigured bot names a settings command: %q", got)
	}
}

func TestSettingsCommandNameIsASetting(t *testing.T) {
	m := botEnv(t)
	m[EnvDiscordSettingsCommandName] = "settings-dev"
	if got := mustLoad(t, m).DiscordBot.SettingsCommandName(); got != "settings-dev" {
		t.Fatalf("SettingsCommandName() = %q, want settings-dev", got)
	}
}

func TestSettingsCommandNameRejectsWhatDiscordRejects(t *testing.T) {
	for _, name := range []string{"Settings", "set tings", "/settings", strings.Repeat("s", maxCommandNameChars+1)} {
		m := botEnv(t)
		m[EnvDiscordSettingsCommandName] = name
		err := loadErr(t, m)
		if !strings.Contains(err.Error(), EnvDiscordSettingsCommandName) || !strings.Contains(err.Error(), "lowercase") {
			t.Fatalf("%q was refused without naming the variable and the rule: %v", name, err)
		}
	}
	m := botEnv(t)
	m[EnvDiscordSettingsCommandName] = "settings "
	if err := loadErr(t, m); !strings.Contains(err.Error(), "whitespace") || !strings.Contains(err.Error(), EnvDiscordSettingsCommandName) {
		t.Fatalf("a trailing space = %v, want the whitespace rule naming the variable", err)
	}
}

func TestSettingsCommandNameWithTheBotOffIsABootError(t *testing.T) {
	err := loadErr(t, map[string]string{EnvDiscordSettingsCommandName: "settings"})
	for _, want := range []string{EnvDiscordSettingsCommandName, "means nothing with the bot off"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the orphan error does not name %s: %v", want, err)
		}
	}
}

func TestSettingsCommandNameMustDifferFromTheOtherChatNames(t *testing.T) {
	for _, tc := range []struct {
		name, settings, other, otherEnv string
	}{
		{"named like the link default", "clip", "", EnvDiscordCommandName},
		{"named like the upload default", "upload", "", EnvDiscordUploadCommandName},
		{"the link command renamed to the settings default", "", "settings", EnvDiscordCommandName},
		{"the upload command renamed to the settings default", "", "settings", EnvDiscordUploadCommandName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := botEnv(t)
			if tc.settings != "" {
				m[EnvDiscordSettingsCommandName] = tc.settings
			}
			if tc.other != "" {
				m[tc.otherEnv] = tc.other
			}
			err := loadErr(t, m)
			for _, want := range []string{EnvDiscordSettingsCommandName, tc.otherEnv, "must differ", "§30.2"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the collision refusal does not carry %q: %v", want, err)
				}
			}
		})
	}
	// The message command is in its own namespace: `settings` there is legal.
	m := botEnv(t)
	m[EnvDiscordEnrichCommandName] = "settings"
	if got := mustLoad(t, m).DiscordBot.SettingsCommandName(); got != DefaultSettingsCommandName {
		t.Fatalf("a message command named like /settings moved it: %q", got)
	}
	// A refused settings name falls back to its default and is not a collision.
	m = botEnv(t)
	m[EnvDiscordSettingsCommandName] = "Bad!"
	m[EnvDiscordCommandName] = "settings"
	err := loadErr(t, m)
	if strings.Contains(err.Error(), "must differ") {
		t.Fatalf("a refused name's fallback was reported as a collision: %v", err)
	}
}

func TestSettingsCommandNameOnAManagedInstanceMustBeTheOfficialOne(t *testing.T) {
	m := botEnv(t)
	m[EnvDiscordBotToken] = ManagedToken
	m[EnvDiscordSettingsCommandName] = "settings-dev"
	err := loadErr(t, m)
	for _, want := range []string{EnvDiscordSettingsCommandName, EnvDiscordBotToken, ManagedToken, `"` + DefaultSettingsCommandName + `"`, "/" + DefaultSettingsCommandName, "§30.8"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the managed refusal does not name %s: %v", want, err)
		}
	}
	m[EnvDiscordSettingsCommandName] = DefaultSettingsCommandName
	c := mustLoad(t, m)
	if !c.DiscordBot.Managed() || c.DiscordBot.SettingsCommandName() != DefaultSettingsCommandName {
		t.Fatalf("managed with the default spelled gave managed=%v %q", c.DiscordBot.Managed(), c.DiscordBot.SettingsCommandName())
	}
}
