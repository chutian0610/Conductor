package binrunner

// Smoke tests for the test-fixture runner. They drive the same
// behaviour that the integration tests rely on (script parsing,
// argv/stdin recording, exit codes) without needing a real
// conductor harness.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_RecordsArgvAndExits(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "s.jsonl")
	argvFile := filepath.Join(dir, "argv.jsonl")

	must(t, os.WriteFile(script, []byte(`{"event":{"hello":"world"}}`), 0o600))

	// Re-exec this test binary as the fake would: pass --script and
	// --argv so it writes the record and emits the event. Using `os.Args[0]`
	// keeps the test dependent only on itself.
	exitFile := filepath.Join(dir, "exit")
	must(t, os.WriteFile(exitFile, []byte(""), 0o600))
	defer func() { _ = os.Remove(exitFile) }()

	// We cannot call Run() in-process (it os.Exit's), so emulate it
	// by setting up the same flag wiring via scanFlags and the
	// public helpers. Use a fresh FlagSet with the same registration
	// scheme Run() uses, parse, and assert the side-effects.
	var scriptPath, argvPath string
	var block bool
	var exitCode int
	flags := map[string]*flagTarget{
		"script": {value: &scriptPath, takesValue: true},
		"argv":   {value: &argvPath, takesValue: true},
		"stdin":  {value: new(string), takesValue: true}, // unused
		"block":  {value: &block, takesValue: false},
		"exit":   {value: &exitCode, takesValue: true},
	}
	argv := []string{
		// unknown flag (production-CLI shape)
		"-p", "--output-format", "stream-json",
		// ours
		"--script=" + script,
		"--argv=" + argvFile,
		"--exit=0",
	}
	scanFlags(argv, flags)

	if scriptPath != script {
		t.Fatalf("scriptPath = %q, want %q", scriptPath, script)
	}
	if argvPath != argvFile {
		t.Fatalf("argvPath = %q, want %q", argvPath, argvFile)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if block {
		t.Error("block = true, want false")
	}

	// Now exercise writeArgvJSONL and runScript in-process.
	mustWrite(t, argvFile, argv)

	if err := writeArgvJSONL(argvFile, argv); err != nil {
		t.Fatalf("writeArgvJSONL: %v", err)
	}
	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		lines = append(lines, l)
	}
	if len(lines) != len(argv) {
		t.Errorf("argv record has %d lines, want %d (%v)", len(lines), len(argv), lines)
	}
	for _, want := range []string{`"-p"`, `"--output-format"`, `"stream-json"`, `"--script=` + script + `"`} {
		found := false
		for _, l := range lines {
			if strings.Contains(l, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("argv record missing %q in %v", want, lines)
		}
	}

	if err := runScript(script); err != nil {
		t.Fatalf("runScript: %v", err)
	}
}

func TestRunScript_OneEventEmitsOneLine(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "s.jsonl")
	line := ScriptStep{Event: json.RawMessage(`{"kind":"x"}`)}
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(script, append(b, '\n'), 0o600))

	// Capture stdout via pipe redirection.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = origStdout })

	if err := runScript(script); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	_ = w.Close()

	got := make([]byte, 0, 256)
	buf := make([]byte, 256)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	_ = r.Close()
	if !strings.Contains(string(got), `{"kind":"x"}`) {
		t.Errorf("stdout missing event; got %q", string(got))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, argv []string) {
	t.Helper()
	for _, l := range argv {
		_ = path
		_ = l
	}
}
