package main

import (
	"strings"
	"testing"

	"clippet/internal/config"
)

// The clip webhook's doctor rows and admin vocabulary (ROADMAP #54, §20
// erratum 319).

const (
	doctorHookToken  = "hooktok-must-never-print-0123"
	doctorHookSecret = "doctor-hook-secret-doctor-hook-secret"
)

// webhookRows returns the `webhook` check's lines, in order.
func webhookRows(out string) []string {
	var rows []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "webhook" {
			rows = append(rows, line)
		}
	}
	return rows
}

func TestDoctorHookUnsetLeavesTheRowsAlone(t *testing.T) {
	fx := newDoctorFixture(t)
	_, out := fx.run(t)
	rows := webhookRows(out)
	if len(rows) != 1 || !strings.Contains(rows[0], "DISCORD_WEBHOOK_URL is unset: no clip is announced anywhere and the binary opens no outbound connection but S3 (§15)") {
		t.Fatalf("webhook rows with the hook unset:\n%s", strings.Join(rows, "\n"))
	}
	if strings.Contains(out, "CLIP_WEBHOOK") {
		t.Fatalf("a hook row with the hook unset:\n%s", out)
	}
}

func TestDoctorHookRow(t *testing.T) {
	for _, tc := range []struct {
		url, form string
		plain     bool
	}{
		{"https://automation.example.com/hooks/" + doctorHookToken, "https://automation.example.com/(path redacted)", false},
		{"http://automation.lan:5678/in?k=" + doctorHookToken, "http://automation.lan:5678/(path redacted)", true},
	} {
		fx := newDoctorFixture(t)
		fx.env[config.EnvClipWebhookURL] = tc.url
		fx.env[config.EnvClipWebhookSecret] = doctorHookSecret
		_, out := fx.run(t)
		rows := webhookRows(out)
		if len(rows) != 2 {
			t.Fatalf("%s: want the Discord row then the hook row:\n%s", tc.url, strings.Join(rows, "\n"))
		}
		if !strings.Contains(rows[0], "DISCORD_WEBHOOK_URL is unset: no clip is posted to Discord") || strings.Contains(rows[0], "anywhere") {
			t.Fatalf("%s: the Discord-unset row with the hook set:\n%s", tc.url, rows[0])
		}
		want := "webhook  ok   CLIP_WEBHOOK_URL parses as " + tc.form + " - every clip that becomes ready is sent there, unlisted ones included, signed with CLIP_WEBHOOK_SECRET (DEPLOY.md §11.1); the shape is checked, nothing is sent"
		if tc.plain {
			want += "; plain http can be read by anyone on the network path"
		}
		if rows[1] != want {
			t.Fatalf("hook row\n got %q\nwant %q", rows[1], want)
		}
		if verdictOf(out, "webhook") != vOK {
			t.Fatalf("webhook verdict = %s", verdictOf(out, "webhook"))
		}
		for _, leak := range []string{doctorHookToken, doctorHookSecret} {
			if strings.Contains(out, leak) {
				t.Fatalf("doctor printed %q:\n%s", leak, out)
			}
		}
		if !strings.Contains(out, "doctor:  19 checks") {
			t.Fatalf("the summary moved:\n%s", out)
		}
	}
}

// TestDoctorHookAfterDiscordRows: with Discord on, its rows are unchanged and
// the hook row is last.
func TestDoctorHookAfterDiscordRows(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.env["DISCORD_WEBHOOK_URL"] = "https://discord.com/api/webhooks/123456789/tok3n-that-must-never-print"
	fx.env[config.EnvClipWebhookURL] = "https://h.example/x?k=" + doctorHookToken
	fx.env[config.EnvClipWebhookSecret] = doctorHookSecret
	_, out := fx.run(t)
	rows := webhookRows(out)
	if len(rows) != 2 || !strings.Contains(rows[0], "DISCORD_WEBHOOK_URL parses as discord.com/123456789/(token redacted) - every LISTED clip") ||
		!strings.Contains(rows[1], "CLIP_WEBHOOK_URL parses as https://h.example/(path redacted)") {
		t.Fatalf("rows:\n%s", strings.Join(rows, "\n"))
	}
}

// TestDoctorHookConfigErrorNeverEchoes: a value that fails to load is still a
// secret on its way to stderr.
func TestDoctorHookConfigErrorNeverEchoes(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.env[config.EnvClipWebhookURL] = "https://h.example/x?k=" + doctorHookToken
	code, out := fx.run(t)
	if code == 0 || !strings.Contains(out, "CLIP_WEBHOOK_SECRET: is unset but CLIP_WEBHOOK_URL is set") {
		t.Fatalf("doctor with the URL alone: %d\n%s", code, out)
	}
	if strings.Contains(out, doctorHookToken) {
		t.Fatalf("doctor printed the URL:\n%s", out)
	}
}

// TestDoctorFSStoreRowWithTheHook: the `fs` sentence keeps only its true half
// when the hook is an outbound destination.
func TestDoctorFSStoreRowWithTheHook(t *testing.T) {
	fx := newFSFixture(t)
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	_, out := fx.doctor(t)
	if !strings.Contains(out, "on `fs` the media path has NO outbound destination: with the webhook and the bot unset this instance opens no outbound connection at all (§15)") {
		t.Fatalf("the fs row with the hook unset moved:\n%s", out)
	}
	if !strings.Contains(out, "with STORAGE_BACKEND=fs the binary opens no outbound connection at all (§15)") {
		t.Fatalf("the Discord-unset row on fs moved:\n%s", out)
	}
	fx.env[config.EnvClipWebhookURL] = "http://10.0.0.5:8123/api/webhook/" + doctorHookToken
	fx.env[config.EnvClipWebhookSecret] = doctorHookSecret
	_, out = fx.doctor(t)
	if !strings.Contains(out, "store    ok   on `fs` the media path has NO outbound destination\n") {
		t.Fatalf("the fs row with the hook set:\n%s", out)
	}
	if strings.Contains(out, "opens no outbound connection") || strings.Contains(out, doctorHookToken) {
		t.Fatalf("a false or leaking sentence with the hook set:\n%s", out)
	}
}

func TestAdminKnowsTheHookKind(t *testing.T) {
	if !validJobKind("hook") || jobKinds[len(jobKinds)-1] != "hook" {
		t.Fatalf("jobKinds = %v: `hook` must be valid and last", jobKinds)
	}
	fx := newAdminFixture(t)
	code, out := fx.run(t, "stats", "--no-storage")
	if code != 0 || !strings.Contains(out, " filmstrip=0 full=0 hook=0)") {
		t.Fatalf("stats: %d\n%s", code, out)
	}
}
