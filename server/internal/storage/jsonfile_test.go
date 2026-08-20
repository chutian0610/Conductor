package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"conductor/server/internal/protocol"
)

// setupStorage resets CONDUCTOR_HOME to a temp dir and returns a
// fresh JsonFileStorage + its root.
func setupStorage(t *testing.T) (*JsonFileStorage, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CONDUCTOR_HOME", dir)
	s, err := NewJsonFileStorage()
	if err != nil {
		t.Fatalf("NewJsonFileStorage: %v", err)
	}
	return s, dir
}

// TestCreateRunHappyPath verifies the on-disk shape after CreateRun:
// runs/<runid>/state.json with Status=running + StartedAt set.
func TestCreateRunHappyPath(t *testing.T) {
	s, dir := setupStorage(t)
	ctx := context.Background()

	state, err := s.CreateRun(ctx, "abc123", "spec-foo", "hello")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if state.Status != RunStatusRunning {
		t.Errorf("Status = %v, want running", state.Status)
	}
	if state.StartedAt.IsZero() {
		t.Errorf("StartedAt is zero")
	}
	if _, err := os.Stat(filepath.Join(dir, "runs", "abc123", "state.json")); err != nil {
		t.Errorf("state.json not on disk: %v", err)
	}
}

// TestCreateRunDuplicateRejected guards against runID collisions
// (a stale file from a previous test, say).
func TestCreateRunDuplicateRejected(t *testing.T) {
	s, _ := setupStorage(t)
	ctx := context.Background()

	if _, err := s.CreateRun(ctx, "dup", "spec", "p"); err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}
	_, err := s.CreateRun(ctx, "dup", "spec", "p")
	if err == nil {
		t.Errorf("expected duplicate error, got nil")
	}
}

// TestGetRunRoundTrip reads back a freshly-created run.
func TestGetRunRoundTrip(t *testing.T) {
	s, _ := setupStorage(t)
	ctx := context.Background()

	original, err := s.CreateRun(ctx, "rt", "spec", "round-trip")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got, err := s.GetRun(ctx, "rt")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.RunID != original.RunID {
		t.Errorf("RunID = %q, want %q", got.RunID, original.RunID)
	}
	if got.Prompt != "round-trip" {
		t.Errorf("Prompt = %q", got.Prompt)
	}
	if got.Status != RunStatusRunning {
		t.Errorf("Status = %v", got.Status)
	}
}

// TestGetRunNotFound — missing runID is ErrRunNotFound, not some
// opaque fs error.
func TestGetRunNotFound(t *testing.T) {
	s, _ := setupStorage(t)
	_, err := s.GetRun(context.Background(), "ghost")
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}

// TestUpdateRunAppliesAndPersists verifies the read-modify-write
// semantics: mutate fields, write back, GetRun reflects them.
func TestUpdateRunAppliesAndPersists(t *testing.T) {
	s, _ := setupStorage(t)
	ctx := context.Background()

	if _, err := s.CreateRun(ctx, "u", "spec", "p"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	now := time.Now().UTC()
	err := s.UpdateRun(ctx, "u", func(rs *RunState) {
		rs.Status = RunStatusCompleted
		rs.SessionID = "thr-xyz"
		rs.FinishedAt = &now
		rs.Result = &protocol.AgentTurnResult{
			SessionID: "thr-xyz",
			Usage:     protocol.AgentUsage{InputTokens: 7, OutputTokens: 3},
			Finish:    protocol.AgentTurnFinish{Reason: "end_turn", Success: true},
		}
	})
	if err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	got, err := s.GetRun(ctx, "u")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != RunStatusCompleted {
		t.Errorf("Status = %v", got.Status)
	}
	if got.SessionID != "thr-xyz" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.Result == nil || got.Result.Usage.InputTokens != 7 {
		t.Errorf("Result not persisted: %+v", got.Result)
	}
}

// TestUpdateRunConcurrentSerializes verifies that two concurrent
// UpdateRun calls for the SAME runId don't tear state.json —
// the per-runID mutex makes them sequential.
func TestUpdateRunConcurrentSerializes(t *testing.T) {
	s, _ := setupStorage(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "race", "spec", "p"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.UpdateRun(ctx, "race", func(rs *RunState) {
				rs.SessionID = rs.SessionID + string(rune('a'+i))
			})
		}(i)
	}
	wg.Wait()

	got, err := s.GetRun(ctx, "race")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(got.SessionID) != 10 {
		t.Errorf("SessionID = %q (len=%d), want 10 chars (10 sequential updates)",
			got.SessionID, len(got.SessionID))
	}
}

// TestUpdateRunFnNil guards against the obvious foot-gun.
func TestUpdateRunFnNil(t *testing.T) {
	s, _ := setupStorage(t)
	if _, err := s.CreateRun(context.Background(), "x", "s", "p"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.UpdateRun(context.Background(), "x", nil); err == nil {
		t.Errorf("expected error for nil fn")
	}
}

// TestUpdateRunNotFound covers the error path when the run was
// never created.
func TestUpdateRunNotFound(t *testing.T) {
	s, _ := setupStorage(t)
	err := s.UpdateRun(context.Background(), "ghost", func(*RunState) {})
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}

// TestAppendTimelineAndRead verifies the append-only NDJSON loop.
func TestAppendTimelineAndRead(t *testing.T) {
	s, _ := setupStorage(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "tl", "spec", "p"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	want := []protocol.AgentStreamEvent{
		{Kind: protocol.EventText, Text: "hello"},
		{Kind: protocol.EventToolCall, ToolName: "bash", ToolArgs: map[string]any{"cmd": "ls"}},
		{Kind: protocol.EventToolResult, ToolResult: "file1\nfile2"},
	}
	for _, ev := range want {
		if err := s.AppendTimeline(ctx, "tl", TimelineItem{
			TS:    time.Now().UTC(),
			Event: ev,
		}); err != nil {
			t.Fatalf("AppendTimeline: %v", err)
		}
	}

	r, err := s.ReadTimeline(ctx, "tl")
	if err != nil {
		t.Fatalf("ReadTimeline: %v", err)
	}
	defer r.Close()
	var got []protocol.AgentStreamEvent
	for {
		item, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, item.Event)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Kind != want[i].Kind {
			t.Errorf("event[%d].Kind = %v, want %v", i, got[i].Kind, want[i].Kind)
		}
	}
}


// TestReadTimelineEmptyForNewRun — a freshly-created run has no
// timeline yet, ReadTimeline should return an empty reader (immediate
// EOF), not an error.
func TestReadTimelineEmptyForNewRun(t *testing.T) {
	s, _ := setupStorage(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "empty", "s", "p"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := s.ReadTimeline(ctx, "empty")
	if err != nil {
		t.Fatalf("ReadTimeline on fresh run: %v", err)
	}
	defer r.Close()
	_, err = r.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected immediate EOF, got %v", err)
	}
}

// TestListRunsSortedAndFiltered exercises the filter logic + ordering.
func TestListRunsSortedAndFiltered(t *testing.T) {
	s, _ := setupStorage(t)
	ctx := context.Background()
	// Three runs across two specs.
	for _, run := range []struct {
		id, spec, prompt string
		delay            time.Duration
	}{
		{"r1", "specA", "first", 0},
		{"r2", "specA", "second", 10 * time.Millisecond},
		{"r3", "specB", "third", 20 * time.Millisecond},
	} {
		if _, err := s.CreateRun(ctx, run.id, run.spec, run.prompt); err != nil {
			t.Fatalf("CreateRun %s: %v", run.id, err)
		}
		time.Sleep(run.delay)
	}

	t.Run("all", func(t *testing.T) {
		all, err := s.ListRuns(ctx, RunFilter{})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(all) != 3 {
			t.Errorf("len = %d, want 3", len(all))
		}
		// Newest first → r3, r2, r1.
		if all[0].RunID != "r3" || all[1].RunID != "r2" || all[2].RunID != "r1" {
			t.Errorf("order = [%s, %s, %s], want [r3, r2, r1]", all[0].RunID, all[1].RunID, all[2].RunID)
		}
	})

	t.Run("spec filter", func(t *testing.T) {
		a, err := s.ListRuns(ctx, RunFilter{SpecID: "specA"})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(a) != 2 {
			t.Errorf("len = %d, want 2 (specA only)", len(a))
		}
		for _, r := range a {
			if r.SpecID != "specA" {
				t.Errorf("r.SpecID = %s, want specA", r.SpecID)
			}
		}
	})

	t.Run("status filter", func(t *testing.T) {
		// All three are running. Mark one completed.
		if err := s.UpdateRun(ctx, "r2", func(rs *RunState) {
			rs.Status = RunStatusCompleted
		}); err != nil {
			t.Fatalf("UpdateRun: %v", err)
		}
		c, err := s.ListRuns(ctx, RunFilter{Status: []RunStatus{RunStatusCompleted}})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(c) != 1 || c[0].RunID != "r2" {
			t.Errorf("got %v, want [r2]", c)
		}
	})

	t.Run("limit", func(t *testing.T) {
		limited, err := s.ListRuns(ctx, RunFilter{Limit: 2})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(limited) != 2 {
			t.Errorf("len = %d, want 2", len(limited))
		}
	})

	t.Run("orphan skipped", func(t *testing.T) {
		// Create a stray directory under runs/ with no state.json.
		if err := os.MkdirAll(filepath.Join(s.runsDir(), "orphan"), 0o700); err != nil {
			t.Fatalf("mkdir orphan: %v", err)
		}
		all, err := s.ListRuns(ctx, RunFilter{})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(all) != 3 {
			t.Errorf("orphan should be skipped, got %d", len(all))
		}
	})
}

// TestNewRunIDUnique verifies NewRunID gives different ids on
// repeated calls (sanity check on crypto/rand.Read).
func TestNewRunIDUnique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		id := NewRunID()
		if seen[id] {
			t.Fatalf("duplicate id: %s", id)
		}
		seen[id] = true
	}
}

// TestNewJsonFileStorageCreatesRunsDir covers the EnsureBaseDirs-
// like behavior on construction.
func TestNewJsonFileStorageCreatesRunsDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONDUCTOR_HOME", dir)
	s, err := NewJsonFileStorage()
	if err != nil {
		t.Fatalf("NewJsonFileStorage: %v", err)
	}
	if s == nil {
		t.Fatalf("nil storage")
	}
	info, err := os.Stat(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatalf("runs dir missing: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("runs is not a dir")
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("runs perms = %o, want 0700", perm)
	}
}

// TestAtomicStateWriteCrashSafety covers the tmp+rename guarantee.
// If the process dies after writing tmp but before renaming, the
// next GetRun must not see the half-written file.
func TestAtomicStateWriteCrashSafety(t *testing.T) {
	s, dir := setupStorage(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "crash", "s", "p"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Simulate a crash mid-write: leave state.json.tmp behind.
	tmp := filepath.Join(dir, "runs", "crash", "state.json.tmp")
	if err := os.WriteFile(tmp, []byte("{partial"), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	got, err := s.GetRun(ctx, "crash")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.RunID != "crash" {
		t.Errorf("RunID = %q", got.RunID)
	}
	// state.json.tmp should be ignored — the real state.json is intact.
	data, _ := os.ReadFile(filepath.Join(dir, "runs", "crash", "state.json"))
	var st RunState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("state.json corrupted: %v", err)
	}
	// (we just want the file to be parseable; full content is
	// tested by TestGetRunRoundTrip)
	var buf bytes.Buffer
	_ = json.Indent(&buf, data, "", "  ")
	if !bytes.Contains(buf.Bytes(), []byte("runId")) {
		t.Errorf("state.json doesn't look right: %s", buf.String())
	}
}

// TestLookupSessionIDHappyPath verifies the resume-by-runId
// helper returns the recorded sessionId.
func TestLookupSessionIDHappyPath(t *testing.T) {
	s, _ := setupStorage(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "rt", "spec", "p"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.UpdateRun(ctx, "rt", func(rs *RunState) {
		rs.SessionID = "thr-abc"
		rs.Status = RunStatusCompleted
	}); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	got, err := s.LookupSessionID(ctx, "rt")
	if err != nil {
		t.Fatalf("LookupSessionID: %v", err)
	}
	if got != "thr-abc" {
		t.Errorf("sessionId = %q, want thr-abc", got)
	}
}

// TestLookupSessionIDMissing covers runs without a sessionId
// (still running, or failed before turn/completed).
func TestLookupSessionIDMissing(t *testing.T) {
	s, _ := setupStorage(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "norun", "spec", "p"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Status=running, SessionID="".
	_, err := s.LookupSessionID(ctx, "norun")
	if !errors.Is(err, ErrSessionIDMissing) {
		t.Errorf("err = %v, want ErrSessionIDMissing", err)
	}
}

// TestLookupSessionIDNotFound covers unknown runId.
func TestLookupSessionIDNotFound(t *testing.T) {
	s, _ := setupStorage(t)
	_, err := s.LookupSessionID(context.Background(), "ghost")
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}
