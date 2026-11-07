// Package httpx holds the HTTP plumbing shared by the JSON API and the SSR
// pages (DESIGN.md §8 conventions, §9.1/§9.2 headers, §18.1): the error
// envelope, middleware, request-scoped access-log annotations and the shared
// storage-health cache.
package httpx

import (
	"encoding/json"
	"net/http"
)

// ErrorBody is the §8 error envelope: {"error":"snake_code","message":"…"}.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// WriteJSON encodes v with status. The body is marshalled before any header
// is written so an encoding failure becomes a clean 500 rather than a
// half-written response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		b = []byte(`{"error":"internal","message":"response encoding failed"}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// WriteError writes the error envelope with a stable snake_case code.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, ErrorBody{Error: code, Message: msg})
}
