package main

import (
	"errors"
	"strings"
	"testing"
)

// dataDirRoom is the DATA_DIR half of the free-space check (N034). It is a
// pure function precisely so the decision can be tested without a filesystem
// small enough to trip it.
func TestDataDirRoom(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		name       string
		free, size int64
		sameDev    bool
		wantFloor  int64
		wantLow    bool
	}{
		// The shipped compose: one Docker filesystem for both directories, so
		// SCRATCH_MIN_FREE_BYTES already guards the database and this check
		// has nothing to add — even on a device with almost nothing left.
		{"same device, nearly full", 1, 500 * mib, true, 0, false},
		{"same device, roomy", 900 * mib, 4 * mib, true, 0, false},

		// DATA_DIR on a device of its own. A small database floors at 64 MiB…
		{"own device, tiny db, plenty free", 900 * mib, 4 * mib, false, 64 * mib, false},
		{"own device, tiny db, no room", 8 * mib, 4 * mib, false, 64 * mib, true},
		{"own device, tiny db, exactly at the floor", 64 * mib, 4 * mib, false, 64 * mib, false},
		{"own device, tiny db, one byte under", 64*mib - 1, 4 * mib, false, 64 * mib, true},

		// …and a big one raises the floor to twice its size: room for a
		// snapshot beside the live file.
		{"own device, big db, room for two", 900 * mib, 200 * mib, false, 400 * mib, false},
		{"own device, big db, room for one", 300 * mib, 200 * mib, false, 400 * mib, true},

		// No database yet (the pre-boot preflight this command exists for)
		// still gets the minimum floor rather than a floor of zero.
		{"own device, no db yet", 8 * mib, 0, false, 64 * mib, true},
		{"own device, no db yet, roomy", 900 * mib, 0, false, 64 * mib, false},
	} {
		floor, low := dataDirRoom(tc.free, tc.size, tc.sameDev)
		if floor != tc.wantFloor || low != tc.wantLow {
			t.Errorf("%s: dataDirRoom(%d, %d, %v) = (%d, %v), want (%d, %v)",
				tc.name, tc.free, tc.size, tc.sameDev, floor, low, tc.wantFloor, tc.wantLow)
		}
	}
}

// The fixture puts DATA_DIR and SCRATCH_DIR under one temp root, which is one
// device — the shipped shape — so the new branch must stay silent and `paths`
// must still be ok. A warn here would mean the check fires on the common case.
func TestDoctorPathsStaysQuietWhenTheDirectoriesShareADevice(t *testing.T) {
	fx := newDoctorFixture(t)
	code, out := fx.run(t)
	if code != 0 || verdictOf(out, "paths") != vOK {
		t.Fatalf("paths on one device: %d %s -- %s", code, verdictOf(out, "paths"), out)
	}
	if strings.Contains(out, "SCRATCH_MIN_FREE_BYTES does not guard it") {
		t.Fatalf("the DATA_DIR floor fired on a single-device install: %s", out)
	}
}

// N111: on a config-load failure doctor prints every loader error and returns
// before the parsed per-route loop can register those webhooks as secrets —
// which is exactly when an unvetted raw value is closest to stderr. The raw
// value is therefore swept for secrets BEFORE that branch, and BOTH halves of
// each rule go in: config.go's own warning is that the dangerous typo is
// `<url>=<tag>`, which puts the live webhook in the tag half.
func TestRouteSecretsCoversBothHalvesOfEveryRule(t *testing.T) {
	const good = "https://discord.com/api/webhooks/123456789/rout3d-tok3n-never-print"
	const typoed = "https://discord.com/api/webhooks/987654321/oth3r-tok3n-never-print"
	raw := "clutch=" + good + " ; " + typoed + "=oops"

	r := &doctorRun{}
	r.secret = append(r.secret, routeSecrets(raw)...)

	// A message quoting ONE rule's URL half, and one quoting the other rule's
	// URL where the operator put it by mistake. Neither may survive.
	for _, quoted := range []string{
		"rule 1 is not a webhook: " + good,
		"rule 2: " + typoed + " is not a tag",
		"DISCORD_WEBHOOK_ROUTES=" + raw + " is malformed",
	} {
		got := r.safe(errors.New(quoted))
		for _, tok := range []string{"rout3d-tok3n-never-print", "oth3r-tok3n-never-print"} {
			if strings.Contains(got, tok) {
				t.Errorf("a routed webhook token survived redaction of %q: %s", quoted, got)
			}
		}
		if !strings.Contains(got, "(redacted)") {
			t.Errorf("nothing was redacted out of %q: %s", quoted, got)
		}
	}

	// An unset variable contributes nothing at all: an empty secret would
	// match every string, and the redaction is applied to error text.
	if got := routeSecrets("   "); got != nil {
		t.Errorf("routeSecrets on an unset value = %v, want nil", got)
	}
	for _, s := range routeSecrets(raw) {
		if strings.TrimSpace(s) == "" {
			t.Errorf("routeSecrets produced an empty entry from %q", raw)
		}
	}
}

// The end of the same claim, through the real command: a load that FAILS with
// routes configured still prints no routed token, and still skips every check
// below `config` rather than guessing.
func TestDoctorWithRoutesAndAnUnparsableConfig(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.env["DISCORD_WEBHOOK_URL"] = "https://discord.com/api/webhooks/111111111/default-tok3n"
	fx.env["DISCORD_WEBHOOK_ROUTES"] = "clutch=https://discord.com/api/webhooks/123456789/rout3d-tok3n-never-print"
	fx.env["SWEEP_INTERVAL"] = "banana"
	code, out := fx.run(t)
	if code != 1 || !strings.Contains(out, "SWEEP_INTERVAL") {
		t.Fatalf("an unparsable config: %d -- %s", code, out)
	}
	for _, tok := range []string{"rout3d-tok3n-never-print", "default-tok3n"} {
		if strings.Contains(out, tok) {
			t.Fatalf("doctor printed a webhook token (%q): %s", tok, out)
		}
	}
	for _, check := range doctorChecks[1:] {
		if v := verdictOf(out, check); v != vSkip {
			t.Fatalf("check %q = %q, want skip: %s", check, v, out)
		}
	}
}
