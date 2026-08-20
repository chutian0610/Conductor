package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"conductor/server/internal/home"
	"conductor/server/internal/protocol"
	"conductor/server/internal/storage"
	"conductor/server/internal/spec"
)

// fakeCodexScript is a shell stand-in for the codex app-server that
// speaks just enough JSON-RPC to drive runner tests. Recognised
// methods:
//
//	thread/start  → {"result":{"threadId":"thr-fake"}}
//	turn/start    → {"result":{"ok":true}}, then emits:
//	                 item/agentMessage/delta {"text":"hi"}
//	                 turn/completed          (usage + finish + threadId)
//
// Anything else → JSON-RPC error -32601.
const fakeCodexScript = `#!/bin/sh
while read -r REQ; do
  METHOD=$(printf '%s' "$REQ" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  ID=$(printf '%s' "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$METHOD" in
    thread/start)
      echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-fake\"}}"
      ;;
    turn/start)
      echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"
      echo '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"text":"hi from fake"}}'
      echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"usage":{"inputTokens":7,"outputTokens":3,"costUsd":0.0042},"finish":{"reason":"end_turn","success":true},"threadId":"thr-fake"}}'
      # Block so the client has time to consume events before exit.
      exit 0
      ;;
    *)
      echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"error\":{\"code\":-32601,\"message\":\"unknown: ${METHOD}\"}}"
      ;;
  esac
done
`

// writeFakeCodex writes the fake script to a temp file and returns
// its path.
func writeFakeCodex(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-codex.sh")
	if err := os.WriteFile(path, []byte(fakeCodexScript), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	return path
}

// setupTestFixture creates a temp CONDUCTOR_HOME, registers a spec
// pointing at the fake codex binary (via CreateInput.Spec.Cwd isn't
// needed; we override Bin at the SessionConfig layer), and returns
// the spec id.
func setupTestFixture(t *testing.T, fakePath string) string {
	t.Helper()
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	ctx := context.Background()

	res, err := spec.Create(ctx, spec.CreateInput{
		Spec: protocol.AgentSpec{
			Provider: protocol.ProviderCodex,
			Model:    "test-model",
			Name:     "runner-test",
		},
		BaseURL: "https://example.com",
		EnvKey:  "EXAMPLE_KEY",
	})
	if err != nil {
		t.Fatalf("spec.Create: %v", err)
	}
	return res.SpecId
}

// TestInvokeHappyPath verifies the full flow: spec load → codex
// Session → turn → events → result.
func TestInvokeHappyPath(t *testing.T) {
	fakePath := writeFakeCodex(t)
	specId := setupTestFixture(t, fakePath)

	// The runner doesn't take a Bin override today (it gets the
	// spec's HOME and lets codex.NewSession pick "codex" from
	// PATH). For tests we need to make "codex" resolve to our
	// fake. We do that by putting a symlink to /bin/sh in PATH
	// that itself runs the fake — simpler: just add a small
	// "codex" wrapper to PATH that execs the fake with the right
	// args.
	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "codex")
	wrapperBody := "#!/bin/sh\nexec /bin/sh " + fakePath + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(wrapperBody), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var got []protocol.AgentStreamEvent
	var mu sync.Mutex
	handler := func(ev protocol.AgentStreamEvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}

	result, err := Invoke(context.Background(), specId, "hello", "test-run-id", storage.NoopStorage{}, handler, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.SessionID != "thr-fake" {
		t.Errorf("SessionID = %q, want thr-fake", result.SessionID)
	}
	if result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want {7, 3}", result.Usage)
	}
	if result.Usage.CostUSD != 0.0042 {
		t.Errorf("CostUSD = %v, want 0.0042", result.Usage.CostUSD)
	}
	if !result.Finish.Success || result.Finish.Reason != "end_turn" {
		t.Errorf("Finish = %+v", result.Finish)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(got), got)
	}
	if got[0].Kind != protocol.EventText || got[0].Text != "hi from fake" {
		t.Errorf("event[0] = %+v, want text 'hi from fake'", got[0])
	}
}

// TestInvokeSpecNotFound — bad specId returns spec.ErrNotFound
// (not some opaque error), so callers can distinguish "spec is
// missing" from "codex broke".
func TestInvokeSpecNotFound(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	_, err := Invoke(context.Background(), "no-such-spec", "x", "test-run-id", storage.NoopStorage{}, nil, nil)
	if !errors.Is(err, spec.ErrNotFound) {
		t.Errorf("err = %v, want spec.ErrNotFound", err)
	}
}

// TestInvokeRequiresSpecId — empty specId rejected up front.
func TestInvokeRequiresSpecId(t *testing.T) {
	_, err := Invoke(context.Background(), "", "x", "test-run-id", storage.NoopStorage{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "specID required") {
		t.Errorf("err = %v, want 'specID required'", err)
	}
}

// TestInvokeRequiresPrompt — empty prompt rejected up front.
func TestInvokeRequiresPrompt(t *testing.T) {
	fakePath := writeFakeCodex(t)
	specId := setupTestFixture(t, fakePath)

	// PATH wrapper not strictly needed (error fires before the
	// subprocess starts), but harmless.
	_, err := Invoke(context.Background(), specId, "", "test-run-id", storage.NoopStorage{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "prompt required") {
		t.Errorf("err = %v, want 'prompt required'", err)
	}
}

// TestInvokeOnEventNil covers the no-callback path — events are
// still consumed (so the pump doesn't block) but nothing is
// surfaced.
func TestInvokeOnEventNil(t *testing.T) {
	fakePath := writeFakeCodex(t)
	specId := setupTestFixture(t, fakePath)
	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "codex")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /bin/sh "+fakePath+` "$@"`+"\n"), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	result, err := Invoke(context.Background(), specId, "test", "test-run-id", storage.NoopStorage{}, nil, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.Finish.Success {
		t.Errorf("Finish.Success = false, want true")
	}
}

// TestInvokeCtxCancel verifies that cancelling ctx while a turn is
// in flight returns ctx.ErrError — the runner inherits Send's
// cancellation semantics.
func TestInvokeCtxCancel(t *testing.T) {
	fakePath := writeFakeCodex(t)
	specId := setupTestFixture(t, fakePath)
	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "codex")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /bin/sh "+fakePath+` "$@"`+"\n"), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Fake that blocks on turn/start so the turn never completes.
	slowFake := t.TempDir() + "/slow-fake.sh"
	if err := os.WriteFile(slowFake, []byte(`#!/bin/sh
while read -r REQ; do
  METHOD=$(printf '%s' "$REQ" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  ID=$(printf '%s' "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$METHOD" in
    thread/start) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-slow\"}}";;
    turn/start) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}";;
    turn/interrupt) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"; exit 0;;
  esac
done
`), 0o755); err != nil {
		t.Fatalf("write slow fake: %v", err)
	}
	// Replace the wrapper to point at the slow fake.
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /bin/sh "+slowFake+` "$@"`+"\n"), 0o755); err != nil {
		t.Fatalf("rewrite wrapper: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := Invoke(ctx, specId, "test", "test-run-id", storage.NoopStorage{}, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestInvokeSpecHomeIsolation verifies the codex subprocess gets
// HOME set to the spec's per-spec HOME — not the user's $HOME and
// not the test's $CONDUCTOR_HOME. This is the core of §6.2.5's
// isolation guarantee and worth pinning with a test.
//
// The fake records $HOME into a file inside the spec HOME before
// responding, so the test can read it back.
func TestInvokeSpecHomeIsolation(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	ctx := context.Background()

	// Create a spec to get its HOME path.
	res, err := spec.Create(ctx, spec.CreateInput{
		Spec: protocol.AgentSpec{
			Provider: protocol.ProviderCodex,
			Model:    "x",
			Name:     "iso-test",
		},
		BaseURL: "https://x",
	})
	if err != nil {
		t.Fatalf("spec.Create: %v", err)
	}

	// Fake that records $HOME to a known location relative to
	// the HOME we set, then proceeds with the normal flow.
	probeFile := filepath.Join(res.Record.HomePath, "probe.txt")
	fake := `#!/bin/sh
echo "$HOME" > ` + probeFile + `
while read -r REQ; do
  METHOD=$(printf '%s' "$REQ" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  ID=$(printf '%s' "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$METHOD" in
    thread/start) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"threadId\":\"thr-iso\"}}";;
    turn/start)
      echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"result\":{\"ok\":true}}"
      echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"usage":{},"finish":{"reason":"end_turn","success":true}}}'
      exit 0
      ;;
    *) echo "{\"jsonrpc\":\"2.0\",\"id\":${ID},\"error\":{\"code\":-32601}}";;
  esac
done
`
	fakePath := filepath.Join(t.TempDir(), "fake.sh")
	if err := os.WriteFile(fakePath, []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "codex")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /bin/sh "+fakePath+` "$@"`+"\n"), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := Invoke(ctx, res.SpecId, "test", "test-run-id", storage.NoopStorage{}, nil, nil); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	got, err := os.ReadFile(probeFile)
	if err != nil {
		t.Fatalf("read probe: %v (codex subprocess did not run with HOME=$CONDUCTOR_HOME/specs/<id>/home)", err)
	}
	gotHome := strings.TrimRight(string(got), "\n")
	if gotHome != res.Record.HomePath {
		t.Errorf("subprocess HOME = %q, want %q (per-spec isolation broken)", gotHome, res.Record.HomePath)
	}
}

// Compile-time guard: spec package's SpecRecord layout matches what
// the runner reads (catches accidental schema drift early).
var _ = func() *protocol.SpecRecord {
	return &protocol.SpecRecord{HomePath: home.SpecHomeDir("x")}
}()
