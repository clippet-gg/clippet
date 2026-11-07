package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestMediaNoteOnTheShippedComposeStack: the `media` NOTE (erratum 278 (e)) is
// for a MEDIA_PRESIGN_* variable an operator SET under `proxy`. compose.yaml
// passed `MEDIA_PRESIGN_TTL: ${MEDIA_PRESIGN_TTL:-1h}`, so the app saw the
// variable set on every default stack and the NOTE greeted everyone (a nit
// sweep's `[visualmedia, doctor]` row; erratum 278 (e), amended). This reads the row
// compose.yaml really carries, feeds the app what compose hands it with
// nothing in `.env`, and wants no NOTE — and a value the operator set in
// proxy mode still gets one.
func TestMediaNoteOnTheShippedComposeStack(t *testing.T) {
	raw, err := os.ReadFile("../../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^      MEDIA_PRESIGN_TTL: \$\{MEDIA_PRESIGN_TTL:-([^}]*)\}\s*$`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("compose.yaml's app environment carries no `MEDIA_PRESIGN_TTL: ${MEDIA_PRESIGN_TTL:-…}` row")
	}
	shipped := map[string]string{"MEDIA_PRESIGN_TTL": m[1]} // what compose hands the app with nothing in .env
	c := mustLoad(t, shipped)
	if c.MediaDelivery != DeliveryProxy {
		t.Fatalf("the default is %q, want proxy", c.MediaDelivery)
	}
	if note := MediaNote(c, env(shipped)); note != "" {
		t.Fatalf("the shipped compose stack (MEDIA_PRESIGN_TTL=%q) gets the NOTE: %s", m[1], note)
	}
	if c.MediaPresignTTL != DefaultMediaPresignTTL {
		t.Fatalf("an empty MEDIA_PRESIGN_TTL loaded as %s, want the default %s", c.MediaPresignTTL, DefaultMediaPresignTTL)
	}
	set := map[string]string{"MEDIA_PRESIGN_TTL": "30m"}
	note := MediaNote(mustLoad(t, set), env(set))
	if !strings.Contains(note, "MEDIA_PRESIGN_TTL is set but MEDIA_DELIVERY=proxy") {
		t.Fatalf("a TTL the operator set in proxy mode got note %q", note)
	}
	// …even one equal to the default: the operator wrote it, and it is ignored.
	set = map[string]string{"MEDIA_PRESIGN_TTL": "1h"}
	if MediaNote(mustLoad(t, set), env(set)) == "" {
		t.Fatal("an explicit MEDIA_PRESIGN_TTL=1h in proxy mode got no NOTE")
	}
}
