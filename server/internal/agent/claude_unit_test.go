package agent

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
