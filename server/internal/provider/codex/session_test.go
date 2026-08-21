package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"conductor/server/internal/protocol"
)

// fakeCodexScript returns a shell script that simulates a codex
// app-server. Recognised methods (case-by-case in shell):
//
//   thread/start    → {"result":{"threadId":"thr-fake"}}
//   thread/resume   → same (SessionId is logged to CONDUCTOR_FAKE_LOG
//                     so tests can verify resume was called)
//   turn/start      → {"result":{"ok":true}} then emits:
//                     - item/agentMessage/delta {"text":"hi"}
//                     - item/toolCall          {"name":"bash",...}
//                     - item/toolResult        {"result":"ok"}
//                     - turn/completed         (with usage/finish/threadId)
//   turn/interrupt  → {"result":{"ok":true}} then exits 0
//   anything else   → JSON-RPC error -32601
//
// Pass empty keepOpen to make the fake exit after the first turn
// (so Close() can drain); pass "keepOpen" to leave stdin open for
// more turns.
const fakeCodexScript = `#!/bin/sh
METHOD=""
KEEPOPEN="$1"
while read -r REQ; do
  METHOD=$(printf '%s' "$REQ" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  ID=$(printf '%s' "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$CONDUCTOR_FAKE_LOG" ] && printf '%s\n' "$METHOD" >> "$CONDUCTOR_FAKE_LOG"
  if [ "$METHOD" = "initialize" ]; then
    printf '%s
' '{"jsonrpc":"2.0","id":'"$ID"',"result":{"userAgent":"Conductor test"}}'
    printf '%s
' '{"jsonrpc":"2.0","method":"initialized","params":{}}'
    continue
  fi
  case "$METHOD" in
    thread/start)
      echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-fake\"}}"
      ;;
    thread/resume)
      echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-resumed\"}}"
      ;;
    turn/start)
      echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"
      echo '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"text":"hi"}}'
      echo '{"jsonrpc":"2.0","method":"item/toolCall","params":{"name":"bash","id":"c1","arguments":{"cmd":"ls"}}}'
      echo '{"jsonrpc":"2.0","method":"item/toolResult","params":{"result":"ok"}}'
      echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"usage":{"inputTokens":10,"outputTokens":5,"costUsd":0.0012},"finish":{"reason":"end_turn","success":true},"threadId":"thr-fake"}}'
      if [ "$KEEPOPEN" != "keepOpen" ]; then
        # Block on stdin so the client has time to consume the events
        # before we exit. Closing stdin would also work but reading
        # is more portable across shells.
        read -r _ || exit 0
      fi
      ;;
    turn/interrupt)
      echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"
      exit 0
      ;;
    *)
      echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"error\":{\"code\":-32601,\"message\":\"method not found: ${METHOD}\"}}"
      ;;
  esac
done
`

// writeFakeCodex writes the fakeCodexScript to a temp file and
// returns the path. Pass keepOpen="" to make the fake exit after
// one turn, or "keepOpen" to leave stdin open.
func writeFakeCodex(t *testing.T, keepOpen string) string {
	t.Helper()
	return writeFake(t, fakeCodexScript+"\n# arg: "+keepOpen+"\n")
}

// readMethodLog returns the methods seen by the fake (one per
// line, in arrival order).
func readMethodLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// TestSessionTurn exercises the full happy-path: NewSession
// performs thread/start; Send performs turn/start; intermediate
// notifications arrive on Events(); turn/completed populates the
// returned AgentTurnResult.
func TestSessionTurn(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	scriptPath := writeFakeCodex(t, "")
	sess, err := NewSession(ctx, SessionConfig{
		Bin:   "/bin/sh",
		Args:  []string{scriptPath, "keepOpen"},
		Home:  t.TempDir(),
		Model: "test-model",
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	if got := sess.ThreadID(); got != "thr-fake" {
		t.Errorf("ThreadID = %q, want thr-fake", got)
	}

	// Consume Events concurrently.
	var mu sync.Mutex
	var got []protocol.AgentStreamEvent
	consumerDone := make(chan struct{})
	go func() {
		for ev := range sess.Events() {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		}
		close(consumerDone)
	}()

	result, err := sess.Send(ctx, "hello world")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result == nil {
		t.Fatalf("Send returned nil result")
	}
	if result.SessionID != "thr-fake" {
		t.Errorf("SessionID = %q, want thr-fake", result.SessionID)
	}
	if result.Usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", result.Usage.OutputTokens)
	}
	if result.Usage.CostUSD != 0.0012 {
		t.Errorf("CostUSD = %v, want 0.0012", result.Usage.CostUSD)
	}
	if !result.Finish.Success {
		t.Errorf("Finish.Success = false, want true")
	}
	if result.Finish.Reason != "end_turn" {
		t.Errorf("Finish.Reason = %q, want end_turn", result.Finish.Reason)
	}

	sess.Close()
	select {
	case <-consumerDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("consumer did not finish after Close")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (text + toolCall + toolResult): %+v", len(got), got)
	}
	if got[0].Kind != protocol.EventText || got[0].Text != "hi" {
		t.Errorf("events[0] = %+v, want text 'hi'", got[0])
	}
	if got[1].Kind != protocol.EventToolCall || got[1].ToolName != "bash" ||
		got[1].ToolCallID != "c1" || got[1].ToolArgs["cmd"] != "ls" {
		t.Errorf("events[1] = %+v, want toolCall bash/c1/cmd=ls", got[1])
	}
	if got[2].Kind != protocol.EventToolResult || got[2].ToolResult != "ok" {
		t.Errorf("events[2] = %+v, want toolResult 'ok'", got[2])
	}
}

// TestSessionResume verifies that SessionId != "" routes to
// thread/resume (instead of thread/start). The fake writes the
// received method to CONDUCTOR_FAKE_LOG.
func TestSessionResume(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	logPath := t.TempDir() + "/methods.log"
	t.Setenv("CONDUCTOR_FAKE_LOG", logPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	scriptPath := writeFakeCodex(t, "")
	sess, err := NewSession(ctx, SessionConfig{
		Bin:       "/bin/sh",
		Args:      []string{scriptPath, "keepOpen"},
		Home:      t.TempDir(),
		Model:     "test-model",
		SessionId: "prev-session-id",
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	if got := sess.ThreadID(); got != "thr-resumed" {
		t.Errorf("ThreadID = %q, want thr-resumed", got)
	}

	methods := readMethodLog(t, logPath)
	// codex 0.147+ prepends an "initialize" + "initialized" handshake
	// to every session, so the first 2 entries are those, then the
	// resume starts. Find the index of the first "thread/resume".
	resumeIdx := -1
	for i, m := range methods {
		if m == "thread/resume" {
			resumeIdx = i
			break
		}
	}
	if resumeIdx == -1 {
		t.Fatalf("no thread/resume in log: %v", methods)
	}
	// And we must NOT see thread/start (would mean a fresh thread
	// instead of resuming).
	for _, m := range methods {
		if m == "thread/start" {
			t.Errorf("unexpected thread/start in log: %v", methods)
		}
	}
}

// TestSessionCancel verifies Cancel sends turn/interrupt and
// closes the session.
func TestSessionCancel(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	logPath := t.TempDir() + "/methods.log"
	t.Setenv("CONDUCTOR_FAKE_LOG", logPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	scriptPath := writeFakeCodex(t, "")
	sess, err := NewSession(ctx, SessionConfig{
		Bin:  "/bin/sh",
		Args: []string{scriptPath, "keepOpen"},
		Home: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if err := sess.Cancel(ctx); err != nil {
		t.Errorf("Cancel: %v", err)
	}

	select {
	case <-sess.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("Done did not close after Cancel")
	}
	if _, ok := <-sess.Events(); ok {
		t.Errorf("Events still open after Cancel")
	}

	methods := readMethodLog(t, logPath)
	wantInterrupt := false
	for _, m := range methods {
		if m == "turn/interrupt" {
			wantInterrupt = true
		}
	}
	if !wantInterrupt {
		t.Errorf("expected turn/interrupt in methods, got %v", methods)
	}

	// Cancel should be idempotent.
	if err := sess.Cancel(ctx); err != nil {
		t.Errorf("second Cancel: %v", err)
	}
}

// TestSessionSendAfterClose verifies Send returns an error if
// called after Close().
func TestSessionSendAfterClose(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	scriptPath := writeFakeCodex(t, "keepOpen")
	sess, err := NewSession(ctx, SessionConfig{
		Bin:  "/bin/sh",
		Args: []string{scriptPath},
		Home: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// SIGTERM on the blocking fake subprocess makes cmd.Wait() return
	// "signal: terminated" — that is expected, not a test failure.
	if err := sess.Close(); err != nil && !strings.Contains(err.Error(), "signal: terminated") {
		t.Fatalf("Close: %v", err)
	}

	if _, err := sess.Send(ctx, "after-close"); err == nil {
		t.Errorf("Send after Close should error, got nil")
	} else if !strings.Contains(err.Error(), "session closed") {
		t.Errorf("Send err = %v, want 'session closed'", err)
	}
}

// TestSessionConcurrentSendRejected verifies a second Send while
// one is in flight returns an error.
func TestSessionConcurrentSendRejected(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fake that responds to turn/start but never emits turn/completed,
	// so the first Send blocks. It still responds to turn/interrupt so
	// we can clean up via Cancel.
	fake := `#!/bin/sh
while read -r REQ; do
  METHOD=$(printf '%s' "$REQ" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  ID=$(printf '%s' "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  if [ "$METHOD" = "initialize" ]; then
    printf '%s
' '{"jsonrpc":"2.0","id":'"$ID"',"result":{"userAgent":"Conductor test"}}'
    printf '%s
' '{"jsonrpc":"2.0","method":"initialized","params":{}}'
    continue
  fi
  case "$METHOD" in
    thread/start) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-fake\"}}";;
    turn/start) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}";;
    turn/interrupt) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"; exit 0;;
  esac
done
`
	scriptPath := writeFake(t, fake)
	sess, err := NewSession(ctx, SessionConfig{
		Bin:  "/bin/sh",
		Args: []string{scriptPath},
		Home: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Cancel(ctx)

	firstDone := make(chan error, 1)
	go func() {
		_, err := sess.Send(ctx, "first prompt (will block)")
		firstDone <- err
	}()

	// Give the first Send time to register turnWaiter.
	time.Sleep(100 * time.Millisecond)

	if _, err := sess.Send(ctx, "second prompt"); err == nil {
		t.Errorf("second Send should error, got nil")
	} else if !strings.Contains(err.Error(), "already in flight") {
		t.Errorf("second Send err = %v, want 'already in flight'", err)
	}

	// Cancel ctx to unblock the first Send.
	cancel()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("first Send err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("first Send did not return after ctx cancel")
	}
}

// TestSessionUnknownNotificationSkipped verifies unknown
// notifications don't break the pump and don't appear on Events().
func TestSessionUnknownNotificationSkipped(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fake := `#!/bin/sh
while read -r REQ; do
  METHOD=$(printf '%s' "$REQ" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  ID=$(printf '%s' "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  if [ "$METHOD" = "initialize" ]; then
    printf '%s
' '{"jsonrpc":"2.0","id":'"$ID"',"result":{"userAgent":"Conductor test"}}'
    printf '%s
' '{"jsonrpc":"2.0","method":"initialized","params":{}}'
    continue
  fi
  case "$METHOD" in
    thread/start) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-fake\"}}";;
    turn/start)
      echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"
      echo '{"jsonrpc":"2.0","method":"item/someFutureKind","params":{"foo":"bar"}}'
      echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"usage":{},"finish":{"reason":"end_turn","success":true}}}'
      read -r _ || exit 0
      ;;
    turn/interrupt) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"; exit 0;;
  esac
done
`
	scriptPath := writeFake(t, fake)
	sess, err := NewSession(ctx, SessionConfig{
		Bin:  "/bin/sh",
		Args: []string{scriptPath},
		Home: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	// Collect events with timeout — should be empty (unknown skipped).
	done := make(chan struct{})
	go func() {
		for range sess.Events() {
		}
		close(done)
	}()

	result, err := sess.Send(ctx, "test")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !result.Finish.Success {
		t.Errorf("Finish.Success = false, want true (unknown notification should not block turn)")
	}

	sess.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Errorf("consumer did not finish")
	}
}

// TestMapNotification exercises the pure mapping function for
// every notification kind we care about. Parametric so a new
// shape is easy to add.
func TestMapNotification(t *testing.T) {
	type want struct {
		eventKind     protocol.AgentStreamEventKind
		text          string
		toolName      string
		toolCallID    string
		isCompletion  bool
		expectUnknown bool
	}
	cases := []struct {
		name string
		raw  string
		want want
	}{
		{
			name: "agentMessage.delta.text",
			raw:  `{"method":"item/agentMessage/delta","params":{"text":"hi"}}`,
			want: want{eventKind: protocol.EventText, text: "hi"},
		},
		{
			name: "agentMessage.delta.variant",
			raw:  `{"method":"item/agentMessage/delta","params":{"delta":"hi"}}`,
			want: want{eventKind: protocol.EventText, text: "hi"},
		},
		{
			name: "toolCall",
			raw:  `{"method":"item/toolCall","params":{"name":"bash","id":"c1","arguments":{"cmd":"ls"}}}`,
			want: want{
				eventKind: protocol.EventToolCall,
				toolName: "bash", toolCallID: "c1",
			},
		},
		{
			name: "toolResult.success",
			raw:  `{"method":"item/toolResult","params":{"result":"ok"}}`,
			want: want{eventKind: protocol.EventToolResult},
		},
		{
			name: "toolResult.error",
			raw:  `{"method":"item/toolResult","params":{"error":"boom"}}`,
			want: want{eventKind: protocol.EventToolResult},
		},
		{
			name: "permission.command",
			raw:  `{"method":"item/commandExecution/requestApproval","params":{"cmd":"rm -rf /"}}`,
			want: want{eventKind: protocol.EventPermission},
		},
		{
			name: "permission.fileChange",
			raw:  `{"method":"item/fileChange/requestApproval","params":{}}`,
			want: want{eventKind: protocol.EventPermission},
		},
		{
			name: "turnCompleted",
			raw: `{"method":"turn/completed","params":{"usage":{"inputTokens":10,"outputTokens":5,"costUsd":0.001},"finish":{"reason":"end_turn","success":true},"threadId":"thr-1"}}`,
			want: want{isCompletion: true},
		},
		{
			name: "unknown",
			raw:  `{"method":"item/someFutureKind","params":{}}`,
			want: want{expectUnknown: true},
		},
		{
			name: "noMethod",
			raw:  `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
			want: want{expectUnknown: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &raw); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			m := mapNotification(raw)
			if tc.want.isCompletion {
				if m.completion == nil {
					t.Fatalf("completion = nil, want non-nil")
				}
				if m.event.Kind != "" {
					t.Errorf("event.Kind = %v, want empty (turn/completed should not be a stream event)", m.event.Kind)
				}
				return
			}
			if m.completion != nil {
				t.Errorf("completion != nil for non-completion notification")
			}
			if tc.want.expectUnknown {
				if m.event.Kind != "" {
					t.Errorf("event.Kind = %v, want empty (unknown should be skipped)", m.event.Kind)
				}
				return
			}
			if m.event.Kind != tc.want.eventKind {
				t.Errorf("event.Kind = %v, want %v", m.event.Kind, tc.want.eventKind)
			}
			if m.event.Text != tc.want.text {
				t.Errorf("event.Text = %q, want %q", m.event.Text, tc.want.text)
			}
			if m.event.ToolName != tc.want.toolName {
				t.Errorf("event.ToolName = %q, want %q", m.event.ToolName, tc.want.toolName)
			}
			if m.event.ToolCallID != tc.want.toolCallID {
				t.Errorf("event.ToolCallID = %q, want %q", m.event.ToolCallID, tc.want.toolCallID)
			}
		})
	}
}

// TestExtractTurnResult covers the usage / finish / threadId
// extraction paths (independent of the mapper dispatch).
func TestExtractTurnResult(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		check func(t *testing.T, r *protocol.AgentTurnResult)
	}{
		{
			name: "full",
			raw:  `{"usage":{"inputTokens":10,"outputTokens":5,"costUsd":0.001},"finish":{"reason":"end_turn","success":true},"threadId":"thr-1"}`,
			check: func(t *testing.T, r *protocol.AgentTurnResult) {
				if r.SessionID != "thr-1" {
					t.Errorf("SessionID = %q, want thr-1", r.SessionID)
				}
				if r.Usage.InputTokens != 10 || r.Usage.OutputTokens != 5 {
					t.Errorf("Usage tokens = %+v, want {10,5}", r.Usage)
				}
				if r.Usage.CostUSD != 0.001 {
					t.Errorf("CostUSD = %v, want 0.001", r.Usage.CostUSD)
				}
				if !r.Finish.Success || r.Finish.Reason != "end_turn" {
					t.Errorf("Finish = %+v, want success/end_turn", r.Finish)
				}
			},
		},
		{
			name: "costUSD.variant",
			raw:  `{"usage":{"costUSD":0.5},"finish":{}}`,
			check: func(t *testing.T, r *protocol.AgentTurnResult) {
				if r.Usage.CostUSD != 0.5 {
					t.Errorf("CostUSD = %v, want 0.5 (costUSD variant should be accepted)", r.Usage.CostUSD)
				}
			},
		},
		{
			name: "empty",
			raw:  `{}`,
			check: func(t *testing.T, r *protocol.AgentTurnResult) {
				if r.Usage.InputTokens != 0 || r.Usage.OutputTokens != 0 || r.Usage.CostUSD != 0 {
					t.Errorf("Usage should be zero, got %+v", r.Usage)
				}
				if r.Finish.Success || r.Finish.Reason != "" {
					t.Errorf("Finish should be zero, got %+v", r.Finish)
				}
			},
		},
		{
			name: "missingFinish",
			raw:  `{"usage":{"inputTokens":7}}`,
			check: func(t *testing.T, r *protocol.AgentTurnResult) {
				if r.Usage.InputTokens != 7 {
					t.Errorf("InputTokens = %d, want 7", r.Usage.InputTokens)
				}
				if r.Finish.Success {
					t.Errorf("Finish.Success = true, want false (default zero value)")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var params map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &params); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			r := extractTurnResult(params)
			if r == nil {
				t.Fatalf("extractTurnResult = nil")
			}
			tc.check(t, r)
		})
	}
}

// TestSessionMissingBinary verifies NewSession surfaces a clear
// error when the binary can't be found.
func TestSessionMissingBinary(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := NewSession(ctx, SessionConfig{
		Bin:  "/nonexistent/conductor-fake-codex-binary",
		Home: t.TempDir(),
	})
	if err == nil {
		t.Fatalf("expected error for missing binary, got nil")
	}
	if !strings.Contains(err.Error(), "start codex app-server") {
		t.Errorf("error should mention codex startup, got: %v", err)
	}
}
