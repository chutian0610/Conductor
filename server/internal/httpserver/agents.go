package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"conductor/server/internal/agentregistry"
)

// handleAgentsList answers GET /v1/agents.
//
// Query:
//
//	?backend=<type>           filter by backend (claude|codex)
//	?model=<id>               filter by exact model match
//	?include_archived=1       include soft-archived rows
//
// Response: {"agents":[Agent...]}. agentregistry.Agent is the wire
// struct (ADR §4 "no new wire types").
func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	q := r.URL.Query()
	opts := agentregistry.ListAgentOpts{
		Backend:         q.Get("backend"),
		Model:           q.Get("model"),
		IncludeArchived: q.Get("include_archived") == "1" || q.Get("include_archived") == "true",
	}
	agents, err := s.mgr.ListAgents(r.Context(), opts)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if agents == nil {
		agents = []agentregistry.Agent{}
	}
	_ = writeJSON(w, http.StatusOK, struct {
		Agents []agentregistry.Agent `json:"agents"`
	}{Agents: agents})
}

// agentCreateBody mirrors the multica-style POST /api/agents body
// (CLI cmd_agent.go line ~643) but only the fields that the
// Conductor V1.x registry actually persists. Workspace /
// owner / visibility / service_tier / thinking values (the
// multica catalog) are deliberately not surfaced — Conductor is
// single-tenant and these columns do not exist in the
// agentregistry schema yet.
type agentCreateBody struct {
	Name          string          `json:"name"`
	Backend       string          `json:"backend"`
	Description   string          `json:"description"`
	Parent        string          `json:"parent"`
	Instructions  string          `json:"instructions"`
	RuntimeConfig json.RawMessage `json:"runtime_config,omitempty"`
	CustomArgs    json.RawMessage `json:"custom_args,omitempty"`
	CustomEnv     json.RawMessage `json:"custom_env,omitempty"`
	McpConfig     json.RawMessage `json:"mcp_config,omitempty"`
	Model         string          `json:"model"`
	ThinkingLevel string          `json:"thinking_level"`
}

// handleAgentCreate answers POST /v1/agents.
//
// `name` and `backend` are required; the rest is optional. All
// raw-JSON fields are accepted as bytes verbatim — the registry
// stores them and the wire shape echoes them back. Validation
// beyond what agentregistry does is out of scope for this
// handler (per ADR-0010 §4, wire shapes are JSON-typed fields,
// no new wire types).
//
// Status codes:
//
//	201 Created + Agent body on success.
//	400 Bad Request on missing required field or unknown
//	      backend.
//	409 Conflict on unique-name collision (sniffed; see
//	      isAgentNameClashErr).
func (s *Server) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	defer r.Body.Close()

	var body agentCreateBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeJSONError(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	if strings.TrimSpace(body.Backend) == "" {
		writeJSONError(w, http.StatusBadRequest, errors.New("backend is required"))
		return
	}

	a := agentregistry.Agent{
		Name:          strings.TrimSpace(body.Name),
		Backend:       strings.TrimSpace(body.Backend),
		Description:   body.Description,
		Instructions:  body.Instructions,
		RuntimeConfig: body.RuntimeConfig,
		CustomArgs:    body.CustomArgs,
		CustomEnv:     body.CustomEnv,
		McpConfig:     body.McpConfig,
		Model:         body.Model,
		ThinkingLevel: body.ThinkingLevel,
	}
	if body.Parent != "" {
		parent, err := s.mgr.GetAgent(r.Context(), body.Parent)
		if err != nil {
			if errors.Is(err, agentregistry.ErrNotFound) {
				writeJSONError(w, http.StatusBadRequest, fmt.Errorf("parent %q not found", body.Parent))
				return
			}
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		a.ParentID = parent.ID
	}

	created, err := s.mgr.RegisterAgent(r.Context(), a)
	if err != nil {
		if isAgentNameClashErr(err) {
			writeJSONError(w, http.StatusConflict, err)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	_ = writeJSON(w, http.StatusCreated, created)
}

// handleAgentDetail answers GET /v1/agents/{id}. 404 when no
// agent matches the numeric id.
func (s *Server) handleAgentDetail(w http.ResponseWriter, r *http.Request, id int64) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	a, err := s.mgr.GetAgent(r.Context(), fmt.Sprintf("@%d", id))
	if err != nil {
		if errors.Is(err, agentregistry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent %d not found", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	_ = writeJSON(w, http.StatusOK, a)
}

// agentPatchBody mirrors agentCreateBody but with pointer fields
// so PATCH semantics distinguish "absent" from "set to zero".
type agentPatchBody struct {
	Description   *string         `json:"description,omitempty"`
	Backend       *string         `json:"backend,omitempty"`
	Parent        *string         `json:"parent,omitempty"`
	ClearParent   bool            `json:"clear_parent,omitempty"`
	Instructions  *string         `json:"instructions,omitempty"`
	RuntimeConfig json.RawMessage `json:"runtime_config,omitempty"`
	CustomArgs    json.RawMessage `json:"custom_args,omitempty"`
	CustomEnv     json.RawMessage `json:"custom_env,omitempty"`
	McpConfig     json.RawMessage `json:"mcp_config,omitempty"`
	Model         *string         `json:"model,omitempty"`
	ThinkingLevel *string         `json:"thinking_level,omitempty"`
}

// handleAgentPatch answers PATCH /v1/agents/{id}. Partial body;
// only the fields present are written. raw-JSON fields are
// accepted as bytes. clear_parent: true sets parent_id = NULL;
// passing clear_parent: true AND parent simultaneously is
// rejected as ambiguous. 200 + updated Agent body on success.
func (s *Server) handleAgentPatch(w http.ResponseWriter, r *http.Request, id int64) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	defer r.Body.Close()

	var body agentPatchBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	if body.ClearParent && body.Parent != nil {
		writeJSONError(w, http.StatusBadRequest, errors.New("clear_parent and parent are mutually exclusive"))
		return
	}

	var patch agentregistry.AgentPatch
	patch.Description = body.Description
	patch.Backend = body.Backend
	patch.Instructions = body.Instructions
	patch.RuntimeConfig = body.RuntimeConfig
	patch.CustomArgs = body.CustomArgs
	patch.CustomEnv = body.CustomEnv
	patch.McpConfig = body.McpConfig
	patch.Model = body.Model
	patch.ThinkingLevel = body.ThinkingLevel
	patch.ClearParent = body.ClearParent
	if body.Parent != nil {
		parent, err := s.mgr.GetAgent(r.Context(), *body.Parent)
		if err != nil {
			if errors.Is(err, agentregistry.ErrNotFound) {
				writeJSONError(w, http.StatusBadRequest, fmt.Errorf("parent %q not found", *body.Parent))
				return
			}
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		pid := parent.ID
		patch.ParentID = &pid
	}

	updated, err := s.mgr.UpdateAgent(r.Context(), id, patch)
	if err != nil {
		if errors.Is(err, agentregistry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent %d not found", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	_ = writeJSON(w, http.StatusOK, updated)
}

// handleAgentDelete answers DELETE /v1/agents/{id}. Soft
// archives (sets archived_at; underlying row stays). 204 on
// success; 404 if id unknown.
func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request, id int64) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	if err := s.mgr.ArchiveAgent(r.Context(), id); err != nil {
		if errors.Is(err, agentregistry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent %d not found", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentRuns answers GET /v1/agents/{id}/runs. Returns
// the runs (newest first) for one agent; 404 if the agent
// itself does not exist.
//
// Query: ?status=<status>, ?limit=<N>. Same semantics as
// /v1/runs (parseLimit).
func (s *Server) handleAgentRuns(w http.ResponseWriter, r *http.Request, id int64) {
	if s.mgr == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errNotConfigured)
		return
	}
	if _, err := s.mgr.GetAgent(r.Context(), fmt.Sprintf("@%d", id)); err != nil {
		if errors.Is(err, agentregistry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent %d not found", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	q := r.URL.Query()
	opts := agentregistry.ListRunOpts{
		Status: q.Get("status"),
		Limit:  parseLimit(q.Get("limit")),
	}
	runs, err := s.mgr.ListAgentRuns(r.Context(), fmt.Sprintf("@%d", id), opts)
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

// handleAgentsListOrCreate dispatches the /v1/agents root by method.
func (s *Server) handleAgentsListOrCreate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAgentsList(w, r)
	case http.MethodPost:
		s.handleAgentCreate(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAgentSubrouter splits /v1/agents/{id}/... into per-route
// handlers.
func (s *Server) handleAgentSubrouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
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
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid agent id: %s", parts[0]))
		return
	}
	switch len(parts) {
	case 1:
		switch r.Method {
		case http.MethodGet:
			s.handleAgentDetail(w, r, id)
		case http.MethodPatch:
			s.handleAgentPatch(w, r, id)
		case http.MethodDelete:
			s.handleAgentDelete(w, r, id)
		default:
			w.Header().Set("Allow", "GET, PATCH, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case 2:
		if parts[1] == "runs" && r.Method == http.MethodGet {
			s.handleAgentRuns(w, r, id)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// isAgentNameClashErr reports whether err from agentregistry.RegisterAgent
// is the unique-name-constraint violation. The SQLite store
// surfaces this as a UNIQUE constraint failure wrapped in
// "agentregistry: insert agent: %w"; we sniff the err text for
// the constraint signal rather than wiring a typed sentinel
// through every layer. (A future cleanup should add an
// agentregistry.ErrNameClash sentinel.)
func isAgentNameClashErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") &&
		strings.Contains(msg, "agents.name")
}
