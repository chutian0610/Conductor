//go:build windows

// This file is here to satisfy the Go toolchain's build requirements on
// Windows (the platform-specific functions in proc_other.go are not
// available). Conductor does NOT support Windows — the binary will
// compile cleanly but refuse to spawn any agent at runtime with a clear
// "platform not supported" error.
//
// Supported platforms: macOS and Linux only. The Unix process-group
// mechanism (Setpgid + signal-the-group) in proc_other.go has no
// portable Windows equivalent; supporting Windows would require Job
// Objects (see multica's server/pkg/agent/proc_windows.go for a
// reference implementation), which is out of scope for this project.
package backend

import (
	"fmt"
	"log/slog"
	"os/exec"
	"syscall"
	"time"
)

// errWindowsUnsupported is returned from any agent entry point so the
// failure surfaces in the CLI with a single clear message rather than
// a confusing mid-spawn error.
var errWindowsUnsupported = fmt.Errorf(
	"conductor: Windows is not a supported platform. " +
		"Supported: macOS, Linux. " +
		"See README \"Platform support\" for details.",
)

func hideAgentWindow(_ *exec.Cmd) {}

// configureProcessGroup is a no-op on the unsupported platform. The
// functions still have to exist to satisfy the cross-platform contract
// in proc_other.go.
func configureProcessGroup(_ *exec.Cmd) {}

// startOwnedProcessTree refuses to launch anything. Returning an error
// here means backend.Backend.Execute fails fast before spawning a process
// the project cannot manage.
func startOwnedProcessTree(_ *exec.Cmd, _ *slog.Logger) error {
	return errWindowsUnsupported
}

// releaseProcessGroup is a no-op; there is no group to release.
func releaseProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup is a no-op; the platform cannot signal a tree that
// was never owned.
func signalProcessGroup(_ *exec.Cmd, _ syscall.Signal) {}

// waitProcessGroupGone always reports "could not confirm cleanup",
// matching the no-op semantics on this platform.
func waitProcessGroupGone(_ *exec.Cmd, _ time.Duration) bool { return false }
