package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"conductor/server/internal/agentregistry"
	"conductor/server/internal/runmgr"
)

// handleRunsListOrStart is the mux handler for "/v1/runs".
// It dispatches by HTTP method: GET lists, POST starts. Other
// methods get 405.
func (s *Server) handleRunsListOrStart(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleRunsList(w, r)
	case http.MethodPost:
		s.handleRunStart(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRunsList answers GET /v1/runs.
//
//	query strings:
//	  agent  - filter by agent name OR id (default: all agents)
//	  status - filter by run status (default: any status)
//	  limit  - max rows to return (default: 100, hard cap 1000)
//
// Responses are an object {"runs":[Run...]} (not a bare array, so
// we can add pagination metadata in v2 without breaking the JSON
// shape). The Run entries reuse the existing agentregistry.Run
// struct — see ADR-0010 section 4 (no new wire types).
func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	q := r.URL.Query()
	opts := agentregistry.ListRunOpts{
		Status: q.Get("status"),
		Limit:  parseLimit(q.Get("limit")),
	}
	runs, err := s.mgr.ListRuns(r.Context(), q.Get("agent"), opts)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if runs == nil {
		runs = []agentregistry.Run{}
	}
	_ = writeJSON(w, http.StatusOK, struct {
		Runs []agentregistry.Run `json:"runs"`
	}{Runs: runs})
}

// handleRunStart answers POST /v1/runs with the request body
// {"agent":"name-or-id","prompt":"...","resume_id":"..."} (all
// except agent are optional). Returns 202 Created with the
// freshly-created Run row. The backend runs asynchronously; clients
// follow progress via /v1/runs/{id}/stream.
func (s *Server) handleRunStart(w http.ResponseWriter, r *http.Request) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	defer r.Body.Close()

	var body struct {
		Agent    string `json:"agent"` // name or numeric id
		Prompt   string `json:"prompt"`
		ResumeID string `json:"resume_id"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	if strings.TrimSpace(body.Agent) == "" {
		writeJSONError(w, http.StatusBadRequest, errors.New("agent is required"))
		return
	}

	run, err := s.mgr.StartRun(r.Context(), runmgr.StartRequest{
		AgentName: body.Agent,
		Prompt:    body.Prompt,
		ResumeID:  body.ResumeID,
		Env:       osEnvironFromRequest(r),
	})
	if err != nil {
		if errors.Is(err, runmgr.ErrAgentNotFound) {
			writeJSONError(w, http.StatusNotFound, err)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	_ = writeJSON(w, http.StatusAccepted, run)
}

// handleRunGet answers GET /v1/runs/{id}. 404 if the run does
// not exist; 200 with the Run row otherwise.
func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request, id int64) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	run, err := s.mgr.Run(r.Context(), id)
	if err != nil {
		if errors.Is(err, agentregistry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("run %d not found", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	_ = writeJSON(w, http.StatusOK, run)
}

// handleRunEvents answers GET /v1/runs/{id}/events. Reads the
// durable event log; useful for replay and for clients that
// do not want to use SSE.
//
//	query: after_seq=N  (return only events with Seq > N)
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request, id int64) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	afterSeq, err := parseAfterSeq(r.URL.Query().Get("after_seq"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	events, err := s.mgr.EventsAfter(r.Context(), id, afterSeq)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if events == nil {
		events = []agentregistry.Event{}
	}
	_ = writeJSON(w, http.StatusOK, struct {
		Events []agentregistry.Event `json:"events"`
	}{Events: events})
}

// handleRunResult answers GET /v1/runs/{id}/result.
//   - 200 + Run row when the run has finished.
//   - 425 Too Early when the run is still running; client should
//     switch to /v1/runs/{id}/stream.
//
// Deliberately does NOT block on the in-memory goroutine: SSE and
// result endpoints serve different roles, and clients that want
// the terminal row should follow the stream.
func (s *Server) handleRunResult(w http.ResponseWriter, r *http.Request, id int64) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	run, err := s.mgr.Run(r.Context(), id)
	if err != nil {
		if errors.Is(err, agentregistry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("run %d not found", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if run.Status == "running" {
		w.Header().Set("Retry-After", "0")
		writeJSONError(w, http.StatusTooEarly, fmt.Errorf("run %d still running; follow /v1/runs/%d/stream", id, id))
		return
	}
	_ = writeJSON(w, http.StatusOK, run)
}

// handleRunStream answers GET /v1/runs/{id}/stream as
// text/event-stream per ADR-0010 section 5. Replay comes from the
// registry so reconnects pick up where they left off; the
// in-memory broadcaster carries the live tail.
//
// Headers:
//
//	Last-Event-Id: N  - resume after seq N (same semantic as ?after_seq=N)
//	X-Accel-Buffering: no is set on the response so nginx etc. flush immediately.
func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request, id int64) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	afterSeq, err := parseAfterSeq(firstNonEmpty(
		r.Header.Get("Last-Event-Id"),
		r.URL.Query().Get("after_seq"),
	))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}

	// Verify the run exists; explicit 404 before the SSE upgrade.
	if _, err := s.mgr.Run(r.Context(), id); err != nil {
		if errors.Is(err, agentregistry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("run %d not found", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}

	res, err := s.mgr.Subscribe(r.Context(), id, afterSeq, runmgr.DefaultSSEBufferSize)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("X-Accel-Buffering", "no") // nginx etc.
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	if flusher == nil {
		s.logger.Warn("httpserver: response writer is not a Flusher; SSE will stall")
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case bytes, ok := <-res.Events:
			if !ok {
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			if _, err := w.Write(bytes); err != nil {
				// Client gone; the broadcaster is buffered and
				// will drop the slow subscriber on its own; no
				// explicit unsub required.
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// handleRunSubrouter splits /v1/runs/{id}/... into per-route
// handlers. Registered as the mux handler for "/v1/runs/".
func (s *Server) handleRunSubrouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
	if path == "" || strings.Contains(path, "//") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid run id: %s", parts[0]))
		return
	}
	if len(parts) == 1 {
		s.handleRunGet(w, r, id)
		return
	}
	switch parts[1] {
	case "events":
		s.handleRunEvents(w, r, id)
	case "result":
		s.handleRunResult(w, r, id)
	case "stream":
		s.handleRunStream(w, r, id)
	case "audit":
		// GET /v1/runs/{id}/audit and POST /v1/runs/{id}/audit:run diverge
		// by method; dispatch below.
		switch r.Method {
		case http.MethodGet:
			s.handleRunAudit(w, r, id)
		default:
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case "audit:run":
		if r.Method == http.MethodPost {
			s.handleRunAuditRun(w, r, id)
			return
		}
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, r)
	}
}

// --- helpers -------------------------------------------------------------

// parseLimit converts the limit query param into a numeric
// cap. Returns 100 by default, capped at 1000. Non-numeric
// input is treated as default.
func parseLimit(raw string) int {
	if raw == "" {
		return 100
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 100
	}
	if n > 1000 {
		return 1000
	}
	return n
}

// parseAfterSeq converts a query value into a non-negative
// sequence number. Empty input is 0; negative becomes 0; bad
// input returns an error.
func parseAfterSeq(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid after_seq %q: %w", raw, err)
	}
	if n < 0 {
		return 0, nil
	}
	return n, nil
}

// firstNonEmpty returns the first non-empty string among its
// arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// osEnvironFromRequest is a placeholder for identity env injection
// (CONDUCTOR_AGENT_ID, CONDUCTOR_PARENT_AGENT_ID, etc.) — see
// agentregistry.IdentityEnv. Returns nil until identity propagation
// lands in a follow-up.
func osEnvironFromRequest(r *http.Request) []string {
	return nil
}
