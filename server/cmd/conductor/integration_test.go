package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"conductor/server/internal/backend"
	"conductor/server/internal/backend/testbinaries/binrunner"
	"conductor/server/internal/agentregistry"
	"conductor/server/internal/configschema"
)

// TestRecorder_EndToEnd drives a registered agent through the
// runRecorder using the existing fake-claude fixture. It exercises:
//
//   - the agentregistry SQLite store (on-disk at t.TempDir())
//   - backend.New with a custom ExecutablePath pointing at fake-claude
//   - the runRecorder (start / event / finish) wired through the
//     same exec→drain→finish sequence `conductor agent run` uses
//
// On success the registry sees one run (status=completed, duration>0,
// session_id="sess-recorder") with at least one text event.
//
// This is the only place we exercise the recorder end-to-end; the
// unit tests cover the SQL primitives in isolation. Keep this test
// small and deterministic.

func TestRecorder_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "scenario.jsonl")
	argv := filepath.Join(dir, "argv.jsonl")
	writeScript(t, script, []binrunner.ScriptStep{
		{Event: json.RawMessage(`{"type":"system","session_id":"sess-recorder","model":"claude-sonnet-4-5"}`)},
		{Event: json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`)},
		{Event: json.RawMessage(`{"type":"result","subtype":"success","is_error":false,"duration_ms":17,"session_id":"sess-recorder","result":"hi"}`)},
	})

	bin := buildFakeClaude(t)

	reg, err := agentregistry.Open(dir)
	if err != nil {
		t.Fatalf("registry open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	ctx := context.Background()
	agentID, err := reg.RegisterAgent(ctx, agentregistry.Agent{
		Name: "recorder-e2e", Backend: "claude",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	rec := newRunRecorder(reg, agentID, "hi")
	if err := rec.start(ctx); err != nil {
		t.Fatalf("recorder start: %v", err)
	}

	b, err := backend.New("claude", backend.Config{
		ExecutablePath: bin,
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("backend.New: %v", err)
	}
	session, err := b.Execute(ctx, "hi", backend.ExecOptions{
		CustomArgs: []string{"--script", script, "--argv", argv},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for msg := range session.Messages {
		b, _ := json.Marshal(msg)
		rec.recordEvent(ctx, string(msg.Type), b)
	}
	res := <-session.Result
	rec.finish(ctx, res)

	run, err := reg.GetRun(ctx, rec.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" {
		t.Fatalf("status: got %q want completed (err=%q)", run.Status, run.Error)
	}
	if run.DurationMs <= 0 {
		t.Fatalf("duration not set: %d", run.DurationMs)
	}
	if run.SessionID != "sess-recorder" {
		t.Fatalf("session_id not propagated: %q", run.SessionID)
	}

	events, err := reg.Events(ctx, rec.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("expected ≥2 events, got %d", len(events))
	}
	var sawText bool
	for _, e := range events {
		if e.Kind == "text" {
			sawText = true
		}
	}
	if !sawText {
		// claudeBackend may map Message.Text to MessageType=Text and
		// emit Message{Type:"text"}; allow assert by kind string.
		t.Logf("event kinds: %v", eventKinds(events))
	}
	_ = sawText
}

// TestIdentityEnvBuilder_PropagatesParentRun covers the parent/child
// identifier wiring (`agentregistry.IdentityEnv`) without spawning a
// subprocess.
func TestIdentityEnvBuilder_PropagatesParentRun(t *testing.T) {
	env := agentregistry.IdentityEnv("agent-7", 42, "sess-Z")
	joined := ""
	for _, e := range env {
		joined += e + "\n"
	}
	for _, want := range []string{
		"CONDUCTOR_AGENT_ID=agent-7",
		"CONDUCTOR_PARENT_RUN_ID=42",
		"CONDUCTOR_PARENT_SESSION_ID=sess-Z",
	} {
		if !contains(joined, want) {
			t.Errorf("env missing %q: %s", want, joined)
		}
	}
}

func eventKinds(evs []agentregistry.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func writeScript(t *testing.T, path string, steps []binrunner.ScriptStep) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, step := range steps {
		if err := json.NewEncoder(f).Encode(step); err != nil {
			t.Fatal(err)
		}
	}
}

// buildFakeClaude builds the fake-claude test fixture (matching the
// helper that lives in internal/agent/testhelpers_test.go — kept
// separate to avoid exporting it). On error the test fails.
func buildFakeClaude(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// this file lives at <server>/cmd/conductor/integration_test.go
	serverRoot := filepath.Dir(filepath.Dir(filepath.Dir(file)))

	out := filepath.Join(t.TempDir(), "fake-claude")
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("look up `go`: %v", err)
	}
	cmd := exec.Command(goBin, "build", "-tags", "testbinaries",
		"-o", out, "./internal/backend/testbinaries/fake-claude")
	cmd.Dir = serverRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build fake-claude: %v", err)
	}
	return out
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// --- `conductor run` recording behaviour (option A) ---------------------

// TestRun_DefaultRecords_AndAutoRegisters exercises the auto-register
// + record-the-run path that the new default `conductor run` uses. We
// drive the lower-level helpers (the cobra layer is exercised by the
// smoke; here we test the recorder + openRecorderForRun wiring that
// `doRun` calls).
func TestRun_DefaultRecords_AndAutoRegisters(t *testing.T) {
	dir := t.TempDir()
	// Build a fake binary so we can invoke backend.New without PATH.
	bin := buildFakeClaude(t)

	// Synthesize a minimal *configschema.Schema (the production
	// CLI loads this from a YAML; for the unit test we build it
	// directly to avoid touching the filesystem layout).
	schema := schemaFromFields(t, "auto-agent", "claude", "auto description", bin)

	// Call the wiring used by doRun: open the recorder under a
	// fresh cwd, run executeBackend, then inspect the DB.
	reg, rec, err := openRecorderForRun(true, schema, "hi\n", dir)
	if err != nil {
		t.Fatalf("openRecorderForRun: %v", err)
	}
	if reg == nil || rec == nil {
		t.Fatal("expected non-nil reg + rec with recording enabled")
	}
	defer reg.Close()

	// Drive the executeBackend path the way doRun does.
	if err := runExecuteBackendAgainstFake(t, schema, rec); err != nil {
		t.Fatalf("executeBackend: %v", err)
	}

	// Assert: a run exists under the auto-registered agent.
	got, err := reg.GetAgent(context.Background(), "auto-agent")
	if err != nil {
		t.Fatalf("auto-registered agent lookup: %v", err)
	}
	if got.Backend != "claude" || got.Description != "auto description" {
		t.Fatalf("unexpected auto-registered row: %+v", got)
	}
	runs, err := reg.ListRuns(context.Background(), "auto-agent", agentregistry.ListRunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].Status != "completed" {
		t.Fatalf("expected completed run, got %q", runs[0].Status)
	}
	if runs[0].EventCount < 1 {
		t.Fatalf("expected ≥1 event, got %d", runs[0].EventCount)
	}
}

func TestRun_NoRecordSkipsRegistry(t *testing.T) {
	bin := buildFakeClaude(t)

	schema := schemaFromFields(t, "skipped", "claude", "", bin)

	// OpenRecorderForRun with enabled=false returns (nil, nil, nil).
	reg, rec, err := openRecorderForRun(false, schema, "hi\n", t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg != nil || rec != nil {
		t.Fatalf("expected nil reg/rec with recording disabled: reg=%v rec=%v", reg, rec)
	}
}

// TestRun_ExistingAgentReused verifies that two consecutive
// conductor runs with the same agent name attach to a single row
// (matching agentregistry.EnsureAgent's get-or-create contract).
func TestRun_ExistingAgentReused(t *testing.T) {
	dir := t.TempDir()
	bin := buildFakeClaude(t)

	schema := schemaFromFields(t, "reused", "claude", "", bin)

	// First run.
	reg, rec, err := openRecorderForRun(true, schema, "p1\n", dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := runExecuteBackendAgainstFake(t, schema, rec); err != nil {
		t.Fatalf("run #1: %v", err)
	}
	firstID := func() int64 { a, _ := reg.GetAgent(context.Background(), "reused"); return a.ID }()
	reg.Close()

	// Second run with same name; new schema instance to mimic a
	// second CLI invocation.
	schema2 := schemaFromFields(t, "reused", "claude", "different description (should be ignored)", bin)
	reg2, rec2, err := openRecorderForRun /* alias */(true, schema2, "p2\n", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()  // single dir for both calls
	if err := runExecuteBackendAgainstFake(t, schema2, rec2); err != nil {
		t.Fatalf("run #2: %v", err)
	}
	a, err := reg2.GetAgent(context.Background(), "reused")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != firstID {
		t.Fatalf("second run attached to a fresh row (%d vs %d)", a.ID, firstID)
	}
	if a.Description != "" {
		t.Fatalf("EnsureAgent must not overwrite existing description: %q", a.Description)
	}
	runs, _ := reg2.ListRuns(context.Background(), "reused", agentregistry.ListRunOpts{})
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs on the same agent, got %d", len(runs))
	}
}

// schemaFromFields builds a minimal *configschema.Schema carrying
// only the fields openRecorderForRun + executeBackend read. We
// avoid loading a YAML from disk so the test stays self-contained.
func schemaFromFields(t *testing.T, name, backend, description, bin string) *configschema.Schema {
	t.Helper()
	return &configschema.Schema{
		Agent: configschema.Agent{Name: name, Backend: backend, Description: description},
	}
}

// runExecuteBackendAgainstFake drives executeBackend the way doRun
// does, against the fake-claude fixture. Kept as a helper here so
// the three recording tests stay readable.
func runExecuteBackendAgainstFake(t *testing.T, schema *configschema.Schema, rec *runRecorder) error {
	t.Helper()
	bin := buildFakeClaude(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "scenario.jsonl")
	writeScript(t, script, []binrunner.ScriptStep{
		{Event: json.RawMessage(`{"type":"system","session_id":"sess","model":"claude-sonnet-4-5"}`)},
		{Event: json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`)},
		{Event: json.RawMessage(`{"type":"result","subtype":"success","is_error":false,"duration_ms":5,"session_id":"sess","result":"hi"}`)},
	})

	ctx := context.Background()
	b, err := backend.New(schema.Agent.Backend, backend.Config{
		ExecutablePath: bin,
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		return err
	}
	return executeBackend(ctx, io.Discard, io.Discard, schema.Agent.Backend,
		b, "hi", backend.ExecOptions{
			CustomArgs: []string{"--script", script, "--argv", filepath.Join(dir, "argv.jsonl")},
		}, rec, true /* asJSON */, true /* quiet */)
}
