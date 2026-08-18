package runmgr_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"conductor/server/internal/agentregistry"
	"conductor/server/internal/runmgr"
)

// stubBackendFactory lets unit tests redirect backend instantiation.
// It returns the same backend type every call; per-test wiring happens
// by setting Manager.BackendFactory. Production wiring in cmd/conductor
// uses the default factory (calls backend.New).
//

func openStore(t *testing.T) *agentregistry.Store {
	t.Helper()
	reg, err := agentregistry.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func registerAgent(t *testing.T, reg *agentregistry.Store, name, backendType string) {
	t.Helper()
	_, err := reg.RegisterAgent(context.Background(), agentregistry.Agent{
		Name:    name,
		Backend: backendType,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestStartRunAgentNotFound(t *testing.T) {
	reg := openStore(t)
	mgr := runmgr.New(reg, silentLogger())

	_, err := mgr.StartRun(context.Background(), runmgr.StartRequest{
		AgentName: "ghost",
	})
	if err == nil {
		t.Fatal("expected ErrAgentNotFound")
	}
	if !errors.Is(err, runmgr.ErrAgentNotFound) {
		t.Errorf("err = %v, want ErrAgentNotFound", err)
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		err = unwrap(err)
	}
	return false
}
func unwrap(err error) error {
	type unwrapper interface{ Unwrap() error }
	for {
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
	return err
}

func TestStartRunRequiresAgentName(t *testing.T) {
	reg := openStore(t)
	mgr := runmgr.New(reg, silentLogger())

	_, err := mgr.StartRun(context.Background(), runmgr.StartRequest{})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestListRunsDelegatesToStore(t *testing.T) {
	reg := openStore(t)
	registerAgent(t, reg, "a1", "claude")
	registerAgent(t, reg, "a2", "claude")
	mgr := runmgr.New(reg, silentLogger())

	// Pre-seed two runs directly through the store so we test
	// the delegation contract without touching backend.New.
	for _, name := range []string{"a1", "a2"} {
		a, _ := reg.GetAgent(context.Background(), name)
		_, err := reg.StartRun(context.Background(), agentregistry.Run{
			AgentID: a.ID, Status: "running", StartedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	runs, err := mgr.ListRuns(context.Background(), "", agentregistry.ListRunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Errorf("got %d runs, want 2", len(runs))
	}
}

func TestEventsAfterFiltersBySeq(t *testing.T) {
	reg := openStore(t)
	registerAgent(t, reg, "a1", "claude")
	mgr := runmgr.New(reg, silentLogger())

	a, _ := reg.GetAgent(context.Background(), "a1")
	runID, err := reg.StartRun(context.Background(), agentregistry.Run{
		AgentID: a.ID, Status: "running", StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"system", "assistant", "result"} {
		_ = reg.AppendEvent(context.Background(), runID, k, []byte(`{}`))
	}

	after, err := mgr.EventsAfter(context.Background(), runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Errorf("got %d events, want 2 (after seq 1)", len(after))
	}
	if after[0].Seq != 2 || after[1].Seq != 3 {
		t.Errorf("seq = %d, %d; want 2, 3", after[0].Seq, after[1].Seq)
	}
}

func TestActiveCountStartsZero(t *testing.T) {
	reg := openStore(t)
	mgr := runmgr.New(reg, silentLogger())
	if n := mgr.ActiveCount(); n != 0 {
		t.Errorf("ActiveCount = %d, want 0", n)
	}
}

func TestSubscribeReplaysThenClosesForFinishedRun(t *testing.T) {
	reg := openStore(t)
	registerAgent(t, reg, "a1", "claude")
	mgr := runmgr.New(reg, silentLogger())

	a, _ := reg.GetAgent(context.Background(), "a1")
	runID, err := reg.StartRun(context.Background(), agentregistry.Run{
		AgentID: a.ID, Status: "running", StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"system", "assistant"} {
		_ = reg.AppendEvent(context.Background(), runID, k, []byte(`{"k":"`+k+`"}`))
	}
	if err := reg.FinishRun(context.Background(), runID, agentregistry.RunFinish{Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	res, err := mgr.Subscribe(context.Background(), runID, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsActive {
		t.Error("IsActive = true on a finished run; want false")
	}
	// Read all events then expect close.
	want := 0
	for bytes := range res.Events {
		_ = bytes
		want++
	}
	if want != 2 {
		t.Errorf("got %d SSE wire chunks, want 2", want)
	}
}

// ensure sync.WaitGroup does not trigger unused-import false positives
// (kept for symmetry with the production lock pattern runstate.go uses).
var _ = sync.WaitGroup{}
