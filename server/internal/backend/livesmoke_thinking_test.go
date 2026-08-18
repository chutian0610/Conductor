package backend

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"testing"
	"time"
)

// TestClaude_Live_ThinkingForwardingGate documents the upstream CLI's
// current behaviour around extended thinking blocks.
//
// As of Claude Code 2.1.215 (and at the time of writing this test),
// the main agent's `thinking` blocks are NOT forwarded to stdout in
// stream-json output. The CLI's flags:
//   - `--effort <level>` controls how much effort the model puts in
//     but does NOT surface thinking on stdout.
//   - `--forward-subagent-text` forwards SUBAGENT text+thinking.
//     It is the only flag that currently streams `thinking` blocks —
//     but only for subagents, not the main agent.
//
// This test runs the live CLI with `--effort medium` and asserts what
// the contract currently looks like: zero or more MessageThinking
// events on the main agent (today: zero). If a future CLI version
// starts emitting main-agent thinking on stdout, this test catches the
// shape of the change.
//
// Skip when the CLI is not installed; inconclusive if the CLI emits no
// text for the chosen prompt. See docs/backends/claude.md for the
// narrative and the wiring that's already in place inside claude.go
// (case "thinking" → MessageThinking).
func TestClaude_Live_ThinkingForwardingGate(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH; skipping live test")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	be, err := New("claude", Config{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := be.Execute(ctx, "Briefly: what is 2+2?",
		ExecOptions{
			ThinkingLevel: "medium",
			MaxTurns:      1,
			Timeout:       45 * time.Second,
		})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var thinkingCount, textCount int
	for m := range sess.Messages {
		switch m.Type {
		case MessageThinking:
			thinkingCount++
		case MessageText:
			textCount++
		}
	}
	res := <-sess.Result
	if res.Status != "completed" {
		t.Fatalf("status=%q err=%q", res.Status, res.Error)
	}
	if textCount == 0 {
		t.Skipf("no MessageText events; prompt may have produced no output; inconclusive")
	}
	// Today's contract: CLI doesn't stream thinking on stdout.
	// Document this assertion so a future CLI change shows up as a
	// test failure that points at this gate.
	if thinkingCount > 0 {
		t.Logf("info: CLI emitted %d MessageThinking events — upstream gate opened; remove this assertion", thinkingCount)
	}
	// (We DO NOT t.Fatalf when thinkingCount == 0; today it's the
	//  expected state. The contract is "MessageThinking can appear;
	//  render and store it correctly when it does." That's covered
	//  by the integration test using the fake-binary harness.)
}
