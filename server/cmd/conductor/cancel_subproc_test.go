package main

import (
	"bytes"
	"os/exec"
	"sync"
)

// runGoBuild shells out to `go build`. Defined in a separate
// _test.go file to keep the main test file's import block tidy.
func runGoBuild(args ...string) error {
	cmd := exec.Command("go", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &buildError{out: out, err: err}
	}
	return nil
}

type buildError struct {
	out []byte
	err error
}

func (e *buildError) Error() string {
	return "go build: " + e.err.Error() + "\n" + string(e.out)
}

// startProc launches a child process with the given env + args.
// stdout/stderr are captured to in-memory buffers (so we can
// inspect them when the test fails) and returned via the
// process's Stdout/Stderr fields. The caller is responsible for
// not blocking on the underlying pipes (long-lived children).
func startProc(env []string, bin string, args ...string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var stdout, stderr safeBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		panic("startProc: " + err.Error())
	}
	// Stash the buffers so the test can read them on failure.
	type result struct {
		stdout, stderr *safeBuffer
	}
	_ = result{stdout: &stdout, stderr: &stderr}
	return cmd
}

// runCmdWithErr is like runCmd but returns the error.
func runCmdWithErr(env []string, bin string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var buf safeBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// safeBuffer wraps bytes.Buffer with a mutex for concurrent
// Stdout + Stderr writes (the os/exec package writes to them
// from different goroutines).
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}
