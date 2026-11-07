package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clippet/internal/config"
)

// The doctor's `auto` row (§20 erratum 333): off calls nothing; on, ONE
// read-only probe — the token answers, the Message Content intent is on — and
// never the token in the output.
func TestDoctorAutoRow(t *testing.T) {
	const tok = "AUTO.token-that-must-never-print_0123"
	calls := 0
	type answer struct {
		tok, intent bool
		err         error
	}
	var next answer
	orig := autoProbe
	autoProbe = func(ctx context.Context, token string) (bool, bool, error) {
		calls++
		if token != tok {
			t.Errorf("the probe was given %q", token)
		}
		return next.tok, next.intent, next.err
	}
	t.Cleanup(func() { autoProbe = orig })

	fx := newDoctorFixture(t)
	_, out := fx.run(t)
	if calls != 0 || verdictOf(out, "auto") != vOK || !strings.Contains(out, "clippet auto: off") {
		t.Fatalf("off: calls=%d\n%s", calls, out)
	}

	for _, c := range []struct {
		a    answer
		want string
		line string
	}{
		{answer{tok: true, intent: true}, vOK, "Message Content intent is on"},
		{answer{tok: false}, vFail, "refused by Discord"},
		{answer{tok: true, intent: false}, vFail, "Message Content intent is off"},
		{answer{err: errors.New("dial " + tok)}, vWarn, "could not be asked"},
	} {
		next = c.a
		fx := newDoctorFixture(t)
		env := managedEnv(t, func(k string) string { return fx.env[k] })
		for k, v := range map[string]string{config.EnvDiscordAppID: env(config.EnvDiscordAppID),
			config.EnvDiscordPublicKey: env(config.EnvDiscordPublicKey), config.EnvDiscordBotToken: env(config.EnvDiscordBotToken),
			config.EnvDiscordAutoMode: "prompt", config.EnvDiscordAutoToken: tok} {
			fx.env[k] = v
		}
		_, out := fx.run(t)
		if v := verdictOf(out, "auto"); v != c.want || !strings.Contains(out, c.line) {
			t.Fatalf("%+v: auto = %s\n%s", c.a, v, out)
		}
		if strings.Contains(out, tok) {
			t.Fatalf("the doctor printed the token:\n%s", out)
		}
	}
}
