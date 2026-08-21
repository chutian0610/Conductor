package codex

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestWriteCodexFake is a smoke test for the fakecodex helper itself.
// It spins up the fake, runs the full initialize handshake, then
// sends a ping. Verifies the responses parse cleanly.
func TestWriteCodexFake(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Empty customizer: just rely on the default case (echo ok + 1 notification).
	scriptPath := WriteCodexFake(t, "")

	c, err := NewClient(ctx, ClientConfig{
		Bin:  "/bin/sh",
		Args: []string{scriptPath},
		Home: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	var result struct {
		OK bool `json:"ok"`
	}
	if err := c.Call(ctx, "ping", nil, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !result.OK {
		t.Errorf("result.OK = false, want true")
	}
}

// TestWriteCodexFakeCustomizer verifies the customizer case
// branches run inside the case statement, with $ID available.
func TestWriteCodexFakeCustomizer(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Customizer: thread/start echoes a specific threadId; other
	// methods fall through to the default case.
	scriptPath := WriteCodexFake(t, `    thread/start)
      printf '%s\n' '{"jsonrpc":"2.0","id":'$ID',"result":{"threadId":"thr-custom"}}'
      ;;
`)

	c, err := NewClient(ctx, ClientConfig{
		Bin:  "/bin/sh",
		Args: []string{scriptPath},
		Home: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	var result struct {
		ThreadID string `json:"threadId"`
	}
	if err := c.Call(ctx, "thread/start", map[string]any{"cwd": "/tmp"}, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.ThreadID != "thr-custom" {
		t.Errorf("thread.id = %q, want thr-custom", result.ThreadID)
	}
}
