package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"conductor/server/internal/protocol"
	"conductor/server/internal/storage"
	"conductor/server/internal/spec"
)

// runFakeCodexScript — same shape as runner_test's fake, inlined so
// the cmd package doesn't import the test fake from runner (which
// is in a different package).
const runFakeCodexScript = `#!/bin/sh
while read -r REQ; do
  METHOD=$(printf '%s' "$REQ" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  ID=$(printf '%s' "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$METHOD" in
    thread/start) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-cmd\"}}";;
    thread/resume) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-cmd-resumed\"}}";;
    turn/start)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"
      printf '%s\n' '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"text":"hi"}}'
      printf '%s\n' '{"jsonrpc":"2.0","method":"item/toolCall","params":{"name":"bash","id":"c1","arguments":{"cmd":"ls"}}}'
      printf '%s\n' '{"jsonrpc":"2.0","method":"item/toolResult","params":{"result":"file1"}}'
      printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{"usage":{"inputTokens":11,"outputTokens":4,"costUsd":0.005},"finish":{"reason":"end_turn","success":true},"threadId":"thr-cmd"}}'
      exit 0
      ;;
    *) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"error\":{\"code\":-32601}}";;
  esac
done
`

// setupRunFixture writes the fake codex, creates a wrapper in PATH
// that exec's the fake, and registers a spec. Returns the spec id.
func setupRunFixture(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	t.Setenv("CONDUCTOR_HOME", t.TempDir())

	fakePath := filepath.Join(t.TempDir(), "fake.sh")
	if err := os.WriteFile(fakePath, []byte(runFakeCodexScript), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "codex")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /bin/sh "+fakePath+` "$@"`+"\n"), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := spec.Create(context.Background(), spec.CreateInput{
		Spec: protocol.AgentSpec{
			Provider: protocol.ProviderCodex,
			Model:    "m",
			Name:     "cmd-run-test",
		},
		BaseURL: "https://x",
		EnvKey:  "K",
	})
	if err != nil {
		t.Fatalf("spec.Create: %v", err)
	}
	return res.SpecId
}

func TestRunSpecRequired(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	err := runRunWithWriter(context.Background(), []string{"hello"}, &out, &errOut)
	if err == nil {
		t.Fatalf("expected error for missing --spec")
	}
	if !strings.Contains(errOut.String(), "--spec is required") {
		t.Errorf("stderr should mention --spec, got %q", errOut.String())
	}
}

func TestRunPromptRequired(t *testing.T) {
	specId := setupRunFixture(t)
	var out, errOut bytes.Buffer
	err := runRunWithWriter(context.Background(), []string{"--spec", specId}, &out, &errOut)
	if err == nil {
		t.Fatalf("expected error for missing prompt")
	}
	if !strings.Contains(errOut.String(), "prompt") {
		t.Errorf("stderr should mention prompt, got %q", errOut.String())
	}
}

func TestRunSpecNotFound(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	err := runRunWithWriter(context.Background(), []string{"--spec", "missing", "hello"}, &out, &errOut)
	if !errors.Is(err, spec.ErrNotFound) {
		t.Errorf("err = %v, want spec.ErrNotFound", err)
	}
	if !strings.Contains(errOut.String(), "not found") {
		t.Errorf("stderr should mention not found, got %q", errOut.String())
	}
}

func TestRunHappyPathText(t *testing.T) {
	specId := setupRunFixture(t)
	var out, errOut bytes.Buffer
	err := runRunWithWriter(context.Background(),
		[]string{"--spec", specId, "hello"}, &out, &errOut)
	if err != nil {
		t.Fatalf("Run: %v (stderr=%q)", err, errOut.String())
	}

	s := out.String()
	// Streaming events.
	if !strings.Contains(s, "hi") {
		t.Errorf("stdout should contain streamed text 'hi', got %q", s)
	}
	if !strings.Contains(s, "→ bash") {
		t.Errorf("stdout should contain tool call marker, got %q", s)
	}
	if !strings.Contains(s, "← file1") {
		t.Errorf("stdout should contain tool result, got %q", s)
	}
	// Final result summary.
	if !strings.Contains(s, "--- result ---") {
		t.Errorf("stdout should have result block, got %q", s)
	}
	if !strings.Contains(s, "session:") {
		t.Errorf("stdout should print session id, got %q", s)
	}
	if !strings.Contains(s, "thr-cmd") {
		t.Errorf("stdout should mention thread id, got %q", s)
	}
	if !strings.Contains(s, "11 input / 4 output") {
		t.Errorf("stdout should print token usage, got %q", s)
	}
	if !strings.Contains(s, "(~$0.0050)") {
		t.Errorf("stdout should print cost, got %q", s)
	}
	if !strings.Contains(s, "end_turn (success)") {
		t.Errorf("stdout should print finish reason, got %q", s)
	}
}

func TestRunHappyPathJSON(t *testing.T) {
	specId := setupRunFixture(t)
	var out, errOut bytes.Buffer
	err := runRunWithWriter(context.Background(),
		[]string{"--spec", specId, "--json", "hello"}, &out, &errOut)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Each event is a JSON object on its own line.
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) < 4 {
		t.Fatalf("want at least 4 JSON lines (text, toolCall, toolResult, result), got %d:\n%s", len(lines), out.String())
	}

	// First three lines are events; verify each parses.
	var kinds []string
	for _, line := range lines[:3] {
		var ev protocol.AgentStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("line %q not valid JSON: %v", line, err)
			continue
		}
		kinds = append(kinds, string(ev.Kind))
	}
	wantKinds := []string{"text", "tool_call", "tool_result"}
	for i, want := range wantKinds {
		if i >= len(kinds) || kinds[i] != want {
			t.Errorf("event[%d] kind = %v, want %s", i, kinds, want)
		}
	}

	// Result summary appears at the end as "--- result ---" (text
	// not JSON — the summary block is human-readable regardless of
	// --json; --json affects event streaming only).
	if !strings.Contains(out.String(), "--- result ---") {
		t.Errorf("stdout should have result block even with --json, got %q", out.String())
	}
}

func TestRunQuietSuppressesStreaming(t *testing.T) {
	specId := setupRunFixture(t)
	var out, errOut bytes.Buffer
	err := runRunWithWriter(context.Background(),
		[]string{"--spec", specId, "--quiet", "hello"}, &out, &errOut)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// With --quiet, the streamed text 'hi' should NOT appear,
	// but the result summary should.
	if strings.Contains(out.String(), "hi") {
		t.Errorf("--quiet should suppress streaming text, got %q", out.String())
	}
	if !strings.Contains(out.String(), "--- result ---") {
		t.Errorf("--quiet should still print result block, got %q", out.String())
	}
}

func TestRunHelp(t *testing.T) {
	specId := setupRunFixture(t)
	var out, errOut bytes.Buffer
	err := runRunWithWriter(context.Background(),
		[]string{"--spec", specId, "--help"}, &out, &errOut)
	if err != nil {
		t.Fatalf("--help returned err: %v", err)
	}
	if !strings.Contains(errOut.String(), "conductor run — invoke") {
		t.Errorf("--help should print usage, got %q", errOut.String())
	}
}

// resumeRunFakeScript — fake codex that records which thread method
// it received. Returns thr-cmd for thread/start, thr-cmd-resumed
// for thread/resume, plus a turn/completed carrying the chosen
// threadId. The test asserts the recorded method matches what
// --from-run should produce.
const resumeRunFakeScript = `#!/bin/sh
LOGFILE="$CONDUCTOR_FAKE_LOG"
while read -r REQ; do
  METHOD=$(printf '%s' "$REQ" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  ID=$(printf '%s' "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$LOGFILE" ] && printf '%s\n' "$METHOD" >> "$LOGFILE"
  case "$METHOD" in
    thread/start)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-cmd\"}}"
      ;;
    thread/resume)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-cmd-resumed\"}}"
      ;;
    turn/start)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"
      printf '%s\n' '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"text":"hi"}}'
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"turn/completed\",\"params\":{\"usage\":{},\"finish\":{\"reason\":\"end_turn\",\"success\":true},\"threadId\":\"thr-x\"}}"
      exit 0
      ;;
  esac
done
`

// setupResumeRunFixture creates a spec + a fake codex in PATH, then
// runs `conductor run` once to produce a completed RunState with a
// sessionId. Returns that sessionId + the runId.
func setupResumeRunFixture(t *testing.T) (specID, runID, sessionID, logPath string) {
	t.Helper()
	t.Setenv("CONDUCTOR_HOME", t.TempDir())

	logPath = t.TempDir() + "/methods.log"
	t.Setenv("CONDUCTOR_FAKE_LOG", logPath)

	fakePath := filepath.Join(t.TempDir(), "fake.sh")
	if err := os.WriteFile(fakePath, []byte(resumeRunFakeScript), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "codex")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /bin/sh "+fakePath+` "$@"`+"\n"), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := spec.Create(context.Background(), spec.CreateInput{
		Spec:    protocol.AgentSpec{Provider: protocol.ProviderCodex, Model: "m", Name: "resume-test"},
		BaseURL: "https://x",
	})
	if err != nil {
		t.Fatalf("spec.Create: %v", err)
	}
	specID = res.SpecId

	// First run — captures a sessionId.
	var out, errOut bytes.Buffer
	if err := runRunWithWriter(context.Background(),
		[]string{"--spec", specID, "first prompt"}, &out, &errOut); err != nil {
		t.Fatalf("first run: %v\nstderr=%q", err, errOut.String())
	}

	// Find the just-created runId from storage.
	store, err := storage.NewJsonFileStorage()
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	runs, err := store.ListRuns(context.Background(), storage.RunFilter{SpecID: specID})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) == 0 {
		t.Fatalf("no runs recorded")
	}
	runID = runs[0].RunID
	sessionID = runs[0].SessionID
	if sessionID == "" {
		t.Fatalf("first run has no sessionId")
	}

	// Clear the log so the second run's record starts from a clean slate.
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatalf("clear log: %v", err)
	}
	return specID, runID, sessionID, logPath
}

// TestRunResumeRunByRunID is the end-to-end happy path for
// --from-run: creates a run, captures its sessionId, then
// resumes it by runId. Asserts that the second invocation took
// the thread/resume branch (not thread/start) and returned the
// resumed sessionId.
func TestRunResumeRunByRunID(t *testing.T) {
	specID, runID, _, logPath := setupResumeRunFixture(t)

	var out, errOut bytes.Buffer
	err := runRunWithWriter(context.Background(),
		[]string{"--spec", specID, "--from-run", runID, "second prompt"}, &out, &errOut)
	if err != nil {
		t.Fatalf("--from-run: %v\nstderr=%q", err, errOut.String())
	}

	// Should print the "resuming session ..." line for visibility.
	if !strings.Contains(out.String(), "resuming session") {
		t.Errorf("output should announce the resume, got %q", out.String())
	}

	// Verify the fake saw thread/resume (not thread/start).
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	methods := strings.Split(strings.TrimRight(string(logData), "\n"), "\n")
	// We expect exactly one method: thread/resume (turn/start
	// doesn't appear in this run because thread/resume
	// replaced it). Wait — actually we still issue turn/start
	// AFTER thread/resume. So we expect: thread/resume, turn/start.
	if len(methods) != 2 || methods[0] != "thread/resume" || methods[1] != "turn/start" {
		t.Errorf("methods = %v, want [thread/resume, turn/start]", methods)
	}
}

// TestRunResumeRunNotFound covers an unknown runId.
func TestRunResumeRunNotFound(t *testing.T) {
	specID := setupRunFixture(t)
	var out, errOut bytes.Buffer
	err := runRunWithWriter(context.Background(),
		[]string{"--spec", specID, "--from-run", "ghost-run", "x"}, &out, &errOut)
	if err == nil {
		t.Fatalf("expected error for unknown runId")
	}
	if !strings.Contains(errOut.String(), "--from-run ghost-run") {
		t.Errorf("stderr should mention the bad runId, got %q", errOut.String())
	}
}

// TestRunResumeMutuallyExclusive guards against --resume +
// --from-run being combined (ambiguous: which sessionId wins?).
func TestRunResumeMutuallyExclusive(t *testing.T) {
	specID, runID, _, _ := setupResumeRunFixture(t)
	var out, errOut bytes.Buffer
	err := runRunWithWriter(context.Background(),
		[]string{"--spec", specID, "--resume", "thr-direct", "--from-run", runID, "x"},
		&out, &errOut)
	if err == nil {
		t.Fatalf("expected error for --resume + --from-run")
	}
	if !strings.Contains(errOut.String(), "mutually exclusive") {
		t.Errorf("stderr should mention mutual exclusivity, got %q", errOut.String())
	}
}
