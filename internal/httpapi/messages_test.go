package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// Validation messages name the rule that failed (§20 erratum 29): a title
// with a control character is not "too long". The status and error code
// stay 400 invalid_request; only the human text is at stake.
func TestTextValidationMessages(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("carol")
	c, _, _ := e.readyClip(owner, clipOpts{})
	video := testVideo(1000)
	clip := "/api/clips/" + c.ID
	cases := []struct {
		name string
		do   func() resp
		want string
	}{
		{"upload title NUL", func() resp { return e.upload(`{"title":"nul\u0000x"}`, "a.mp4", video, withCookie(cookie)) },
			"title must not contain control characters"},
		{"upload description BEL", func() resp { return e.upload(`{"description":"d\u0007"}`, "a.mp4", video, withCookie(cookie)) },
			"description must not contain control characters"},
		{"upload title too long", func() resp {
			return e.upload(`{"title":"`+strings.Repeat("中", maxTitleRunes+1)+`"}`, "a.mp4", video, withCookie(cookie))
		}, "title must be at most 200 characters"},
		{"upload description too long", func() resp {
			return e.upload(`{"description":"`+strings.Repeat("x", maxDescriptionRunes+1)+`"}`, "a.mp4", video, withCookie(cookie))
		}, "description must be at most 2000 characters"},
		{"patch title ESC", func() resp { return e.doJSON(http.MethodPatch, clip, `{"title":"a\u001bb"}`, withCookie(cookie)) },
			"title must not contain control characters"},
		{"patch title blank", func() resp { return e.doJSON(http.MethodPatch, clip, `{"title":"  "}`, withCookie(cookie)) },
			"title must be 1-200 characters"},
		{"patch title too long", func() resp {
			return e.doJSON(http.MethodPatch, clip, `{"title":"`+strings.Repeat("a", maxTitleRunes+1)+`"}`, withCookie(cookie))
		}, "title must be 1-200 characters"},
		{"patch description DEL", func() resp { return e.doJSON(http.MethodPatch, clip, `{"description":"a\u007fb"}`, withCookie(cookie)) },
			"description must not contain control characters"},
		{"token name NUL", func() resp {
			return e.doJSON(http.MethodPost, "/api/auth/tokens", `{"name":"t\u0000k"}`, withCookie(cookie))
		},
			"name must not contain control characters"},
		{"token name too long", func() resp {
			return e.doJSON(http.MethodPost, "/api/auth/tokens", `{"name":"`+strings.Repeat("n", maxTokenNameRunes+1)+`"}`, withCookie(cookie))
		}, "name must be 1-64 characters"},
		{"token name empty", func() resp { return e.doJSON(http.MethodPost, "/api/auth/tokens", `{"name":""}`, withCookie(cookie)) },
			"name must be 1-64 characters"},
	}
	for _, tc := range cases {
		r := tc.do()
		if r.code != 400 || r.errCode() != "invalid_request" || r.str("message") != tc.want {
			t.Errorf("%s: %d %s (want message %q)", tc.name, r.code, r.body, tc.want)
		}
	}
	// Newlines, tabs and CR are text, not control characters.
	r := e.doJSON(http.MethodPatch, clip, `{"title":"line1\nline2\ttab\r"}`, withCookie(cookie))
	if r.code != 200 || r.str("title") != "line1\nline2\ttab" {
		t.Fatalf("whitespace title: %d %s", r.code, r.body)
	}
}

// A type-mismatched field in a JSON object names the field and the types
// (§20 erratum 30); only syntax errors and non-object documents are told
// the body is not an object.
func TestJSONTypeMismatchMessages(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("carol")
	c, _, _ := e.readyClip(owner, clipOpts{})
	video := testVideo(1000)
	clip := "/api/clips/" + c.ID
	patch := func(body string) func() resp {
		return func() resp { return e.doJSON(http.MethodPatch, clip, body, withCookie(cookie)) }
	}
	upload := func(meta string) func() resp {
		return func() resp { return e.upload(meta, "a.mp4", video, withCookie(cookie)) }
	}
	cases := []struct {
		name string
		do   func() resp
		want string
	}{
		{"patch unlisted string", patch(`{"unlisted":"yes"}`), "field unlisted must be a boolean, got string"},
		{"patch unlisted number", patch(`{"unlisted":1}`), "field unlisted must be a boolean, got number"},
		{"patch title number", patch(`{"title":123}`), "field title must be a string, got number"},
		{"patch title array", patch(`{"title":["x"]}`), "field title must be a string, got array"},
		{"patch description object", patch(`{"description":{}}`), "field description must be a string, got object"},
		{"token name number", func() resp { return e.doJSON(http.MethodPost, "/api/auth/tokens", `{"name":5}`, withCookie(cookie)) },
			"field name must be a string, got number"},
		{"upload unlisted number", upload(`{"title":"u1","unlisted":1}`), "field unlisted must be a boolean, got number"},
		{"upload title bool", upload(`{"title":true}`), "field title must be a string, got bool"},
		// Not objects, or not JSON: the generic message stands.
		{"patch array", patch(`[]`), "body must be a JSON object"},
		{"patch number", patch(`42`), "body must be a JSON object"},
		{"patch truncated", patch(`{"unlisted":`), "body must be a JSON object"},
		{"patch garbage", patch(`title=hello`), "body must be a JSON object"},
		{"upload array", upload(`[]`), "metadata is not a JSON object"},
		{"upload truncated", upload(`{"title":`), "metadata is not a JSON object"},
	}
	for _, tc := range cases {
		r := tc.do()
		if r.code != 400 || r.errCode() != "invalid_request" || r.str("message") != tc.want {
			t.Errorf("%s: %d %s (want message %q)", tc.name, r.code, r.body, tc.want)
		}
	}
	// ttl_days keeps its own code and message (§8.2).
	if r := patch(`{"ttl_days":"7"}`)(); r.code != 400 || r.errCode() != "invalid_ttl" {
		t.Fatalf("ttl_days string: %d %s", r.code, r.body)
	}
	// And a well-typed body still works.
	if r := patch(`{"unlisted":false}`)(); r.code != 200 {
		t.Fatalf("control: %d %s", r.code, r.body)
	}
}
