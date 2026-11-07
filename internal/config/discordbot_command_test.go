package config

import (
	"strings"
	"testing"
)

// DISCORD_COMMAND_NAME (§20 erratum 284): the slash command's name is a
// setting, its default is unchanged, Discord's own rule is enforced at boot,
// and the two shapes that could only fail silently — a name on an instance with
// no bot, and a renamed MANAGED instance — are boot errors.

func TestDiscordCommandNameDefaultsToClip(t *testing.T) {
	c := mustLoad(t, botEnv(t))
	if got := c.DiscordBot.CommandName(); got != DefaultCommandName {
		t.Fatalf("CommandName() = %q with %s unset, want %q", got, EnvDiscordCommandName, DefaultCommandName)
	}
	// The OFF state has no command at all — there is nothing to name, and no
	// caller reaches for one.
	if got := mustLoad(t, nil).DiscordBot.CommandName(); got != "" {
		t.Fatalf("an unconfigured bot names a command: %q", got)
	}
}

func TestDiscordCommandNameAcceptsWhatDiscordAccepts(t *testing.T) {
	for _, name := range []string{
		"clip", "clip-dev", "clip_dev", "c", "clip2", "2clip", "-", "_",
		strings.Repeat("c", maxCommandNameChars), // exactly 32
		"клип",                                   // a non-ASCII script, all lowercase
		"クリップ",                                   // a script with no case at all
	} {
		m := botEnv(t)
		m[EnvDiscordCommandName] = name
		c := mustLoad(t, m)
		if got := c.DiscordBot.CommandName(); got != name {
			t.Fatalf("%s=%q gave CommandName() %q", EnvDiscordCommandName, name, got)
		}
	}
}

func TestDiscordCommandNameRejectsWhatDiscordRejects(t *testing.T) {
	// Every one of these is a registration Discord refuses — which an operator
	// would otherwise meet minutes later, from the wrong side, as "the command
	// never appeared in the picker".
	for _, name := range []string{
		"Clip",      // uppercase
		"clipDev",   // uppercase in the middle
		"CLIP",      //
		"clip dev",  // a space
		"/clip",     // the slash, which is not part of the name
		"clip.dev",  // a dot
		"clip!",     // punctuation
		"clip:dev",  //
		"clip\tdev", // a tab
		strings.Repeat("c", maxCommandNameChars+1), // 33
		"ǅ", // a TITLECASE rune: neither upper nor lower, and refused all the same
	} {
		m := botEnv(t)
		m[EnvDiscordCommandName] = name
		err := loadErr(t, m)
		if !strings.Contains(err.Error(), EnvDiscordCommandName) {
			t.Fatalf("%q was refused without naming the variable: %v", name, err)
		}
		if !strings.Contains(err.Error(), "lowercase") {
			t.Fatalf("%q was refused without naming the rule: %v", name, err)
		}
		if strings.Contains(err.Error(), testBotToken) {
			t.Fatalf("the rejection echoed the bot token: %v", err)
		}
	}
	// A whitespace-only value reads as UNSET, exactly as it does for every
	// other variable this loader reads: the name falls back to the default
	// rather than becoming a refusal nobody meant to configure.
	m := botEnv(t)
	m[EnvDiscordCommandName] = "   "
	if got := mustLoad(t, m).DiscordBot.CommandName(); got != DefaultCommandName {
		t.Fatalf("a blank %s gave %q, want the default", EnvDiscordCommandName, got)
	}
}

func TestDiscordCommandNameWithTheBotOffIsABootError(t *testing.T) {
	err := loadErr(t, map[string]string{EnvDiscordCommandName: "clip-dev"})
	for _, want := range []string{EnvDiscordCommandName, EnvDiscordAppID, EnvDiscordPublicKey, EnvDiscordBotToken} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the orphan error does not name %s: %v", want, err)
		}
	}
	// A name that is perfectly legal is refused just the same: the fault is
	// that nothing would ever read it, not that it is malformed.
	if err := loadErr(t, map[string]string{EnvDiscordCommandName: DefaultCommandName}); err == nil ||
		!strings.Contains(err.Error(), EnvDiscordCommandName) {
		t.Fatalf("the DEFAULT name with the bot off = %v, want the same refusal", err)
	}
	// And with one of the three set it is the THREE-variable error that is
	// reported, not two errors about the same mistake: the operator's next
	// action is the missing pair.
	err = loadErr(t, map[string]string{EnvDiscordAppID: testAppID, EnvDiscordCommandName: "clip-dev"})
	if !strings.Contains(err.Error(), "all three") {
		t.Fatalf("one of three plus a command name = %v, want the all-three error", err)
	}
	if strings.Contains(err.Error(), "means nothing with the bot off") {
		t.Fatalf("a half-configured bot was also told its command name is an orphan: %v", err)
	}
}

func TestDiscordCommandNameOnAManagedInstanceMustBeTheOfficialOne(t *testing.T) {
	// The forwarder relays the official application's one global command. A
	// managed instance under any other name publishes nothing, is reached by
	// nothing, and answers "Clippet does not know that command" to the one
	// interaction that does arrive — silence with extra steps.
	m := botEnv(t)
	m[EnvDiscordBotToken] = ManagedToken
	m[EnvDiscordCommandName] = "clip-dev"
	err := loadErr(t, m)
	for _, want := range []string{EnvDiscordCommandName, EnvDiscordBotToken, ManagedToken, DefaultCommandName, "§30.8"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the managed refusal does not name %s: %v", want, err)
		}
	}
	// The default is fine, spelled or not.
	m[EnvDiscordCommandName] = DefaultCommandName
	if got := mustLoad(t, m).DiscordBot.CommandName(); got != DefaultCommandName {
		t.Fatalf("managed + the default name gave %q", got)
	}
	delete(m, EnvDiscordCommandName)
	c := mustLoad(t, m)
	if !c.DiscordBot.Managed() || c.DiscordBot.CommandName() != DefaultCommandName {
		t.Fatalf("managed with no name gave managed=%v name=%q", c.DiscordBot.Managed(), c.DiscordBot.CommandName())
	}
	// A malformed name on a managed instance is refused by the SHAPE rule
	// first: the operator is told what is wrong with what they typed, not that
	// it is the wrong word for this deployment.
	m[EnvDiscordCommandName] = "Clip Dev"
	if err := loadErr(t, m); !strings.Contains(err.Error(), "lowercase") {
		t.Fatalf("a malformed name on a managed instance = %v, want the shape rule", err)
	}
}

// TestDiscordCommandNameRefusesSurroundingWhitespace: `l.raw` trims, so a name
// with a space around it used to be accepted as the trimmed word — the operator
// asked for one name and the instance answered another, silently. A value that
// is ONLY whitespace is still "unset" (asserted above); this is about a value
// with content in it.
func TestDiscordCommandNameRefusesSurroundingWhitespace(t *testing.T) {
	for _, name := range []string{"clip ", " clip", " clip ", "clip-dev\t", "\nclip"} {
		m := botEnv(t)
		m[EnvDiscordCommandName] = name
		err := loadErr(t, m)
		if err == nil {
			t.Fatalf("%s=%q was accepted", EnvDiscordCommandName, name)
		}
		if !strings.Contains(err.Error(), EnvDiscordCommandName) {
			t.Fatalf("%q was refused without naming the variable: %v", name, err)
		}
		if !strings.Contains(err.Error(), "whitespace") {
			t.Fatalf("%q was refused without naming the rule: %v", name, err)
		}
		if strings.Contains(err.Error(), testBotToken) {
			t.Fatalf("the rejection echoed the bot token: %v", err)
		}
	}
}

// TestDiscordBotCoReportsABadNameWithABadIdOrKey: the loader collects every
// fault and prints them together (§4), and the command name is now resolved
// before the application id and the public key are checked. Until then both of
// those returned first, so an operator who mistyped two things fixed one,
// restarted the container, and met the other.
func TestDiscordBotCoReportsABadNameWithABadIdOrKey(t *testing.T) {
	for _, tc := range []struct{ name, key, want string }{
		{name: "a bad application id", key: EnvDiscordAppID, want: "numeric id"},
		{name: "a bad public key", key: EnvDiscordPublicKey, want: "PUBLIC KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := botEnv(t)
			m[tc.key] = "nonsense"
			m[EnvDiscordCommandName] = "Clip Dev"
			err := loadErr(t, m)
			if err == nil {
				t.Fatal("two faults were accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the %s is not reported: %v", tc.name, err)
			}
			if !strings.Contains(err.Error(), EnvDiscordCommandName) {
				t.Fatalf("the bad command name is hidden behind %s: %v", tc.name, err)
			}
			if !strings.Contains(err.Error(), "lowercase") {
				t.Fatalf("the name's own rule is not stated: %v", err)
			}
			if strings.Contains(err.Error(), testBotToken) {
				t.Fatalf("the rejection echoed the bot token: %v", err)
			}
		})
	}
}
