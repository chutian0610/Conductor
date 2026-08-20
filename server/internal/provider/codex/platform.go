package codex

import (
	"os"
	"runtime"
	"syscall"
	"time"
)

// osEnviron is split out for testability: tests can swap it to a
// hermetic env without polluting the test runner.
var osEnviron = os.Environ

// unixSIGTERM returns the SIGTERM signal on unix-like systems. On
// Windows, we use os.Kill as a fallback since Go's syscall.SIGTERM
// is unreliable there; Phase 1 targets darwin/linux only.
func unixSIGTERM() syscall.Signal {
	if runtime.GOOS == "windows" {
		return syscall.SIGKILL
	}
	return syscall.SIGTERM
}

// afterChan returns a channel that fires after d. Extracted so tests
// can stub time.After.
var afterChan = func(d time.Duration) <-chan time.Time {
	return time.After(d)
}
