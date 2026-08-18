package httpserver

import "net/http"

// handleVersion reports the daemon version. Public (no auth
// required) so monitoring / build-info integrations can scrape
// it cheaply.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = writeJSON(w, http.StatusOK, struct {
		Version string `json:"version"`
	}{Version: Version})
}
