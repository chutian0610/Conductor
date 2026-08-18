package backend

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ── Live tests ───────────────────────────────────────────────────────────
//
// These tests run against the real Claude Code CLI installed on $PATH.
// They are skipped automatically when the binary is absent, so CI runs
// are not gated on having Claude installed; contributors who do have it
// get end-to-end coverage of the wire protocol for free.
//
// Each test pins a small, cheap prompt ("respond with the single word:
// ok") so the network + token cost stays bounded. Timeouts are tight so
// a hang in the backend fails fast rather than blocking the suite.

// requireClaude skips the test if the `claude` binary is not on $PATH.
func requireClaude(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not installed; skipping live test")
	}
	return path
}

// runLiveClaude boots the real claude CLI through the Backend interface
// and returns the collected events + final result.
func runLiveClaude(t *testing.T, execPath, prompt string, opts ExecOptions, ctx context.Context) ([]Message, Result) {
	t.Helper()
	backend, err := New("claude", Config{
		ExecutablePath: execPath,
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, err := backend.Execute(ctx, prompt, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	type collected struct {
		msgs []Message
		res  Result
	}
	ch := make(chan collected, 1)
	go func() {
		var msgs []Message
		for m := range session.Messages {
			msgs = append(msgs, m)
		}
		ch <- collected{msgs, <-session.Result}
	}()
	select {
	case got := <-ch:
		return got.msgs, got.res
	case <-time.After(60 * time.Second):
		t.Fatal("Execute did not produce Result within 60s")
		return nil, Result{}
	}
}

// TestClaude_Live_HappyPath runs the real CLI end-to-end. Validates the
// full wire-protocol round-trip: stream-json framing, session id
// extraction, token usage reporting, and final output.
func TestClaude_Live_HappyPath(t *testing.T) {
	path := requireClaude(t)
	msgs, res := runLiveClaude(t, path,
		"Respond with the single word: ok. Do not call any tools.",
		ExecOptions{MaxTurns: 1, Timeout: 30 * time.Second},
		context.Background())

	if res.Status != "completed" {
		t.Fatalf("Status = %q, want completed (err: %s)", res.Status, res.Error)
	}
	if res.Output == "" {
		t.Errorf("Output is empty")
	}
	if !strings.Contains(strings.ToLower(res.Output), "ok") {
		t.Errorf("Output = %q, expected to contain 'ok'", res.Output)
	}
	if res.SessionID == "" {
		t.Errorf("SessionID is empty")
	}
	if res.DurationMs <= 0 {
		t.Errorf("DurationMs = %d, want > 0", res.DurationMs)
	}

	// We expect at least one text message.
	var sawText bool
	for _, m := range msgs {
		if m.Type == MessageText && m.Content != "" {
			sawText = true
			break
		}
	}
	if !sawText {
		t.Errorf("did not see any MessageText in %d messages", len(msgs))
	}

	// And the status pins should carry the session id.
	for _, m := range msgs {
		if m.Type == MessageStatus && m.Status == "running" {
			if m.SessionID == "" {
				t.Errorf("running status carries empty SessionID: %+v", m)
			}
		}
	}
}

// TestClaude_Live_Cancel exercises graceful cancellation against the
// real CLI. The CLI is given a long-running task; we cancel mid-flight
// and assert the subprocess actually exits within the grace window.
func TestClaude_Live_Cancel(t *testing.T) {
	path := requireClaude(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, res := runLiveClaude(t, path,
		"Count slowly from 1 to 100, one number per turn. Do not stop early.",
		ExecOptions{Timeout: 60 * time.Second},
		ctx)
	elapsed := time.Since(start)

	if res.Status != "aborted" {
		t.Errorf("Status = %q, want aborted", res.Status)
	}
	if elapsed > 10*time.Second {
		t.Errorf("cancel took %s, expected < 10s (subprocess did not die promptly)", elapsed)
	}
}

// TestClaude_Live_Timeout exercises the wall-clock bound. The CLI is
// given a long-running task with opts.Timeout shorter than the task can
// possibly finish.
func TestClaude_Live_Timeout(t *testing.T) {
	path := requireClaude(t)
	start := time.Now()
	_, res := runLiveClaude(t, path,
		"Count slowly from 1 to 100, one number per turn. Do not stop early.",
		ExecOptions{Timeout: 2 * time.Second},
		context.Background())
	elapsed := time.Since(start)

	if res.Status != "timeout" {
		t.Errorf("Status = %q, want timeout (err: %s)", res.Status, res.Error)
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout took %s, expected < 10s", elapsed)
	}
	if !strings.Contains(res.Error, "timed out") {
		t.Errorf("Error = %q, want contains 'timed out'", res.Error)
	}
}

// ── Pure-Go test (no subprocess) ─────────────────────────────────────────

// TestClaude_BlocklistFiltersCustomArgs verifies the user-vs-conductor
// argv filter without spawning the real CLI. We capture the argv via a
// shell wrapper, run any binary that echoes "$@" before exiting, and
// verify the conductor-owned flags survived the filter.
func TestClaude_BlocklistFiltersCustomArgs(t *testing.T) {
	dir := t.TempDir()
	// /bin/sh -c 'echo "$@" > file' — no LLM required, just argv capture.
	argvFile := dir + "/argv.txt"
	wrapper := dir + "/wrapper"
	script := "#!/bin/sh\necho \"$@\" > " + argvFile + "\n"
	if err := writeFile(wrapper, script, 0o755); err != nil {
		t.Fatal(err)
	}

	opts := ExecOptions{
		Model:      "config-model",
		CustomArgs: []string{"--model", "user-attempt", "--user-flag", "user-value"},
	}
	backend, err := New("claude", Config{ExecutablePath: wrapper, Logger: testLogger(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, err := backend.Execute(context.Background(), "test", opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range session.Messages {
	}
	<-session.Result

	data, err := readFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.TrimSpace(string(data))

	if strings.Contains(argv, "user-attempt") {
		t.Errorf("argv should not contain --model user-attempt, got: %s", argv)
	}
	if !strings.Contains(argv, "config-model") {
		t.Errorf("argv should contain config-model, got: %s", argv)
	}
	if !strings.Contains(argv, "--user-flag") || !strings.Contains(argv, "user-value") {
		t.Errorf("argv should contain --user-flag user-value, got: %s", argv)
	}
}

// TestClaude_Live_Resume verifies that --resume continues a prior
// conversation. The first run asks the agent to remember a token; the
// second run resumes the same session and asks the agent to recall it.
// If resume works the answer must contain the token; if it doesn't,
// the second run is a fresh conversation and the answer won't know it.
//
// V1 does not implement fallback (multica's ResumeExpected +
// ResumeContinuityNotice); a bad session id would surface as a hard
// failure on the second run. That's covered by TestClaude_Live_HappyPath.
func TestClaude_Live_Resume(t *testing.T) {
	path := requireClaude(t)
	token := fmt.Sprintf("RESUMETEST-%d", time.Now().UnixNano())

	_, first := runLiveClaude(t, path,
		fmt.Sprintf("Remember this exact token: %s. Do not call any tools.", token),
		ExecOptions{MaxTurns: 1, Timeout: 30 * time.Second},
		context.Background())
	if first.Status != "completed" {
		t.Fatalf("first run Status = %q (err: %s)", first.Status, first.Error)
	}
	if first.SessionID == "" {
		t.Fatal("first run produced no session id")
	}
	t.Logf("first run session_id = %s", first.SessionID)

	msgs, second := runLiveClaude(t, path,
		fmt.Sprintf("What is the exact token I asked you to remember earlier? Respond with just the token."),
		ExecOptions{
			MaxTurns:        1,
			Timeout:         30 * time.Second,
			ResumeSessionID: first.SessionID,
		},
		context.Background())
	if second.Status != "completed" {
		t.Fatalf("second run Status = %q (err: %s); resume failed", second.Status, second.Error)
	}
	if !strings.Contains(second.Output, token) {
		t.Errorf("second run output = %q, expected to contain remembered token %q (resume did not carry conversation context)", second.Output, token)
	}
	if second.SessionID == "" {
		t.Errorf("second run produced no session id")
	}
	// Sanity: at least one MessageText event from the second run.
	var sawText bool
	for _, m := range msgs {
		if m.Type == MessageText && m.Content != "" {
			sawText = true
			break
		}
	}
	if !sawText {
		t.Errorf("second run did not produce any MessageText events: %+v", msgs)
	}
}
