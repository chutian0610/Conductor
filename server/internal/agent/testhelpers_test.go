package agent

import (
	"io"
	"log/slog"
	"os"
	"testing"
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
