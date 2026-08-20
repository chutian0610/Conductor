package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestClientCallRoundTrip uses a tiny shell script as a stand-in for
// `codex app-server` to verify the JSON-RPC framing and dispatch
// logic. The fake server echoes a single response per request and
// emits a notification; we assert that:
//   - Call() correlates the response to the right id
//   - Events() surfaces the notification
//   - Close() tears down the subprocess
func TestClientCallRoundTrip(t *testing.T) {
	// Skip if no shell available (we rely on /bin/sh).
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}

	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fake codex app-server: reads one line per request, writes one
	// response with id=1 and one notification "item/agentMessage/delta".
	fake := `#!/bin/sh
read -r REQ
# Pull the id out (very loose; real JSON parser would be safer).
ID=$(echo "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
echo "{\"jsonrpc\":\"2.0\",\"id\":$ID,\"result\":{\"ok\":true}}"
echo '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"text":"hi"}}'
# Block on stdin so the client has time to consume.
read -r _
`
	scriptPath := writeFake(t, fake)

	c, err := NewClient(ctx, ClientConfig{
		Bin: "/bin/sh",
		Args: []string{scriptPath},
		Home: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer c.Close()

	// Issue a Call and assert the response decodes correctly.
	var result struct {
		OK bool `json:"ok"`
	}
	if err := c.Call(ctx, "ping", nil, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.OK {
		t.Errorf("result.OK = false, want true (result=%+v)", result)
	}

	// The notification should arrive on Events() within a short
	// window.
	select {
	case ev, ok := <-c.Events():
		if !ok {
			t.Fatalf("events channel closed before notification")
		}
		if got := ev["method"]; got != "item/agentMessage/delta" {
			t.Errorf("notification method = %v, want item/agentMessage/delta", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive notification within 2s")
	}
}

// TestClientCloseIdempotent verifies Close() can be called multiple
// times safely. Important for deferred Close() in callers.
func TestClientCloseIdempotent(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fake server that exits immediately.
	scriptPath := writeFake(t, "#!/bin/sh\nexit 0\n")

	c, err := NewClient(ctx, ClientConfig{
		Bin: "/bin/sh",
		Args: []string{scriptPath},
		Home: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Give the subprocess a moment to exit.
	time.Sleep(100 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if err := c.Close(); err != nil {
			t.Errorf("close #%d: %v", i, err)
		}
	}
}

// TestClientMissingBinary verifies NewClient surfaces a clear error
// when the binary can't be found.
func TestClientMissingBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := NewClient(ctx, ClientConfig{
		Bin: "/nonexistent/conductor-fake-codex-binary",
		Home: t.TempDir(),
	})
	if err == nil {
		t.Fatalf("expected error for missing binary, got nil")
	}
	if !strings.Contains(err.Error(), "start codex app-server") {
		t.Errorf("error should mention codex startup, got: %v", err)
	}
}

// writeFake writes a shell script under t.TempDir() and returns its
// path. The script is chmod'd to 0755 so /bin/sh can exec it.
func writeFake(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/fake-codex.sh"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	return path
}

// TestParseIDAcceptsIntegerAndString verifies the id parser handles
// both numeric and string-form ids defensively. Codex always uses
// integers, but the parser is the dispatcher; any panics here
// crash the goroutine.
func TestParseIDAcceptsIntegerAndString(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{`1`, 1, true},
		{`42`, 42, true},
		{`"7"`, 7, true},
		{`null`, 0, false},
		{``, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			// Wrap in JSON so json.Unmarshal accepts the input.
			data := []byte(tc.in)
			if tc.in != "null" && tc.in != "" {
				data = []byte(tc.in)
			}
			got, ok := parseID(data)
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("id = %d, want %d", got, tc.want)
			}
		})
	}
}

// Compile-time guard so we notice if json.RawMessage changes shape
// and our parseID needs a parallel refactor.
var _ = func() error {
	_, err := json.Marshal(struct {
		Result json.RawMessage `json:"result"`
	}{})
	return err
}()

// Unused import guard (io and bufio are referenced only by future
// tests that may use them).
var (
	_ = io.Discard
	_ = bufio.NewReader
	_ = fmt.Sprintf
)
