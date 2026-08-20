package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"conductor/server/internal/protocol"
	"conductor/server/internal/storage"
)

// lsSetup creates a fresh storage + several runs across two specs
// + one orphan. Returns the storage so individual tests can
// mutate state.
func lsSetup(t *testing.T) storage.Storage {
	t.Helper()
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	s, err := storage.NewJsonFileStorage()
	if err != nil {
		t.Fatalf("NewJsonFileStorage: %v", err)
	}

	// Three runs across two specs, with intentional status variation.
	now := time.Now().UTC()
	for _, r := range []struct {
		id, specID, prompt, sessionID string
		status                       storage.RunStatus
		startedAt                    time.Time
		finishedAt                   *time.Time
	}{
		{
			id: "aaa111", specID: "specA", prompt: "first run",
			sessionID: "thr-aaa", status: storage.RunStatusCompleted,
			startedAt: now.Add(-3 * time.Minute),
		},
		{
			id: "bbb222", specID: "specA", prompt: "second run",
			sessionID: "thr-bbb", status: storage.RunStatusRunning,
			startedAt: now.Add(-2 * time.Minute),
		},
		{
			id: "ccc333", specID: "specB", prompt: "third",
			sessionID: "thr-ccc", status: storage.RunStatusFailed,
			startedAt: now.Add(-1 * time.Minute),
		},
	} {
		_, err := s.CreateRun(context.Background(), r.id, r.specID, r.prompt)
		if err != nil {
			t.Fatalf("CreateRun %s: %v", r.id, err)
		}
		err = s.UpdateRun(context.Background(), r.id, func(rs *storage.RunState) {
			rs.Status = r.status
			rs.SessionID = r.sessionID
			if r.finishedAt != nil {
				rs.FinishedAt = r.finishedAt
			}
		})
		if err != nil {
			t.Fatalf("UpdateRun %s: %v", r.id, err)
		}
	}

	// Append one timeline item to aaa111 (so show mode has data).
	if err := s.AppendTimeline(context.Background(), "aaa111", storage.TimelineItem{
		TS: now,
		Event: protocol.AgentStreamEvent{
			Kind: protocol.EventText,
			Text: "hello",
		},
	}); err != nil {
		t.Fatalf("AppendTimeline: %v", err)
	}

	return s
}

// TestLsEmpty covers the (no runs) branch — needs a clean storage
// without any seeded runs.
func TestLsEmpty(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if err := runLsWithWriter(context.Background(), nil, &out, &errOut); err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(out.String(), "no runs") {
		t.Errorf("output should say '(no runs)', got %q", out.String())
	}
}

// TestLsTable — verify the table header + at least one data row.
func TestLsTable(t *testing.T) {
	lsSetup(t)

	var out, errOut bytes.Buffer
	if err := runLsWithWriter(context.Background(), nil, &out, &errOut); err != nil {
		t.Fatalf("ls: %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"RUN ID", "SPEC ID", "STATUS", "STARTED", "DURATION", "PROMPT", "SESSION",
		"aaa111", "bbb222", "ccc333",
		"completed", "running", "failed",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("table missing %q:\n%s", want, s)
		}
	}
	// Newest first: ccc333 then bbb222 then aaa111.
	idx1 := strings.Index(s, "ccc333")
	idx2 := strings.Index(s, "bbb222")
	idx3 := strings.Index(s, "aaa111")
	if !(idx1 < idx2 && idx2 < idx3) {
		t.Errorf("expected newest-first order, got idx ccc=%d bbb=%d aaa=%d", idx1, idx2, idx3)
	}
}

// TestLsFilterSpec narrows the listing to one spec.
func TestLsFilterSpec(t *testing.T) {
	lsSetup(t)

	var out, errOut bytes.Buffer
	err := runLsWithWriter(context.Background(),
		[]string{"--spec", "specA"}, &out, &errOut)
	if err != nil {
		t.Fatalf("ls --spec: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "aaa111") || !strings.Contains(s, "bbb222") {
		t.Errorf("specA runs should be present, got %q", s)
	}
	if strings.Contains(s, "ccc333") {
		t.Errorf("specB run ccc333 should be filtered out, got %q", s)
	}
}

// TestLsFilterStatus — multi-status filter (csv).
func TestLsFilterStatus(t *testing.T) {
	lsSetup(t)

	var out, errOut bytes.Buffer
	err := runLsWithWriter(context.Background(),
		[]string{"--status", "running,failed"}, &out, &errOut)
	if err != nil {
		t.Fatalf("ls --status: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "bbb222") || !strings.Contains(s, "ccc333") {
		t.Errorf("running+failed runs should be present, got %q", s)
	}
	if strings.Contains(s, "aaa111") {
		t.Errorf("completed run aaa111 should be filtered out, got %q", s)
	}
}

// TestLsFilterStatusBadValue — rejects unrecognized statuses.
func TestLsFilterStatusBadValue(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	err := runLsWithWriter(context.Background(),
		[]string{"--status", "running,bogus"}, &out, &errOut)
	if err == nil {
		t.Fatalf("expected error for bogus status value")
	}
	if !strings.Contains(errOut.String(), "is not one of") {
		t.Errorf("stderr should explain the bad status, got %q", errOut.String())
	}
}

// TestLsLimit — newest N only.
func TestLsLimit(t *testing.T) {
	lsSetup(t)

	var out, errOut bytes.Buffer
	err := runLsWithWriter(context.Background(),
		[]string{"--limit", "2"}, &out, &errOut)
	if err != nil {
		t.Fatalf("ls --limit: %v", err)
	}
	s := out.String()
	// Header should appear once + data rows. tabwriter pads
	// with spaces (not trailing tabs), so check the substring.
	hdrCount := strings.Count(s, "RUN ID  SPEC ID")
	if hdrCount != 1 {
		t.Errorf("expected 1 header line, got %d occurrences in %q", hdrCount, s)
	}
	if strings.Contains(s, "aaa111") {
		t.Errorf("limit=2 should exclude oldest run aaa111, got %q", s)
	}
	if !strings.Contains(s, "bbb222") || !strings.Contains(s, "ccc333") {
		t.Errorf("limit=2 should include newest 2 runs, got %q", s)
	}
}

// TestLsJSON — machine-readable output: one RunState per line,
// each line parseable.
func TestLsJSON(t *testing.T) {
	lsSetup(t)

	var out, errOut bytes.Buffer
	err := runLsWithWriter(context.Background(),
		[]string{"--json"}, &out, &errOut)
	if err != nil {
		t.Fatalf("ls --json: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	seen := make(map[string]bool)
	for _, line := range lines {
		var rs storage.RunState
		if err := json.Unmarshal([]byte(line), &rs); err != nil {
			t.Errorf("line %q not valid JSON: %v", line, err)
			continue
		}
		seen[rs.RunID] = true
	}
	for _, want := range []string{"aaa111", "bbb222", "ccc333"} {
		if !seen[want] {
			t.Errorf("missing %s in JSON output", want)
		}
	}
}

// TestLsShow — <runId> arg switches to show mode (state + timeline).
func TestLsShow(t *testing.T) {
	lsSetup(t)

	var out, errOut bytes.Buffer
	err := runLsWithWriter(context.Background(),
		[]string{"aaa111"}, &out, &errOut)
	if err != nil {
		t.Fatalf("ls aaa111: %v", err)
	}
	s := out.String()
	// state.json header + raw RunState fields.
	for _, want := range []string{
		"--- state.json ---",
		`"runId": "aaa111"`,
		`"status": "completed"`,
		`"sessionId": "thr-aaa"`,
		"--- timeline.ndjson ---",
		`"kind":"text"`,
		`"text":"hello"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("show output missing %q", want)
		}
	}
}

// TestLsShowJSON — show mode with --json emits the state +
// timeline envelope.
func TestLsShowJSON(t *testing.T) {
	lsSetup(t)

	var out, errOut bytes.Buffer
	err := runLsWithWriter(context.Background(),
		[]string{"--json", "aaa111"}, &out, &errOut)
	if err != nil {
		t.Fatalf("ls --json aaa111: %v", err)
	}
	var envelope struct {
		State    storage.RunState       `json:"state"`
		Timeline []storage.TimelineItem `json:"timeline"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("envelope not valid JSON: %v\n%s", err, out.String())
	}
	if envelope.State.RunID != "aaa111" {
		t.Errorf("state.RunID = %q", envelope.State.RunID)
	}
	if len(envelope.Timeline) != 1 {
		t.Errorf("len(timeline) = %d, want 1", len(envelope.Timeline))
	}
	if envelope.Timeline[0].Event.Kind != protocol.EventText {
		t.Errorf("timeline[0].Event.Kind = %v", envelope.Timeline[0].Event.Kind)
	}
}

// TestLsShowNotFound — show mode on a missing runId is a clear error.
func TestLsShowNotFound(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	err := runLsWithWriter(context.Background(),
		[]string{"ghost-run"}, &out, &errOut)
	if err == nil {
		t.Fatalf("expected error for missing runId")
	}
	if !strings.Contains(errOut.String(), "not found") {
		t.Errorf("stderr should mention not found, got %q", errOut.String())
	}
}

// TestLsHelp — --help prints usage and exits 0.
func TestLsHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := runLsWithWriter(context.Background(),
		[]string{"--help"}, &out, &errOut); err != nil {
		t.Fatalf("--help: %v", err)
	}
	if !strings.Contains(errOut.String(), "conductor ls — list stored runs") {
		t.Errorf("--help should print usage, got %q", errOut.String())
	}
}

// TestLsTooManyArgs — extra positional args beyond <runId> error out.
func TestLsTooManyArgs(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	err := runLsWithWriter(context.Background(),
		[]string{"aaa111", "extra"}, &out, &errOut)
	if err == nil {
		t.Fatalf("expected error for too many args")
	}
	if !strings.Contains(errOut.String(), "at most one runId") {
		t.Errorf("stderr should explain, got %q", errOut.String())
	}
}
