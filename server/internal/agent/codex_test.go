package agent

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
// Tests that drive the real `codex` CLI in app-server mode. Skipped when
// the binary is not on $PATH. See claude_test.go for the live-test
// rationale; the same token-cost discipline applies (small prompts,
// tight timeouts).

func requireCodex(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI not installed; skipping live test")
	}
	return path
}

func runLiveCodex(t *testing.T, execPath, prompt string, opts ExecOptions, ctx context.Context) ([]Message, Result) {
	t.Helper()
	backend, err := New("codex", Config{
		ExecutablePath: execPath,
		Logger:        testLogger(t),
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
	case <-time.After(120 * time.Second):
		t.Fatal("Execute did not produce Result within 120s")
		return nil, Result{}
	}
}

// TestCodex_Live_HappyPath drives the real codex CLI end-to-end through
// the JSON-RPC app-server protocol. Validates the full handshake
// (initialize → thread/start → turn/start), notification parsing, and
// the final-result extraction.
func TestCodex_Live_HappyPath(t *testing.T) {
	path := requireCodex(t)
	msgs, res := runLiveCodex(t, path,
		"Respond with the single word: ok. Do not call any tools or run any commands.",
		ExecOptions{Timeout: 60 * time.Second},
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
		t.Errorf("SessionID is empty (thread id never captured)")
	}
	if res.DurationMs <= 0 {
		t.Errorf("DurationMs = %d, want > 0", res.DurationMs)
	}

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
}

// TestCodex_Live_Cancel exercises graceful cancellation against the
// real codex CLI in exec --json mode. Unlike app-server mode, exec --json
// does not open a remote-control websocket to chatgpt.com, so it works
// in environments where only an API key is available.
func TestCodex_Live_Cancel(t *testing.T) {
	path := requireCodex(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, res := runLiveCodex(t, path,
		"Count slowly from 1 to 100, one number per turn. Do not stop early.",
		ExecOptions{Timeout: 120 * time.Second},
		ctx)
	elapsed := time.Since(start)

	if res.Status != "aborted" {
		t.Errorf("Status = %q, want aborted (err: %s)", res.Status, res.Error)
	}
	if elapsed > 15*time.Second {
		t.Errorf("cancel took %s, expected < 15s", elapsed)
	}
}

// TestCodex_Live_Timeout exercises the wall-clock bound against the
// real codex CLI. Same machine-readable rationale as TestClaude_Live_Timeout.
func TestCodex_Live_Timeout(t *testing.T) {
	path := requireCodex(t)
	start := time.Now()
	_, res := runLiveCodex(t, path,
		"Count slowly from 1 to 100, one number per turn. Do not stop early.",
		ExecOptions{Timeout: 2 * time.Second},
		context.Background())
	elapsed := time.Since(start)

	if res.Status != "timeout" {
		t.Errorf("Status = %q, want timeout (err: %s)", res.Status, res.Error)
	}
	if elapsed > 15*time.Second {
		t.Errorf("timeout took %s, expected < 15s", elapsed)
	}
	if !strings.Contains(res.Error, "timed out") {
		t.Errorf("Error = %q, want contains 'timed out'", res.Error)
	}
}

// ── Pure-Go test (no subprocess) ─────────────────────────────────────────

// TestCodex_BlocklistFiltersCustomArgs verifies the user-vs-conductor
// argv filter without spawning the real CLI. The conductor-owned flags
// for codex are app-server / --listen / -c / --model — different from
// claude's set, but the invariant is the same: user CustomArgs cannot
// override them.
func TestCodex_BlocklistFiltersCustomArgs(t *testing.T) {
	dir := t.TempDir()
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
	backend, err := New("codex", Config{ExecutablePath: wrapper, Logger: testLogger(t)})
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

	if strings.Contains(argv, "--model") {
		t.Errorf("argv should not contain --model (codex takes model via JSON-RPC), got: %s", argv)
	}
	if strings.Contains(argv, "user-attempt") {
		t.Errorf("argv should not contain user-attempt, got: %s", argv)
	}
	if !strings.Contains(argv, "--user-flag") || !strings.Contains(argv, "user-value") {
		t.Errorf("argv should contain --user-flag user-value, got: %s", argv)
	}
	if !strings.Contains(argv, "exec") {
		t.Errorf("argv should contain exec subcommand, got: %s", argv)
	}
	if !strings.Contains(argv, "--json") {
		t.Errorf("argv should contain --json, got: %s", argv)
	}
	if !strings.Contains(argv, "--approve-for-me") {
		t.Errorf("argv should contain --approve-for-me, got: %s", argv)
	}
	if !strings.Contains(argv, "config-model") {
		t.Errorf("argv should contain -m config-model, got: %s", argv)
	}
}

// TestCodex_Live_Resume verifies that `codex exec resume <id>` continues
// a prior conversation. The first run asks codex to remember a token;
// the second run resumes the same thread and asks codex to recall it.
// If resume works the answer must contain the token; if it doesn't,
// the second run is a fresh thread and the answer won't know it.
func TestCodex_Live_Resume(t *testing.T) {
	path := requireCodex(t)
	token := fmt.Sprintf("RESUMETEST-%d", time.Now().UnixNano())

	_, first := runLiveCodex(t, path,
		fmt.Sprintf("Remember this exact token: %s. Do not call any tools or run any commands.", token),
		ExecOptions{Timeout: 60 * time.Second},
		context.Background())
	if first.Status != "completed" {
		t.Fatalf("first run Status = %q (err: %s)", first.Status, first.Error)
	}
	if first.SessionID == "" {
		t.Fatal("first run produced no thread id")
	}
	t.Logf("first run thread_id = %s", first.SessionID)

	msgs, second := runLiveCodex(t, path,
		fmt.Sprintf("What is the exact token I asked you to remember earlier? Respond with just the token."),
		ExecOptions{
			Timeout:         60 * time.Second,
			ResumeSessionID: first.SessionID,
		},
		context.Background())
	if second.Status != "completed" {
		t.Fatalf("second run Status = %q (err: %s); resume failed", second.Status, second.Error)
	}
	if !strings.Contains(second.Output, token) {
		t.Errorf("second run output = %q, expected to contain remembered token %q (resume did not carry conversation context)", second.Output, token)
	}
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
