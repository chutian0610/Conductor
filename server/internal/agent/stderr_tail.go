package agent

import (
	"bytes"
	"io"
	"log/slog"
	"sync"
)

// stderrTailBytes bounds the stderr buffer captured during one execution.
// When the subprocess crashes with no useful Result.Error, the last few KB
// of stderr are attached to the failure message — without this, an
// exit-code-only failure looks like "claude exited with error: exit
// status 3", which is useless for root-causing V8 aborts, Bun panics, or
// any other CLI-side crash.
const stderrTailBytes = 16 * 1024

// stderrTail captures the last stderrTailBytes of bytes the subprocess
// wrote to stderr. Concurrently it tees every line through to an slog
// writer so operators see the full stderr stream in real time.
type stderrTail struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	cap  int
	tail []byte
	out  io.Writer // everything goes here too (typically the slog writer)
}

func newStderrTail(logWriter io.Writer) *stderrTail {
	return &stderrTail{cap: stderrTailBytes, out: logWriter}
}

func (s *stderrTail) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(p)
	if s.buf.Len() > s.cap {
		// Keep only the most recent cap bytes.
		data := s.buf.Bytes()
		s.tail = append(s.tail[:0], data[len(data)-s.cap:]...)
	} else if s.tail == nil {
		// First write under the cap: snapshot the buffer.
		s.tail = append([]byte(nil), s.buf.Bytes()...)
	}
	if s.out != nil {
		_, _ = s.out.Write(p)
	}
	return len(p), nil
}

// Tail returns the most recent stderr bytes (at most stderrTailBytes).
// Safe to call after the subprocess has exited.
func (s *stderrTail) Tail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tail == nil {
		return ""
	}
	return string(s.tail)
}

// slogLineWriter adapts slog.Logger so it can be used as an io.Writer for
// per-line stderr teeing. Each Write call emits one Info record.
type slogLineWriter struct {
	logger *slog.Logger
	prefix string
}

func newSlogLineWriter(logger *slog.Logger, prefix string) *slogLineWriter {
	return &slogLineWriter{logger: logger, prefix: prefix}
}

func (w *slogLineWriter) Write(p []byte) (int, error) {
	w.logger.Info(w.prefix + string(p))
	return len(p), nil
}

// withStderrTail attaches the captured stderr tail to a failure message
// so the user can see what the CLI was complaining about. No-op when
// errMsg is empty.
func withStderrTail(msg, tail string) string {
	if tail == "" {
		return msg
	}
	return msg + "\n--- stderr (tail) ---\n" + tail
}
