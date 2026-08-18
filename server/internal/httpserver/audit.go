package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"conductor/server/internal/agentregistry"
	"conductor/server/internal/audit"
)

// handleRunAudit answers GET /v1/runs/{id}/audit.
// Returns the latest audit row for the run. If the run has not
// been audited yet, returns 404; if the run itself does not exist
// returns 404 too (operator cannot distinguish "no run" from
// never audited" without leaking ids).
//
// The audit row uses agentregistry.RunAudit struct (ADR-0010 sect 4
// "no new wire types"): the same wire shape that the audit CLI
// surfaces.
func (s *Server) handleRunAudit(w http.ResponseWriter, r *http.Request, id int64) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	if _, err := s.mgr.Run(r.Context(), id); err != nil {
		if errors.Is(err, agentregistry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("run %d not found", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	audit, found, err := s.mgr.GetLatestAudit(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("run %d has never been audited", id))
		return
	}
	_ = writeJSON(w, http.StatusOK, audit)
}

// handleRunAuditRun answers POST /v1/runs/{id}/audit:run.
// Triggers a fresh adversarial audit (ADR-0009) for the run and
// returns the RunAudit row when the auditor subprocess finishes.
//
// Synchronous on purpose (ADR-0010 sect 4 "sync because they are
// cheap-ish"): the auditor is a short subprocess (default 5 min
// cap in audit.auditTimeout).
//
// Body (all fields optional): {"force": bool, "model": "..."}.
//   - force: re-audit a run that already has an audit row.
//   - model: override the auditor model (default = claude pick).
//
// Status codes:
//
//	200 OK               audit finished, body is the RunAudit row.
//	404 Not Found        run does not exist.
//	409 Conflict         run already audited and force=false.
func (s *Server) handleRunAuditRun(w http.ResponseWriter, r *http.Request, id int64) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}

	var body struct {
		Force bool   `json:"force"`
		Model string `json:"model"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}

	// Verify the run exists first; audit.Run on a missing run is
	// ambiguous (could be ErrNotFound or a backend error), so we
	// resolve it explicitly here.
	if _, err := s.mgr.Run(r.Context(), id); err != nil {
		if errors.Is(err, agentregistry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("run %d not found", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}

	res, err := s.mgr.AuditRun(r.Context(), id, body.Force, strings.TrimSpace(body.Model))
	if err != nil {
		if errors.Is(err, audit.ErrAlreadyAudited) {
			w.Header().Set("Retry-After", "0")
			writeJSONError(w, http.StatusConflict, err)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	_ = writeJSON(w, http.StatusOK, res)
}

// handleAuditsPending answers GET /v1/audits/pending.
// Returns the list of run ids that have never been audited, or
// whose previous audit is a stale "pending" row left behind by a
// crashed mid-audit invocation.
//
// Query: ?limit=N (default 50, hard cap 1000, mirrors /v1/runs).
//
// Response is an object {"pending":[int64...]}; future pagination
// / cursor metadata can land in the same envelope without breaking
// the wire shape (ADR-0010 sect 4 "no new wire types").
func (s *Server) handleAuditsPending(w http.ResponseWriter, r *http.Request) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	q := r.URL.Query()
	opts := agentregistry.ListRunOpts{
		Status: q.Get("status"),
		Limit:  parseLimit(q.Get("limit")),
	}
	ids, err := s.mgr.ListPendingAudits(r.Context(), opts)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if ids == nil {
		ids = []int64{}
	}
	_ = writeJSON(w, http.StatusOK, struct {
		Pending []int64 `json:"pending"`
	}{Pending: ids})
}
