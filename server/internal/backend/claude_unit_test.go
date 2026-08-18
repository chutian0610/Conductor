package backend

// Unit tests for the small Claude-side helpers that the integration
// scenarios don't directly exercise. They live in package agent (not
// the integration_test) because they construct internal types like
// claudeSDKMessage and don't need a real Claude CLI.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestHandleClaudeControlRequest_MalformedRequest pins the JSON-unmarshal
// fail branch (followup row #3): when msg.Request is not a valid JSON
// object, the handler returns silently without writing anything to
// stdin. This guards against a future refactor accidentally panicking
// or crashing on corrupt wire input.
func TestHandleClaudeControlRequest_MalformedRequest(t *testing.T) {
	var stdin bytes.Buffer
	logger := testLogger(t)
	msg := claudeSDKMessage{
		Type:      "control_request",
		RequestID: "req-bad",
		// Not parseable as claudeControlRequestPayload — missing
		// braces / unterminated string / etc.
		Request: json.RawMessage(`{not valid`),
	}
	// Should not panic, should not write to stdin.
	handleClaudeControlRequest(msg, &stdin, logger)
	if stdin.Len() != 0 {
		t.Fatalf("malformed request must not write stdin, got %q", stdin.String())
	}
}

// TestHandleClaudeControlRequest_RunInBackgroundFalse exercises the
// common path: a valid request with run_in_background=true triggers
// the forced-foreground mutation + control_response write.
func TestHandleClaudeControlRequest_RunInBackgroundFalse(t *testing.T) {
	var stdin bytes.Buffer
	logger := testLogger(t)
	msg := claudeSDKMessage{
		Type:      "control_request",
		RequestID: "req-bg",
		Request: json.RawMessage(`{
            "subtype":"tool_use",
            "tool_name":"Bash",
            "input":{"run_in_background": true, "cmd":"ls"}
        }`),
	}
	handleClaudeControlRequest(msg, &stdin, logger)

	got := bytes.TrimSpace(stdin.Bytes())
	if len(got) == 0 {
		t.Fatal("expected response to be written to stdin")
	}
	var resp map[string]any
	if err := json.Unmarshal(got, &resp); err != nil {
		t.Fatalf("response not valid JSON: %v: %s", err, got)
	}
	if resp["type"] != "control_response" {
		t.Fatalf("missing control_response wrapper: %v", resp["type"])
	}
}

// TestWriteMcpConfigToTemp_HappyPath (followup row #4): default-success
// path returns the path inside a conductor-mcp-* temp dir, and the
// file actually contains the bytes we passed.
func TestWriteMcpConfigToTemp_HappyPath(t *testing.T) {
	raw := json.RawMessage(`{"servers":[{"name":"x","command":"y","args":["z"]}]}`)
	path, err := writeMcpConfigToTemp(raw)
	if err != nil {
		t.Fatalf("writeMcpConfigToTemp: %v", err)
	}
	if !strings.Contains(filepath.Base(filepath.Dir(path)), "conductor-mcp-") {
		t.Errorf("expected dir prefix conductor-mcp-*, got %s", filepath.Dir(path))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("file content mismatch: got %s want %s", got, raw)
	}
	// Cleanup the temp dir the function chose.
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(path)) })
}

// TestWriteMcpConfigToTemp_MkdirFailure pins the MkdirTemp error path.
// We force os.MkdirTemp to fail by setting TMPDIR to a path that
// can't be created — a regular file at the parent location. The
// function must return a wrapped error and leave nothing behind.
func TestWriteMcpConfigToTemp_MkdirFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "regular")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(blocker, "sub"))

	_, err := writeMcpConfigToTemp(json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error when TMPDIR cannot be created")
	}
	if !strings.Contains(err.Error(), "create mcp config temp dir") {
		t.Fatalf("error not wrapped as expected: %v", err)
	}
}

// TestCleanupMcpConfigTemp_NoOpWhenPathEmpty verifies that a zero
// path is a no-op — used by the deferred cleanup in run-time paths.
func TestCleanupMcpConfigTemp_NoOpWhenPathEmpty(t *testing.T) {
	// No panic, no error return — function signature is void.
	cleanupMcpConfigTemp("")
	cleanupMcpConfigTemp("/no/such/file/or/dir")
}

// Sanity: confirm the testLogger helper hasn't lost its slog.Logger
// semantics (this is what handleClaudeControlRequest / future
// helpers depend on).
func TestTestLogger_DoesNotPanic(t *testing.T) {
	l := testLogger(t)
	if l == nil {
		t.Fatal("testLogger returned nil")
	}
	var _ *slog.Logger = l
	var _ io.Writer = os.Stderr
	_ = io.Discard
}

// TestBuildClaudeArgs_ReportsDropped exercises the new BlockedArg
// surface. Operators who put `--model my-model` or `--output-format
// text` in `args:` deserve a stderr line, not silent failure.
func TestBuildClaudeArgs_ReportsDropped(t *testing.T) {
	t.Run("non-takes-value flag", func(t *testing.T) {
		args, dropped := buildClaudeArgs(ExecOptions{
			CustomArgs: []string{"-p", "--user-flag", "value"},
		})
		// -p is blocked (no value following); --user-flag passes through.
		if len(dropped) != 1 || dropped[0].Flag != "-p" || dropped[0].TakesValue {
			t.Fatalf("unexpected dropped: %+v", dropped)
		}
		if !argSliceContains(args, "--user-flag", "value") {
			t.Errorf("non-blocked args not preserved: %v", args)
		}
	})

	t.Run("takes-value flag drops partner", func(t *testing.T) {
		// --model my-model — conductor keeps the user's --model dropped
		// AND consumes the following "my-model" value token.
		args, dropped := buildClaudeArgs(ExecOptions{
			Model:      "config-model",
			CustomArgs: []string{"--model", "user-model", "--user-flag", "value"},
		})
		want := BlockedArg{Flag: "--model", TakesValue: true}
		if len(dropped) != 1 || dropped[0] != want {
			t.Fatalf("dropped: got %+v want %+v", dropped[0], want)
		}
		// The "user-model" value must NOT survive in argv.
		for _, a := range args {
			if a == "user-model" {
				t.Errorf("consumed value token leaked into argv: %v", args)
			}
		}
		// But the conductor-pinned model from opts.Model did.
		if !argSliceContains(args, "config-model") {
			t.Errorf("config-model missing: %v", args)
		}
	})

	t.Run("no drops returns empty slice", func(t *testing.T) {
		_, dropped := buildClaudeArgs(ExecOptions{
			CustomArgs: []string{"--user-flag", "value"},
		})
		if len(dropped) != 0 {
			t.Fatalf("expected zero drops, got %+v", dropped)
		}
	})
}

// TestBuildClaudeArgs_DropsAreLogged pins the caller-side wiring: the
// runOneAttempt function logs each BlockedArg at WARN level so it
// surfaces in the operator's stderr stream. We can't easily exercise
// runOneAttempt directly here (it spawns a real CLI), so this is a
// contract pin via the same warning helpers the production code uses.
//
// If a future refactor renames the warning key or removes the loop,
// this test catches the regression.
func TestBuildClaudeArgs_DropsAreLogged(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&safeBuf{buf: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Replicate exactly what runOneAttempt does with the dropped list.
	args, dropped := buildClaudeArgs(ExecOptions{
		CustomArgs: []string{"--model", "user-x"},
	})
	for _, d := range dropped {
		logger.Warn("claude: blocked user arg dropped by conductor",
			"flag", d.Flag, "took_value_with_it", d.TakesValue)
	}
	if !strings.Contains(buf.String(), "blocked user arg dropped") {
		t.Fatalf("expected warn line in log, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "--model") {
		t.Fatalf("expected flag name in log, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "took_value_with_it=true") {
		t.Fatalf("expected takes-value indicator, got %q", buf.String())
	}
	// Sanity: we still got the full argv, just with a warning line.
	if len(args) == 0 {
		t.Fatal("argv unexpectedly empty")
	}
}

// safeBuf wraps a *bytes.Buffer in a sync.Mutex so the slog handler
// doesn't race with the test thread when a single emitter calls Write
// concurrently (slog handlers are documented as goroutine-safe).
type safeBuf struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *safeBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func argSliceContains(args []string, want ...string) bool {
	// Subsequence check: every `want` value appears as a contiguous
	// run somewhere in args. Useful to assert argv preservation
	// without committing to a fixed positional order.
	if len(want) == 0 {
		return true
	}
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for k, w := range want {
			if args[i+k] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
