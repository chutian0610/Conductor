package backend

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"conductor/server/internal/backend/testbinaries/binrunner"
)

// testLogger returns a slog logger that emits at Warn level to the test's
// log. Live tests otherwise generate a torrent of JSON logs per protocol
// event; warning-only keeps noise down while still surfacing protocol
// summary lines on failure.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))
}

// writeFile / readFile are thin wrappers so individual tests stay short.
func writeFile(path, content string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(content), mode)
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// ScriptStep is one entry in a fake CLI's scenario file. Step values
// match binrunner.ScriptStep so an integration test can build a list in
// memory and pass it straight through.
type ScriptStep = binrunner.ScriptStep

// WriteScript serialises steps as JSONL and writes them to <dir>/<name>.
// Returns the absolute path.
func WriteScript(t *testing.T, dir, name string, steps []ScriptStep) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create script %s: %v", path, err)
	}
	defer f.Close()
	for i, step := range steps {
		if err := json.NewEncoder(f).Encode(step); err != nil {
			t.Fatalf("encode script step %d: %v", i, err)
		}
	}
	return path
}

// ReadArgv decodes the JSONL argv file produced by `--argv`. Each line
// is one argv element.
func ReadArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read argv %s: %v", path, err)
	}
	var out []string
	for _, line := range bytesSplit(data, '\n') {
		if len(line) == 0 {
			continue
		}
		var s string
		if err := json.Unmarshal(line, &s); err != nil {
			t.Fatalf("decode argv line %q: %v", line, err)
		}
		out = append(out, s)
	}
	return out
}

// bytesSplit is a tiny helper that does what bytes.Split does without
// the empty trailing element.
func bytesSplit(b []byte, sep byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == sep {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	out = append(out, b[start:])
	return out
}

// MustBuildFakeBinary compiles one of the test binaries
// (fake-claude, fake-codex) on demand from
// internal/agent/testbinaries/<name> under the -tags testbinaries
// build tag and returns the absolute path of the resulting
// executable. The binary lives in a per-test temp dir and is cleaned
// up automatically when the test ends.
//
// Build time is ~1 s on a warm cache; tests that share a binary
// should call this once in TestMain and cache the path, but the
// current per-test invocation is fast enough that caching is not
// required.
func MustBuildFakeBinary(t *testing.T, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("look up `go`: %v", err)
	}
	cmd := exec.Command(goBin, "build", "-tags", "testbinaries",
		"-o", out, "./internal/backend/testbinaries/"+name)
	cmd.Dir = serverRoot(t)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return out
}

// serverRoot walks up from this source file to find the directory
// that holds server/go.mod. Used to anchor `go build` invocations
// in MustBuildFakeBinary.
func serverRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// this file lives at <server>/internal/agent/testhelpers_test.go,
	// so the server root is three parents up.
	d := filepath.Dir(file) // .../internal/backend
	d = filepath.Dir(d)     // .../internal
	return filepath.Dir(d)  // .../server
}
