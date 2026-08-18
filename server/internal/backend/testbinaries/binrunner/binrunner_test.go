package binrunner

// Smoke tests for the test-fixture runner. They drive the same
// behaviour that the integration tests rely on (script parsing,
// argv/stdin recording, exit codes) without needing a real
// conductor harness.

import (
	"bytes"
	"encoding/json"
	"io"
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

// --- drainStdin ---------------------------------------------------------

// TestDrainStdin_Happy exercises the path the production runner uses:
// claude's stdin is the prompt (JSON line); the fake drains it so the
// parent can close its write end and not block.
func TestDrainStdin_Happy(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "stdin.txt")

	// Replace os.Stdin with a pipe we control. Restore in cleanup so
	// later tests see the original stdin.
	origStdin := os.Stdin
	t.Cleanup(func() { os.Stdin = origStdin })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r

	const want = `{"prompt":"hello","session":"sess-X"}`
	go func() {
		_, _ = w.Write([]byte(want))
		_ = w.Close()
	}()

	if err := drainStdin(out); err != nil {
		t.Fatalf("drainStdin: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("drained content mismatch: got %q want %q", got, want)
	}
}

// TestDrainStdin_CreateError verifies the failure surface when the
// destination path can't be created (read-only dir, missing parent).
func TestDrainStdin_CreateError(t *testing.T) {
	// Empty cwd + a path that requires a missing segment under a
	// non-creatable parent. We can simulate with a path whose parent
	// is a regular file.
	tmp := t.TempDir()
	parent := filepath.Join(tmp, "regular")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := drainStdin(filepath.Join(parent, "child.txt"))
	if err == nil {
		t.Fatal("expected error when parent is a regular file")
	}
}

// TestDrainStdin_PropagatesEOF verifies the path completes cleanly when
// the writer side of the pipe is closed without sending any bytes.
// drainStdin treats EOF as a successful empty drain.
func TestDrainStdin_PropagatesEOF(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "empty.txt")

	origStdin := os.Stdin
	t.Cleanup(func() { os.Stdin = origStdin })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	_ = w.Close() // immediate EOF

	if err := drainStdin(out); err != nil {
		t.Fatalf("drainStdin should swallow EOF cleanly, got %v", err)
	}
	got, _ := os.ReadFile(out)
	if len(got) != 0 {
		t.Fatalf("expected empty drain on EOF, got %q", got)
	}
}

// --- scanFlags ----------------------------------------------------------

// TestScanFlags_FlagRequiringValueMissing exercises the "flag present
// but no value follows" branch — scanFlags should consume the flag
// without setting the target (so the caller sees the zero value).
func TestScanFlags_FlagRequiringValueMissing(t *testing.T) {
	var out string
	var argv []struct {
		name   string
		target *string
	}
	_ = argv
	flags := map[string]*flagTarget{
		"script": {value: &out, takesValue: true},
	}
	// `--script` is the last argv element, no value follows.
	scanFlags([]string{"--script"}, flags)
	if out != "" {
		t.Fatalf("script path should be empty when value missing, got %q", out)
	}
}

// TestScanFlags_BoolPresence exercises the no-argument boolean path:
// `--block` with no value following must still register as true.
func TestScanFlags_BoolPresence(t *testing.T) {
	var block bool
	flags := map[string]*flagTarget{
		"block": {value: &block, takesValue: false},
	}
	scanFlags([]string{"--block"}, flags)
	if !block {
		t.Fatal("--block should set true")
	}
}

// TestScanFlags_SingleDash exercises the alternate prefix that
// scanFlags recognises (`--` and `-` both).
func TestScanFlags_SingleDash(t *testing.T) {
	var v string
	flags := map[string]*flagTarget{
		"script": {value: &v, takesValue: true},
	}
	scanFlags([]string{"-script", "/tmp/script.jsonl"}, flags)
	if v != "/tmp/script.jsonl" {
		t.Fatalf("single-dash prefix ignored, got %q", v)
	}
}

// --- writeArgvJSONL -----------------------------------------------------

// TestWriteArgvJSONL_EmptyInput documents the empty-arg edge — should
// produce an empty file without erroring.
func TestWriteArgvJSONL_EmptyInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "argv.jsonl")
	if err := writeArgvJSONL(path, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty file, got %q", got)
	}
}

// TestWriteArgvJSONL_EncodingRoundTrip: the file written must parse
// back to the exact argv we passed in.
func TestWriteArgvJSONL_EncodingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "argv.jsonl")
	in := []string{"a", "b\"c", "d\ne", "—", "中"}
	if err := writeArgvJSONL(path, in); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var s string
		if err := json.Unmarshal(line, &s); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		out = append(out, s)
	}
	if len(out) != len(in) {
		t.Fatalf("length: got %d want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("[%d] got %q want %q", i, out[i], in[i])
		}
	}
}

// --- runScript ----------------------------------------------------------

// TestRunScript_BlankAndComments pins the parser's tolerance for
// blank lines and `#` comments in scenario files.
func TestRunScript_BlankAndComments(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "s.jsonl")
	const content = "" +
		"# header comment\n" +
		"\n" +
		"   \n" +
		`{"event":{"kind":"first"}}` + "\n" +
		"   \n" +
		"# trailing\n" +
		`{"event":{"kind":"second"}}` + "\n"
	mustWriteFile(t, script, content)

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
	out := readAll(t, r)
	for _, want := range []string{`{"kind":"first"}`, `{"kind":"second"}`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("missing event %s in output %q", want, out)
		}
	}
	// And only those two — blank/comment lines must not emit JSON.
	if bytes.Count(out, []byte(`{"kind":`)) != 2 {
		t.Errorf("unexpected extra events: %q", out)
	}
}

// TestRunScript_InvalidJSONLine verifies that a malformed line fails
// the run with a wrapped error (runScript is called by Run(), which
// would os.Exit(1) on this branch).
func TestRunScript_InvalidJSONLine(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bad.jsonl")
	mustWriteFile(t, script, "{not valid json")
	err := runScript(script)
	if err == nil {
		t.Fatal("expected parse error on malformed line")
	}
	if !strings.Contains(err.Error(), "parse script line") {
		t.Fatalf("error message not wrapped: %v", err)
	}
}

// TestRunScript_OpensMissingFile verifies the io.Open error path.
func TestRunScript_OpensMissingFile(t *testing.T) {
	err := runScript(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if err == nil {
		t.Fatal("expected error on missing file")
	}
}

// --- helpers ------------------------------------------------------------

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	return out
}
