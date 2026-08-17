package agentregistry_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"conductor/server/internal/agentregistry"
)

func registerOne(t *testing.T, s *agentregistry.Store) int64 {
	t.Helper()
	id, err := s.RegisterAgent(context.Background(), agentregistry.Agent{
		Name: "test-agent", Backend: "claude",
	})
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	return id
}

func TestRunLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	agentID := registerOne(t, s)

	runID, err := s.StartRun(ctx, agentregistry.Run{
		AgentID:   agentID,
		PromptSHA: agentregistry.ParsePrompt("hello"),
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if runID == 0 {
		t.Fatal("expected non-zero run id")
	}

	// Append a few events.
	for _, k := range []string{"text", "tool-use", "text"} {
		payload, _ := json.Marshal(map[string]any{"kind": k})
		if err := s.AppendEvent(ctx, runID, k, payload); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	// Finish.
	usage, _ := json.Marshal(map[string]any{"claude-sonnet-4-5": map[string]int64{
		"input_tokens": 10, "output_tokens": 20,
	}})
	if err := s.FinishRun(ctx, runID, agentregistry.RunFinish{
		Status: "completed", DurationMs: 1234, SessionID: "sess-X", Usage: usage,
	}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	got, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || got.DurationMs != 1234 || got.SessionID != "sess-X" {
		t.Fatalf("run not finished correctly: %+v", got)
	}
	if got.EventCount != 3 {
		t.Fatalf("event count: got %d, want 3", got.EventCount)
	}
	if !strings.Contains(string(got.Usage), "input_tokens") {
		t.Fatalf("usage empty: %s", got.Usage)
	}

	evs, err := s.Events(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("events: got %d, want 3", len(evs))
	}
	if evs[0].Kind != "text" || evs[2].Kind != "text" {
		t.Fatalf("event order/contents wrong: %+v", evs)
	}
}

func TestStartRun_MissingAgent(t *testing.T) {
	s := newStore(t)
	_, err := s.StartRun(context.Background(), agentregistry.Run{AgentID: 9999})
	if err == nil || !strings.Contains(err.Error(), "agent 9999 not found") {
		t.Fatalf("want missing-agent error, got %v", err)
	}
}

func TestFinishRun_Idempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	aID := registerOne(t, s)
	runID, _ := s.StartRun(ctx, agentregistry.Run{AgentID: aID})
	if err := s.FinishRun(ctx, runID, agentregistry.RunFinish{Status: "completed", DurationMs: 5}); err != nil {
		t.Fatalf("first FinishRun: %v", err)
	}
	if err := s.FinishRun(ctx, runID, agentregistry.RunFinish{Status: "completed", DurationMs: 99}); err != nil {
		t.Fatalf("second FinishRun (idempotent) should not error: %v", err)
	}
	got, _ := s.GetRun(ctx, runID)
	if got.DurationMs != 5 {
		t.Fatalf("first finish must win; got %d", got.DurationMs)
	}
}

func TestListRunsFiltersStatus(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	aID := registerOne(t, s)
	r1, _ := s.StartRun(ctx, agentregistry.Run{AgentID: aID})
	s.FinishRun(ctx, r1, agentregistry.RunFinish{Status: "completed", DurationMs: 1})
	r2, _ := s.StartRun(ctx, agentregistry.Run{AgentID: aID})
	s.FinishRun(ctx, r2, agentregistry.RunFinish{Status: "failed", DurationMs: 2})

	runs, err := s.ListRuns(ctx, "", agentregistry.ListRunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(runs))
	}
	// Newest first.
	if runs[0].ID < runs[1].ID {
		t.Fatalf("runs not sorted DESC by id: %v", runs)
	}

	failed, _ := s.ListRuns(ctx, "", agentregistry.ListRunOpts{Status: "failed"})
	if len(failed) != 1 {
		t.Fatalf("want 1 failed, got %d", len(failed))
	}
}

func TestGetRun_NotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetRun(context.Background(), 123)
	if !errors.Is(err, agentregistry.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
