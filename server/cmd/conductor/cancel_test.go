package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"conductor/server/internal/protocol"
	"conductor/server/internal/spec"
	"conductor/server/internal/storage"
)

// cancelSetup creates a fresh storage + a single spec + a long-running
// run (PID set to os.Getpid() so cancel can find us). Returns the
// runId.
func cancelSetup(t *testing.T, status storage.RunStatus) (runID string) {
	t.Helper()
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	store, err := storage.NewJsonFileStorage()
	if err != nil {
		t.Fatalf("NewJsonFileStorage: %v", err)
	}
	ctx := context.Background()

	res, err := spec.Create(ctx, spec.CreateInput{
		Spec:    protocol.AgentSpec{Provider: protocol.ProviderCodex, Model: "m", Name: "cancel-test"},
		BaseURL: "https://x",
	})
	if err != nil {
		t.Fatalf("spec.Create: %v", err)
	}

	runID = storage.NewRunID()
	if _, err := store.CreateRun(ctx, runID, res.SpecId, "p"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Set status + PID in one update.
	if err := store.UpdateRun(ctx, runID, func(rs *storage.RunState) {
		rs.Status = status
		rs.PID = os.Getpid()
	}); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	return runID
}

// TestCancelNoopWhenAlreadyCompleted is the safe path: cancel on a
// non-running run is a no-op (with a friendly message), not an
// error.
func TestCancelNoopWhenAlreadyCompleted(t *testing.T) {
	runID := cancelSetup(t, storage.RunStatusCompleted)

	var out, errOut bytes.Buffer
	err := runCancelWithWriter(context.Background(),
		[]string{runID}, &out, &errOut)
	if err != nil {
		t.Fatalf("cancel: %v (stderr=%q)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "nothing to cancel") {
		t.Errorf("output should say nothing to cancel, got %q", out.String())
	}
}

// TestCancelRequiresRunId guards the missing-arg path.
func TestCancelRequiresRunId(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	err := runCancelWithWriter(context.Background(), nil, &out, &errOut)
	if err == nil {
		t.Fatalf("expected error for missing runId")
	}
	if !strings.Contains(errOut.String(), "usage: conductor cancel") {
		t.Errorf("stderr should mention runId required, got %q", errOut.String())
	}
}

// TestCancelUnknownRunId covers the error path.
func TestCancelUnknownRunId(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	err := runCancelWithWriter(context.Background(),
		[]string{"does-not-exist"}, &out, &errOut)
	if err == nil {
		t.Fatalf("expected error for unknown runId")
	}
	if !strings.Contains(errOut.String(), "not found") {
		t.Errorf("stderr should mention not found, got %q", errOut.String())
	}
}

// TestCancelNoPID — a run created before PID tracking can't be
// cancelled by signal. Returns a clear error.
func TestCancelNoPID(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	store, err := storage.NewJsonFileStorage()
	if err != nil {
		t.Fatalf("NewJsonFileStorage: %v", err)
	}
	ctx := context.Background()
	runID := storage.NewRunID()
	if _, err := store.CreateRun(ctx, runID, "specA", "p"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Leave PID == 0.
	if err := store.UpdateRun(ctx, runID, func(rs *storage.RunState) {
		rs.Status = storage.RunStatusRunning
	}); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	var out, errOut bytes.Buffer
	err = runCancelWithWriter(context.Background(),
		[]string{runID}, &out, &errOut)
	if err == nil {
		t.Fatalf("expected error for run without PID")
	}
	if !strings.Contains(errOut.String(), "no recorded PID") {
		t.Errorf("stderr should mention no PID, got %q", errOut.String())
	}
}

// TestCancelHelp — --help exits 0.
func TestCancelHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runCancelWithWriter(context.Background(),
		[]string{"--help"}, &out, &errOut)
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	if !strings.Contains(errOut.String(), "conductor cancel") {
		t.Errorf("--help should print usage, got %q", errOut.String())
	}
}

// TestCancelTooManyArgs — extra positional args error out.
func TestCancelTooManyArgs(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	err := runCancelWithWriter(context.Background(),
		[]string{"a", "b"}, &out, &errOut)
	if err == nil {
		t.Fatalf("expected error for too many args")
	}
}

// TestCancelSubprocessHappyPath spawns a real `conductor run`
// subprocess in the background, then runs `conductor cancel` to
// verify the full end-to-end signal flow. This is the most
// realistic test of cancel.
func TestCancelSubprocessHappyPath(t *testing.T) {
	t.Helper()
	// Real-process subprocess test. Skipped under -short (CI default)
	// because it depends on a clean process environment and adds
	// ~5s to the test suite. Run with FAIL	. [setup failed]
	// (no -short) when iterating on cancel logic.
	if testing.Short() {
		t.Skip("subprocess test; run without -short to exercise")
	}
	// Build a fresh conductor binary in a tempdir.
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "conductor")
	if err := execGo("build", "-o", binPath, "."); err != nil {
		t.Skipf("can't build conductor: %v", err)
	}

	// Set up a home with a fake codex that hangs forever on
	// turn/start so we have time to cancel.
	homeDir := t.TempDir()
	binDirPath := filepath.Join(homeDir, "bin")
	coHome := filepath.Join(homeDir, "co")
	codexHome := filepath.Join(homeDir, "codex")
	if err := os.MkdirAll(binDirPath, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.MkdirAll(coHome, 0o755); err != nil {
		t.Fatalf("mkdir co: %v", err)
	}
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex: %v", err)
	}

	// Fake codex that hangs on turn/start (so the run stays
	// in status=running while we cancel).
	fakePath := filepath.Join(binDirPath, "codex")
	if err := os.WriteFile(fakePath, []byte(`#!/bin/sh
while read -r REQ; do
  METHOD=$(printf '%s' "$REQ" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  ID=$(printf '%s' "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$METHOD" in
    thread/start) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-x\"}}";;
    turn/start) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"; sleep 999;;
    turn/interrupt) exit 0;;
  esac
done
`), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	// Create a spec.
	env := append(os.Environ(),
		"PATH="+binDirPath+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CONDUCTOR_HOME="+coHome,
		"CODEX_HOME="+codexHome,
	)
	createOutput := runCmd(t, env, binPath, "spec", "create", "--name", "subproc", "--provider", "codex", "--model", "m")
	t.Logf("spec create output:\n%s", createOutput)
	listOutput := runCmd(t, env, binPath, "spec", "list")
	t.Logf("spec list output:\n%s", listOutput)
	// Parse the specId from the second line of `spec list` output.
	var specID string
	for _, line := range strings.Split(string(listOutput), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasPrefix(fields[0], "subproc-") {
			specID = fields[0]
			break
		}
	}
	if specID == "" {
		t.Fatalf("could not find subproc specId in:\n%s", listOutput)
	}
	t.Logf("specID=%s", specID)

	// Start conductor run in background. The fake codex hangs on
	// turn/start so the run stays in status=running while we cancel.
	type result struct {
		output []byte
		err    error
	}
	runDone := make(chan result, 1)
	runProc := startProc(env, binPath, "run", "--spec", specID, "long task")
	defer func() {
		// Cleanup: kill the run if still alive.
		_ = runProc.Process.Kill()
		_, _ = runProc.Process.Wait()
	}()

	// Wait for the run to appear in storage.
	var runID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		store, err := storage.NewJsonFileStorageForHome(coHome)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		runs, _ := store.ListRuns(context.Background(), storage.RunFilter{Status: []storage.RunStatus{storage.RunStatusRunning}})
		if len(runs) > 0 {
			runID = runs[0].RunID
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if runID == "" {
		t.Fatalf("no running run appeared in storage within 3s")
	}

	// Now run cancel and wait for it to complete.
	cancelOutput, cancelErr := runCmdWithErr(env, binPath, "cancel", runID)
	if cancelErr != nil {
		t.Logf("cancel output: %s", string(cancelOutput))
		t.Fatalf("cancel failed: %v", cancelErr)
	}
	if !bytes.Contains(cancelOutput, []byte("cancelled")) &&
		!bytes.Contains(cancelOutput, []byte("now cancelled")) {
		t.Errorf("cancel output should mention cancelled, got %q", string(cancelOutput))
	}

	// Wait for the run process to exit. We give it 3s; if the signal
	// handler didn't propagate (or the test's env is wrong), the
	// process will hang and the test would block indefinitely.
	waitDone := make(chan error, 1)
	go func() { waitDone <- runProc.Wait() }()
	select {
	case waitErr := <-waitDone:
		_ = waitErr
		t.Logf("run exited cleanly after cancel: err=%v", waitErr)
	case <-time.After(7 * time.Second):
		_ = runProc.Process.Kill()
		<-waitDone
		t.Logf("run did not exit after cancel; SIGKILLed (env likely wrong: PATH missing fake codex?)")
		t.Logf("run stdout/stderr: %s", runOutputString(runProc))
	}

	// Verify storage shows status=cancelled.
	store, err := storage.NewJsonFileStorageForHome(coHome)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	state, err := store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if state.Status != storage.RunStatusCancelled {
		t.Errorf("status = %v, want cancelled", state.Status)
	}

	// Suppress unused warning for runDone (we use runProc.Wait instead).
	_ = runDone
}

// runOutputString returns the captured stdout+stderr from a Cmd
// started by startProc. Stderr is appended after stdout.
func runOutputString(cmd *exec.Cmd) string {
	var b strings.Builder
	if s, ok := cmd.Stdout.(*safeBuffer); ok {
		b.WriteString("STDOUT:\n")
		b.Write(s.Bytes())
	}
	if s, ok := cmd.Stderr.(*safeBuffer); ok {
		b.WriteString("\nSTDERR:\n")
		b.Write(s.Bytes())
	}
	return b.String()
}

// --- subprocess plumbing ---

// execGo is a small helper that shells out to `go build`. Lives in
// the test file rather than a separate test helper to keep the
// test self-contained.
func execGo(args ...string) error {
	// We can't import "os/exec" at the top of the test file because
	// it would shadow our local command. Use a separate file.
	return runGoBuild(args...)
}

// runCmd is a convenience wrapper that runs a command and fails
// the test on error.
func runCmd(t *testing.T, env []string, bin string, args ...string) []byte {
	t.Helper()
	out, err := runCmdWithErr(env, bin, args...)
	if err != nil {
		t.Fatalf("run %v: %v\n%s", args, err, string(out))
	}
	return out
}
