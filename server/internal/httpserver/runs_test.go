package httpserver_test

import (
	"bufio"
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
	"conductor/server/internal/runmgr"
)

// seedAgent inserts a Claude agent named name into reg and returns
// its assigned ID. The agent is kept open until the test ends.
func seedAgent(t *testing.T, reg *agentregistry.Store, name string) int64 {
	t.Helper()
	_, err := reg.RegisterAgent(context.Background(),
		agentregistry.Agent{Name: name, Backend: "claude"})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	agent, err := reg.GetAgent(context.Background(), name)
	if err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	return agent.ID
}

func TestRunsListRequiresAuth(t *testing.T) {
	reg := openRegOnly(t)
	addr, _ := startServerWithReg(t, reg)

	resp, err := http.Get("http://" + addr + "/v1/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRunsListEmpty(t *testing.T) {
	reg := openRegOnly(t)
	addr, tok := startServerWithReg(t, reg)

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/runs", nil)
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
		Runs []agentregistry.Run `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Runs == nil {
		t.Error("runs is nil; want []")
	}
	if len(body.Runs) != 0 {
		t.Errorf("len(runs) = %d, want 0", len(body.Runs))
	}
}

func TestRunsListWithSeed(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "s1")
	for i := 0; i < 3; i++ {
		_, err := reg.StartRun(context.Background(), agentregistry.Run{
			AgentID: id, Status: "running", StartedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	addr, tok := startServerWithReg(t, reg)

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/runs?agent=s1", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Runs []agentregistry.Run `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Runs) != 3 {
		t.Errorf("len(runs) = %d, want 3", len(body.Runs))
	}
}

func TestRunResult425WhileRunning(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "r1")
	runID, err := reg.StartRun(context.Background(), agentregistry.Run{
		AgentID: id, Status: "running", StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, tok := startServerWithReg(t, reg)

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://%s/v1/runs/%d/result", addr, runID), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooEarly {
		t.Errorf("status = %d, want 425", resp.StatusCode)
	}
}

func TestRunResult200WhenFinished(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "f1")
	runID, _ := reg.StartRun(context.Background(), agentregistry.Run{
		AgentID: id, Status: "running", StartedAt: time.Now().UTC(),
	})
	_ = reg.FinishRun(context.Background(), runID,
		agentregistry.RunFinish{Status: "completed", DurationMs: 17})
	addr, tok := startServerWithReg(t, reg)

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://%s/v1/runs/%d/result", addr, runID), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRunEventsReplayByAfterSeq(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "e1")
	runID, _ := reg.StartRun(context.Background(), agentregistry.Run{
		AgentID: id, Status: "running", StartedAt: time.Now().UTC(),
	})
	for _, k := range []string{"system", "assistant", "result"} {
		_ = reg.AppendEvent(context.Background(), runID, k,
			[]byte(`{"k":"`+k+`"}`))
	}
	addr, tok := startServerWithReg(t, reg)

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://%s/v1/runs/%d/events?after_seq=1", addr, runID), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Events []agentregistry.Event `json:"events"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Events) != 2 {
		t.Errorf("len(events) = %d, want 2 (after seq 1)", len(body.Events))
	}
}

func TestVersionPublicNoAuth(t *testing.T) {
	addr, _ := startServerWithReg(t, openRegOnly(t))
	resp, err := http.Get("http://" + addr + "/v1/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRunStart404UnknownAgent(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	body := strings.NewReader(`{"agent":"ghost"}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/runs", body)
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

func TestRunStart400MissingAgent(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	body := strings.NewReader(`{"prompt":"hi"}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/runs", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// --- SSE: replay a finished run ------------------------------------------

func TestStreamReplaysFinishedRun(t *testing.T) {
	reg := openRegOnly(t)
	id := seedAgent(t, reg, "sse-fin")
	runID, _ := reg.StartRun(context.Background(), agentregistry.Run{
		AgentID: id, Status: "running", StartedAt: time.Now().UTC(),
	})
	for _, k := range []string{"system", "assistant", "result"} {
		_ = reg.AppendEvent(context.Background(), runID, k,
			[]byte(`{"k":"`+k+`"}`))
	}
	_ = reg.FinishRun(context.Background(), runID,
		agentregistry.RunFinish{Status: "completed"})
	addr, tok := startServerWithReg(t, reg)

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://%s/v1/runs/%d/stream", addr, runID), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	scanner := bufio.NewScanner(resp.Body)
	idSeen := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id:") {
			idSeen++
		}
	}
	if idSeen != 3 {
		t.Errorf("saw %d SSE id-lines, want 3", idSeen)
	}
}

func TestStream404UnknownRun(t *testing.T) {
	addr, tok := startServerWithReg(t, openRegOnly(t))
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/runs/9999/stream", nil)
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

// --- helpers -------------------------------------------------------------

func openRegOnly(t *testing.T) *agentregistry.Store {
	reg, err := agentregistry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func startServerWithReg(t *testing.T, reg *agentregistry.Store) (addr, token string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := runmgr.New(reg, logger)
	token = fmt.Sprintf("tok-%d", time.Now().UnixNano())
	srv, err := httpserver.New("127.0.0.1:0", token, logger, mgr)
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
	t.Cleanup(func() { cancel(); <-done; _ = ln.Close() })
	return ln.Addr().String(), token
}
