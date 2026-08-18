package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
)

// writeJSONError emits {"error": "<message>"} with the given HTTP
// status. err is rendered as its Error() string; if it is nil the
// status text is used. Always 4xx / 5xx with a stable JSON shape.
func writeJSONError(w http.ResponseWriter, status int, err error) {
	msg := http.StatusText(status)
	if err != nil && err.Error() != "" {
		msg = err.Error()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}

// writeJSON marshals v and writes it with status. Returns the
// encoder error so test code can assert on it.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

// errNotConfigured is the sentinel used by handlers when the
// manager dep is not wired (server constructed with mgr=nil).
// Surfaced as 503 to the client.
var errNotConfigured = errors.New("httpserver: run manager not configured")
