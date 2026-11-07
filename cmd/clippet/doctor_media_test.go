package main

import (
	"errors"
	"strings"
	"testing"
)

// `admin doctor`'s `media` row (ROADMAP #34 lane F, §20 erratum 278). Delivery
// is a different question from storage, and it is the one an operator who has
// just edited `.env` wants answered from inside the container — before any
// viewer is the one who finds out.

func TestDoctorMediaRow(t *testing.T) {
	// The shipped default: one line, ok, and no mention of a presign.
	fx := newDoctorFixture(t)
	_, out := fx.run(t)
	row := rowOf(t, out, "media")
	if v := verdictOf(out, "media"); v != vOK {
		t.Fatalf("media = %q on the shipped default:\n%s", v, out)
	}
	if !strings.Contains(row, "MEDIA_DELIVERY=proxy") || strings.Contains(row, "X-Amz") || strings.Contains(row, "302") {
		t.Errorf("proxy row: %s", row)
	}

	// A MEDIA_PRESIGN_* value that was read and then IGNORED is a NOTE, not a
	// warning: it is what `.env` looks like while an operator gets ready to
	// flip the switch. But it is never silent — "I set it and nothing
	// happened" must have an answer that is not a viewer.
	fx = newDoctorFixture(t)
	fx.env["MEDIA_PRESIGN_ENDPOINT"] = "https://clips.example.test"
	_, out = fx.run(t)
	row = rowOf(t, out, "media")
	if verdictOf(out, "media") != vOK {
		t.Errorf("an ignored MEDIA_PRESIGN_ENDPOINT is not a warning:\n%s", out)
	}
	if !strings.Contains(row, "NOTE:") || !strings.Contains(row, "MEDIA_PRESIGN_ENDPOINT") || !strings.Contains(row, "ignored") {
		t.Errorf("the note is missing: %s", row)
	}

	// Redirect: the mode, the origin, the TTL, and the routes that stay
	// app-served. The origin is the one a real presign LANDS on, which the Fake
	// stands in for — the whole point of §20 erratum 278's amendment is that it
	// is not necessarily the configured endpoint.
	fx = newDoctorFixture(t)
	fx.env["MEDIA_DELIVERY"] = "redirect"
	fx.env["MEDIA_PRESIGN_TTL"] = "15m"
	fx.fake.PresignOrigin = "https://clips.acct.r2.cloudflarestorage.com" // virtual-host: the bucket is in the host
	_, out = fx.run(t)
	row = rowOf(t, out, "media")
	if verdictOf(out, "media") != vOK {
		t.Fatalf("a well-formed redirect configuration warned:\n%s", out)
	}
	for _, want := range []string{"MEDIA_DELIVERY=redirect", "302", "https://clips.acct.r2.cloudflarestorage.com", "15m", "/original"} {
		if !strings.Contains(row, want) {
			t.Errorf("the redirect row lacks %q: %s", want, row)
		}
	}
	// …and when the signed origin differs from the configured endpoint it says
	// so, because "why does the CSP name a host I never configured" is the
	// question this row exists to answer.
	if !strings.Contains(row, "signed against https://acct.r2.cloudflarestorage.com") ||
		!strings.Contains(row, "virtual-host") {
		t.Errorf("the row does not explain the bucket in the host: %s", row)
	}

	// A presign origin only this machine can reach, under a PUBLIC base URL: a
	// viewer would follow the redirect to nothing. WARN, never FAIL — a
	// single-label name can be resolvable in somebody's own DNS, and doctor's
	// exit code must not turn on a heuristic.
	fx = newDoctorFixture(t)
	fx.env["MEDIA_DELIVERY"] = "redirect"
	fx.env["MEDIA_PRESIGN_ENDPOINT"] = "https://minio"
	fx.fake.PresignOrigin = "https://clips.minio"
	code, out := fx.run(t)
	if v := verdictOf(out, "media"); v != vWarn {
		t.Fatalf("a private presign origin under a public base = %q:\n%s", v, out)
	}
	if code != 0 {
		t.Errorf("a warning failed the preflight: exit %d", code)
	}
	if row = rowOf(t, out, "media"); !strings.Contains(row, "MEDIA_PRESIGN_ENDPOINT") {
		t.Errorf("the warning does not name the variable to set: %s", row)
	}

	// …and the same origin under a LOOPBACK base URL is the dev stack, which
	// is exactly right and says nothing.
	fx = newDoctorFixture(t)
	fx.env["PUBLIC_BASE_URL"] = "http://localhost:8481"
	fx.env["MEDIA_DELIVERY"] = "redirect"
	fx.env["MEDIA_PRESIGN_ENDPOINT"] = "http://minio:9000"
	fx.fake.PresignOrigin = "http://clips.minio:9000"
	if _, out = fx.run(t); verdictOf(out, "media") != vOK {
		t.Errorf("a loopback dev stack warned about its own bucket:\n%s", out)
	}

	// A store that cannot mint a link at all is a WARN carrying the reason,
	// never a row that quietly prints an empty origin.
	fx = newDoctorFixture(t)
	fx.env["MEDIA_DELIVERY"] = "redirect"
	fx.fake.FailNext("PresignGet", errors.New("the signer refused"))
	if _, out = fx.run(t); verdictOf(out, "media") != vWarn {
		t.Fatalf("a presign that failed did not warn:\n%s", out)
	}
	if row = rowOf(t, out, "media"); !strings.Contains(row, "redirected nowhere") {
		t.Errorf("the row does not say what a viewer would get: %s", row)
	}
}

// TestIsPrivateHost pins the heuristic the warning above rests on.
func TestIsPrivateHost(t *testing.T) {
	for _, h := range []string{"minio", "localhost", "127.0.0.1", "::1", "10.1.2.3", "192.168.0.9", "172.16.4.4", "169.254.1.1", "0.0.0.0"} {
		if !isPrivateHost(h) {
			t.Errorf("isPrivateHost(%q) = false", h)
		}
	}
	for _, h := range []string{"acct.r2.cloudflarestorage.com", "s3.us-west-004.backblazeb2.com", "clips.example", "8.8.8.8", "", "2606:4700::1111"} {
		if isPrivateHost(h) {
			t.Errorf("isPrivateHost(%q) = true", h)
		}
	}
}

// rowOf returns the first report line for check, for an assertion about its
// TEXT rather than its verdict.
func rowOf(t *testing.T, out, check string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, check+" ") {
			return line
		}
	}
	t.Fatalf("check %q did not appear:\n%s", check, out)
	return ""
}
