package codex

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// WriteCodexFake writes a shell-script fake of `codex app-server`
// that handles the codex 0.147+ initialize handshake, then defers
// to caller-supplied customizer shell code for any other request.
//
// The generated script reads each JSON-RPC line from stdin, parses
// out the request id and method, and dispatches via a bash case:
//   - initialize (id=N): echoes the standard init response
//     (userAgent, codexHome, platformFamily, platformOs), then
//     echoes the `initialized` notification.
//   - initialized (notification, no id): does nothing — pumpStdout
//     routes notifications to c.events, no response needed.
//   - other methods: runs the customizer shell snippet, which
//     is expected to echo the appropriate JSON-RPC response and
//     any notifications. If the customizer is empty, the default
//     case below applies.
//
// The customizer is a string of valid bash code inserted INSIDE
// the case statement. It can introduce additional case branches:
//
//	thread/start)
//	  echo '{"jsonrpc":"2.0","id":'$ID',"result":{"threadId":"thr-x"}}'
//	  ;;
//
// Or it can be a stream of `printf`/`echo` statements that produce
// the right JSON-RPC output. Use `exit 0` to terminate the fake
// after handling one request.
//
// $ID and $REQ are available; $METHOD too. The closing `esac` and
// `done` loop are added by this helper.
func WriteCodexFake(t *testing.T, customizer string) string {
	t.Helper()
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake.sh")
	// The script body. Note the double-percent escapes: Go's fmt.Sprintf
	// treats %% as a literal %, and the final %s interpolates the
	// customizer. Inside the case statement, we have $ID, $REQ, $METHOD
	// from the surrounding bash code.
	script := fmt.Sprintf(`#!/bin/sh
# Handles the codex 0.147+ initialize handshake automatically.
# Caller's customizer (below) handles every other method.
#
# The trap ensures SIGTERM (from Client.Close on a dead-but-not-yet-reaped
# subprocess, or from a hung test) exits cleanly with code 0 instead of
# "signal: terminated" — which would make cmd.Wait return a non-nil error.
trap "exit 0" INT TERM
while read -r REQ; do
  ID=$(printf '%%s' "$REQ" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  METHOD=$(printf '%%s' "$REQ" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  case "$METHOD" in
    initialize)
      printf '%%s\n' '{"jsonrpc":"2.0","id":'$ID',"result":{"userAgent":"Conductor test","codexHome":"/tmp","platformFamily":"unix","platformOs":"linux"}}'
      printf '%%s\n' '{"jsonrpc":"2.0","method":"initialized","params":{}}'
      ;;
    initialized)
      # Notification, no response needed.
      ;;
%s
    *)
      # Default: echo a basic "ok" response + one item/agentMessage/delta
      # notification. Used when the customizer doesn't handle a method.
      printf '%%s\n' '{"jsonrpc":"2.0","id":'$ID',"result":{"ok":true}}'
      printf '%%s\n' '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"text":"hi"}}'
      ;;
  esac
done
`, customizer)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteCodexFake: %v", err)
	}
	return path
}
