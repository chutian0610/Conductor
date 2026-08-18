package httpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"conductor/server/internal/agentregistry"
)

// fullBody returns a multica-style agent-create body covering every
// field the handler accepts. Tests pass either this or a stripped
// variant to the handler.
func fullAgentBody(name, backend string) map[string]any {
	return map[string]any{
		"name":           name,
		"backend":        backend,
		"description":    "test desc",
		"instructions":   "do the thing carefully",
		"model":          "claude-sonnet-4-5",
		"thinking_level": "medium",
		"runtime_config": map[string]any{"max_turns": 8},
		"custom_args":    []string{"--strict"},
		"custom_env":     map[string]string{"FOO": "bar"},
		"mcp_config":     map[string]any{"servers": []any{}},
	}
}

// --- GET /v1/agents ----------------------------------------------------

func TestAgentsListEmpty(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))

	status, body := doGET(t, addr, tok, "/v1/agents")
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	var resp struct {
		Agents []agentregistry.Agent `json:"agents"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Agents == nil {
		t.Error("agents is nil; want []")
	}
}

func TestAgentsListSeededAndFilters(t *testing.T) {
	reg := openRegOnly(t)
	seedAgent(t, reg, "filter-a")
	if err := reg.UpdateAgent(context.Background(), mustAgent(t, reg, "filter-a").ID, agentregistry.AgentPatch{
		Model: strPtr("sonnet"),
	}); err != nil {
		t.Fatal(err)
	}
	seedAgent(t, reg, "filter-b")
	if err := reg.UpdateAgent(context.Background(), mustAgent(t, reg, "filter-b").ID, agentregistry.AgentPatch{
		Model: strPtr("haiku"),
	}); err != nil {
		t.Fatal(err)
	}
	addr, tok := startServerWithReg(t, reg)

	// backend filter
	status, body := doGET(t, addr, tok, "/v1/agents?backend=claude")
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	var resp struct {
		Agents []agentregistry.Agent `json:"agents"`
	}
	_ = json.Unmarshal(body, &resp)
	if len(resp.Agents) != 2 {
		t.Errorf("claude filter: got %d, want 2", len(resp.Agents))
	}

	// model filter
	_, body = doGET(t, addr, tok, "/v1/agents?model=sonnet")
	resp = struct {
		Agents []agentregistry.Agent `json:"agents"`
	}{}
	_ = json.Unmarshal(body, &resp)
	if len(resp.Agents) != 1 || resp.Agents[0].Name != "filter-a" {
		t.Errorf("model filter: got %d agents, want 1 (filter-a)", len(resp.Agents))
	}

	// model filter that matches nothing
	_, body = doGET(t, addr, tok, "/v1/agents?model=nonexistent")
	_ = json.Unmarshal(body, &resp)
	if len(resp.Agents) != 0 {
		t.Errorf("nonexistent model filter: got %d, want 0", len(resp.Agents))
	}
}

// --- GET /v1/agents/{id} ----------------------------------------------

func TestAgentDetail200(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "detail-1")
	addr, tok := startServerWithReg(t, reg)

	status, body := doGET(t, addr, tok, fmt.Sprintf("/v1/agents/%d", id))
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	var a agentregistry.Agent
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.ID != id {
		t.Errorf("id = %d, want %d", a.ID, id)
	}
}

func TestAgentDetail404(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	status, _ := doGET(t, addr, tok, "/v1/agents/9999")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// --- POST /v1/agents --------------------------------------------------

func TestAgentCreate201(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))

	status, body := doJSON(t, addr, tok, "POST", "/v1/agents", fullAgentBody("create-1", "claude"))
	if status != http.StatusCreated {
		t.Errorf("status = %d, want 201", status)
	}
	var a agentregistry.Agent
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.Name != "create-1" || a.Backend != "claude" {
		t.Errorf("got name=%q backend=%q", a.Name, a.Backend)
	}
	if a.Model != "claude-sonnet-4-5" {
		t.Errorf("model = %q, want claude-sonnet-4-5", a.Model)
	}
	if a.ThinkingLevel != "medium" {
		t.Errorf("thinking_level = %q, want medium", a.ThinkingLevel)
	}
	if len(a.RuntimeConfig) == 0 {
		t.Error("runtime_config empty; want non-empty JSON")
	}
}

func TestAgentCreate400MissingName(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	body := map[string]any{"backend": "claude"}
	status, _ := doJSON(t, addr, tok, "POST", "/v1/agents", body)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestAgentCreate400MissingBackend(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	body := map[string]any{"name": "x"}
	status, _ := doJSON(t, addr, tok, "POST", "/v1/agents", body)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestAgentCreate409DupName(t *testing.T) {
	reg := openRegOnly(t)
	seedAgent(t, reg, "dup")
	addr, tok := startServerWithReg(t, reg)
	body := map[string]any{"name": "dup", "backend": "claude"}
	status, _ := doJSON(t, addr, tok, "POST", "/v1/agents", body)
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409", status)
	}
}

func TestAgentCreateRejectsUnknownField(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	body, _ := json.Marshal(map[string]any{
		"name": "x", "backend": "claude", "bogus_field": 1,
	})
	r, _ := http.NewRequest("POST", fmt.Sprintf("http://%s/v1/agents", addr), strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (DisallowUnknownFields)", resp.StatusCode)
	}
}

// --- PATCH /v1/agents/{id} ---------------------------------------------

func TestAgentPatch200Partial(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "patch-me")
	addr, tok := startServerWithReg(t, reg)

	status, body := doJSON(t, addr, tok, "PATCH", fmt.Sprintf("/v1/agents/%d", id),
		map[string]any{"description": "updated", "model": "haiku"})
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	var a agentregistry.Agent
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.Description != "updated" || a.Model != "haiku" {
		t.Errorf("got desc=%q model=%q", a.Description, a.Model)
	}
}

func TestAgentPatchClearParent(t *testing.T) {
	reg := openRegOnly(t)
	parentID := seedAgent(t, reg, "parent-agent")
	childID := seedAgent(t, reg, "child-agent")
	if err := reg.UpdateAgent(context.Background(), childID, agentregistry.AgentPatch{
		ParentID: &parentID,
	}); err != nil {
		t.Fatal(err)
	}
	addr, tok := startServerWithReg(t, reg)
	status, body := doJSON(t, addr, tok, "PATCH", fmt.Sprintf("/v1/agents/%d", childID),
		map[string]any{"clear_parent": true})
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	var a agentregistry.Agent
	_ = json.Unmarshal(body, &a)
	if a.ParentID != 0 {
		t.Errorf("parent_id = %d, want 0 after clear_parent", a.ParentID)
	}
}

func TestAgentPatch400AmbiguousParent(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "ambiguous")
	addr, tok := startServerWithReg(t, reg)
	status, _ := doJSON(t, addr, tok, "PATCH", fmt.Sprintf("/v1/agents/%d", id),
		map[string]any{"parent": "x", "clear_parent": true})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (parent + clear_parent mutually exclusive)", status)
	}
}

func TestAgentPatch404(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	status, _ := doJSON(t, addr, tok, "PATCH", "/v1/agents/9999",
		map[string]any{"description": "x"})
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// --- DELETE /v1/agents/{id} --------------------------------------------

func TestAgentDelete204(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "del-me")
	addr, tok := startServerWithReg(t, reg)

	r, _ := http.NewRequest("DELETE", fmt.Sprintf("http://%s/v1/agents/%d", addr, id), nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	// Subsequent GET still returns the row (soft archive).
	status, _ := doGET(t, addr, tok, fmt.Sprintf("/v1/agents/%d", id))
	if status != http.StatusOK {
		t.Errorf("post-delete GET status = %d, want 200", status)
	}
	// ... but the archived_at field is non-null now.
	status, body := doGET(t, addr, tok, fmt.Sprintf("/v1/agents/%d", id))
	var a agentregistry.Agent
	_ = json.Unmarshal(body, &a)
	if a.ArchivedAt == nil {
		t.Error("archived_at is nil after delete; want non-null")
	}
}

func TestAgentDelete404(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	r, _ := http.NewRequest("DELETE", "http://"+addr+"/v1/agents/9999", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- GET /v1/agents/{id}/runs -----------------------------------------

func TestAgentRunsSeeded(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "runs-host")
	for i := 0; i < 2; i++ {
		_, err := reg.StartRun(context.Background(), agentregistry.Run{
			AgentID: id, Status: "completed", StartedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	addr, tok := startServerWithReg(t, reg)

	status, body := doGET(t, addr, tok, fmt.Sprintf("/v1/agents/%d/runs", id))
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	var resp struct {
		Runs []agentregistry.Run `json:"runs"`
	}
	_ = json.Unmarshal(body, &resp)
	if len(resp.Runs) != 2 {
		t.Errorf("len(runs) = %d, want 2", len(resp.Runs))
	}
}

func TestAgentRuns404UnknownAgent(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	status, _ := doGET(t, addr, tok, "/v1/agents/9999/runs")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (agent missing)", status)
	}
}

// --- Auth + method helpers --------------------------------------------

func TestAgentsRequiresAuth(t *testing.T) {
	addr, _ := startServerWithReg(t, openRegOnly(t))
	resp, err := http.Get("http://" + addr + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// /v1/agents (root, no id) only allows GET and POST; DELETE
// without id falls through to 405.
func TestAgentsRootRejectsDELETE(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	r, _ := http.NewRequest("DELETE", "http://"+addr+"/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}

	// /v1/agents/{id} (no subroute) does NOT allow POST.
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "method-check")
	addr2, tok2 := startServerWithReg(t, reg)
	r2, _ := http.NewRequest("POST", fmt.Sprintf("http://%s/v1/agents/%d", addr2, id), strings.NewReader(`{}`))
	r2.Header.Set("Authorization", "Bearer "+tok2)
	r2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(r2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("/v1/agents/{id} POST status = %d, want 405", resp2.StatusCode)
	}
}

// mgr-nil short-circuit: 503 on all six endpoints when the run
// manager (and therefore the agents layer it owns) is not wired.
func TestAgentsHandlersReturn503WithoutManager(t *testing.T) {
	srv, closeSrv := newServerWithoutManager(t)
	defer closeSrv()
	// Each request on its own transport so the stdlib connection
	// pool can't hold a closed connection across iterations (which
	// manifests as "EOF" on the second request).
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	cases := []struct{ method, path, body string }{
		{"GET", "/v1/agents", ""},
		{"POST", "/v1/agents", `{"name":"x","backend":"claude"}`},
		{"GET", "/v1/agents/1", ""},
		{"PATCH", "/v1/agents/1", `{}`},
		{"DELETE", "/v1/agents/1", ""},
		{"GET", "/v1/agents/1/runs", ""},
	}
	for _, tc := range cases {
		var body io.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
		}
		req, _ := http.NewRequest(tc.method, "http://"+srv.Addr+tc.path, body)
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+srv.Token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// --- helpers ---------------------------------------------------------

// doGET issues an authenticated GET and returns status + raw body.
func doGET(t *testing.T, addr, tok, path string) (int, []byte) {
	t.Helper()
	r, _ := http.NewRequest("GET", "http://"+addr+path, nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// doJSON issues an authenticated request with a JSON body. body may
// be nil for GET / DELETE; otherwise it is JSON-encoded.
func doJSON(t *testing.T, addr, tok, method, path string, body any) (int, []byte) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		payload = strings.NewReader(string(b))
	}
	r, _ := http.NewRequest(method, "http://"+addr+path, payload)
	r.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, rb
}

// mustAgent fetches the named agent and fails the test on error.
func mustAgent(t *testing.T, reg *agentregistry.Store, name string) agentregistry.Agent {
	t.Helper()
	a, err := reg.GetAgent(context.Background(), name)
	if err != nil {
		t.Fatalf("GetAgent %q: %v", name, err)
	}
	return a
}

// strPtr is a small helper for AgentPatch pointer fields.
func strPtr(s string) *string { return &s }

// compiled but unused right now — kept because some of the agent
// subrouter branches still need it as we widen coverage in
// follow-up PRs.
var _ slog.Level = 0
var _ = http.MethodGet
