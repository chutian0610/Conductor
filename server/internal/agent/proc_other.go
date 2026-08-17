//go:build !windows

package agent

import (
	"errors"
	"log/slog"
	"os/exec"
	"syscall"
	"time"
)

// hideAgentWindow is a no-op on non-Windows platforms.
func hideAgentWindow(cmd *exec.Cmd) {}

// configureProcessGroup puts the child into its own process group (it
// becomes the group leader, so the group id equals the child pid). This
// lets us signal the entire tree — the agent CLI plus any tool subprocess
// it spawns — in one syscall, instead of killing only the direct child and
// leaking grandchildren that keep running after a task is cancelled.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// startOwnedProcessTree is a plain Start on non-Windows platforms:
// configureProcessGroup already put the child in its own process group
// before it existed, so there is nothing left to claim once it is running.
func startOwnedProcessTree(cmd *exec.Cmd, _ *slog.Logger) error { return cmd.Start() }

// releaseProcessGroup is a no-op on non-Windows: a process group needs no
// handle and is gone once its members are.
func releaseProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup sends sig to the whole process group led by cmd,
// falling back to the single process if the group send fails. Targeting
// the group (negative pid) reaches the descendants the agent spawned,
// not just the leader.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		_ = cmd.Process.Signal(sig)
	}
}

// waitProcessGroupGone returns true as soon as the group has zero members,
// or false if it is still alive after timeout.
//
// We poll with kill(0) because there is no portable "wait on group" API.
// The 10 ms cadence is plenty for our short grace window.
func waitProcessGroupGone(cmd *exec.Cmd, timeout time.Duration) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(-cmd.Process.Pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
