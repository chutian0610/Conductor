package httpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"conductor/server/internal/agentregistry"
	"conductor/server/internal/httpserver"
)

// --- GET /v1/runs/{id}/audit --------------------------------------------

func TestRunAudit404UnknownRun(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/runs/9999/audit", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRunAudit404WhenNeverAudited(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "audit-not-yet")
	runID, _ := reg.StartRun(context.Background(), agentregistry.Run{
		AgentID: id, Status: "completed", StartedAt: time.Now().UTC(),
	})
	addr, tok := startServerWithReg(t, reg)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/v1/runs/%d/audit", addr, runID), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (run exists, no audit)", resp.StatusCode)
	}
}

func TestRunAudit200WhenAudited(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "audit-present")
	runID, _ := reg.StartRun(context.Background(), agentregistry.Run{
		AgentID: id, Status: "completed", StartedAt: time.Now().UTC(),
	})
	auditID, err := reg.StartAudit(context.Background(), agentregistry.RunAudit{
		RunID:        runID,
		AuditorModel: "claude-3-7-sonnet-test",
		InputSHA:     "sha-input",
		PromptSHA:    "sha-prompt",
	})
	if err != nil {
		t.Fatalf("StartAudit: %v", err)
	}
	if err := reg.FinishAudit(context.Background(), auditID, "pass", "looks good to me"); err != nil {
		t.Fatalf("FinishAudit: %v", err)
	}

	addr, tok := startServerWithReg(t, reg)
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/v1/runs/%d/audit", addr, runID), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body agentregistry.RunAudit
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Verdict != "pass" {
		t.Errorf("verdict = %q, want pass", body.Verdict)
	}
	if body.RunID != runID {
		t.Errorf("run_id = %d, want %d", body.RunID, runID)
	}
}

func TestRunAuditRequiresAuth(t *testing.T) {
	addr, _ := startServerWithReg(t, openRegOnly(t))
	resp, err := http.Get("http://" + addr + "/v1/runs/1/audit")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// --- POST /v1/runs/{id}/audit:run ---------------------------------------

// POST to a non-existent run returns 404. We do not spawn the auditor:
// the run-resolution check runs first and short-circuits.
func TestRunAuditRun404UnknownRun(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	body := strings.NewReader(`{}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/runs/9999/audit:run", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// End-to-end POST that actually spawns the auditor needs a
// fake-auditor-binary harness; deliberately out of scope here. The
// integration test in cmd/conductor is the canonical surface for
// that path. We assert the wire-level guards the handler enforces
// before calling audit.Run: 405 on wrong method, 400 on unknown
// JSON fields (DisallowUnknownFields).
func TestRunAuditRunRejectsGET(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "audit-method")
	runID, _ := reg.StartRun(context.Background(), agentregistry.Run{
		AgentID: id, Status: "completed", StartedAt: time.Now().UTC(),
	})
	addr, tok := startServerWithReg(t, reg)
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/v1/runs/%d/audit:run", addr, runID), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestRunAuditRunRejectsUnknownField(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "audit-extra")
	runID, _ := reg.StartRun(context.Background(), agentregistry.Run{
		AgentID: id, Status: "completed", StartedAt: time.Now().UTC(),
	})
	addr, tok := startServerWithReg(t, reg)
	body := strings.NewReader(`{"force":true,"bogus":1}`)
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/v1/runs/%d/audit:run", addr, runID), body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (DisallowUnknownFields)", resp.StatusCode)
	}
}

// --- GET /v1/audits/pending --------------------------------------------

func TestAuditsPendingEmpty(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/audits/pending", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Pending []int64 `json:"pending"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Pending == nil {
		t.Error("pending is nil; want []")
	}
	if len(body.Pending) != 0 {
		t.Errorf("len(pending) = %d, want 0", len(body.Pending))
	}
}

func TestAuditsPendingSeeded(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "pending-1")
	// Three runs all unaudited: they should all show up.
	var ids []int64
	for i := 0; i < 3; i++ {
		runID, err := reg.StartRun(context.Background(), agentregistry.Run{
			AgentID: id, Status: "completed", StartedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, runID)
	}

	addr, tok := startServerWithReg(t, reg)
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/audits/pending", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Pending []int64 `json:"pending"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	// Other tests may leave state in this shared fresh registry, so
	// we only assert that *all* of our seeded ids are in the list,
	// not that the list equals exactly those ids.
	got := make(map[int64]bool)
	for _, x := range body.Pending {
		got[x] = true
	}
	for _, want := range ids {
		if !got[want] {
			t.Errorf("seeded run id %d missing from pending list", want)
		}
	}
}

func TestAuditsPendingRequiresAuth(t *testing.T) {
	addr, _ := startServerWithReg(t, openRegOnly(t))
	resp, err := http.Get("http://" + addr + "/v1/audits/pending")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// mgr-nil short-circuit: every audit handler must return 503 when
// the run manager is not wired (server constructed with mgr=nil).
//
// newServerWithoutManager is a thin test helper distinct from
// startServerWithReg (which always passes a real mgr); we live
// in this file because both helpers are audit-specific and the
// lifecycle is short.
func TestAuditHandlersReturn503WithoutManager(t *testing.T) {
	srv, closeSrv := newServerWithoutManager(t)
	defer closeSrv()

	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/runs/1/audit"},
		{http.MethodPost, "/v1/runs/1/audit:run"},
		{http.MethodGet, "/v1/audits/pending"},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, "http://"+srv.Addr+tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+srv.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// srv is the listen-and-bound context used by the 503-without-manager
// test above; helpers for closing live in newServerWithoutManager.
type testSrv struct{ Addr, Token string }

func newServerWithoutManager(t *testing.T) (*testSrv, func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	token := "tok-no-mgr"
	srv, err := httpserver.New("127.0.0.1:0", token, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln, 100*time.Millisecond) }()
	time.Sleep(5 * time.Millisecond)
	return &testSrv{Addr: ln.Addr().String(), Token: token}, func() {
		cancel()
		<-done
		_ = ln.Close()
	}
}
