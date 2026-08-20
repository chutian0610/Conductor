// Subcommand: conductor cancel
//
//	conductor cancel <runId>             # graceful: SIGTERM, wait for status flip
//	conductor cancel --force <runId>     # SIGKILL after 2s grace; status stays running
//
// Sends a signal to the host process that owns the given runId, so
// its signal handler can transition the run to status=cancelled
// (and ask codex to stop via turn/interrupt). The CLI polls
// state.json for the status flip with a 5s grace window.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"conductor/server/internal/cli"
	"conductor/server/internal/storage"
)

const (
	cancelGracePeriod  = 5 * time.Second
	cancelForceGrace    = 2 * time.Second
	cancelPollInterval  = 200 * time.Millisecond
)

func runCancel(ctx context.Context, args []string) error {
	return runCancelWithWriter(ctx, args, cli.Stdout, cli.Stderr)
}

func runCancelWithWriter(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := cli.NewFlagSet("conductor cancel")
	f := &cancelFlags{}
	fs.BoolVar(&f.Force, "force", false, "SIGKILL after a 2s grace if SIGTERM didn't flip the status")

	fs.Usage = func() {
		fmt.Fprint(stderr, `conductor cancel <runId> — cancel an in-progress run

Usage:
  conductor cancel [--force] <runId>

Sends SIGTERM to the host process that owns the run (identified
by state.json's pid field). The runner's signal handler transitions
the run to status=cancelled; we poll state.json for the flip.

If --force is set, after a 2s grace we SIGKILL the process and warn
that state.json will remain at status=running until the next prune.

Flags:
`)
		fs.PrintDefaults()
	}

	if err := cli.ParseFlags(fs, args); err != nil {
		if errors.Is(err, cli.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "error: usage: conductor cancel [--force] <runId>\n")
		return errors.New("runId required")
	}
	runID := fs.Arg(0)

	store, err := storage.NewJsonFileStorage()
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return err
	}
	state, err := store.GetRun(ctx, runID)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return err
	}
	if state.Status != storage.RunStatusRunning {
		fmt.Fprintf(stdout, "run %s is %s; nothing to cancel\n", runID, state.Status)
		return nil
	}
	if state.PID == 0 {
		fmt.Fprintf(stderr, "error: run %s has no recorded PID (older run; cancel by signal not supported)\n", runID)
		return errors.New("no PID recorded")
	}
	process, err := os.FindProcess(state.PID)
	if err != nil {
		fmt.Fprintf(stderr, "error: find process %d: %s\n", state.PID, err)
		return err
	}

	fmt.Fprintf(stdout, "sending SIGTERM to pid %d (run %s)\n", state.PID, runID)
	if err := process.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(stderr, "error: signal: %s\n", err)
		return err
	}

	// Poll for status to flip away from running.
	deadline := time.Now().Add(cancelGracePeriod)
	for time.Now().Before(deadline) {
		state, err := store.GetRun(ctx, runID)
		if err != nil {
			fmt.Fprintf(stderr, "error: poll: %s\n", err)
			return err
		}
		if state.Status != storage.RunStatusRunning {
			fmt.Fprintf(stdout, "run %s is now %s\n", runID, state.Status)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cancelPollInterval):
		}
	}

	// Grace expired; either report or force-kill.
	if !f.Force {
		fmt.Fprintf(stderr, "error: pid %d did not transition run %s to a non-running status within %s; rerun with --force to SIGKILL\n",
			state.PID, runID, cancelGracePeriod)
		return errors.New("cancel grace period expired")
	}

	fmt.Fprintf(stdout, "grace expired; sending SIGKILL to pid %d\n", state.PID)
	if err := process.Signal(syscall.SIGKILL); err != nil {
		fmt.Fprintf(stderr, "warn: sigkill: %s (process may have already exited)\n", err)
	}
	// Give the OS a moment to reap, then warn that state is now stale.
	time.Sleep(cancelForceGrace)
	state, _ = store.GetRun(ctx, runID)
	if state.Status == storage.RunStatusRunning {
		fmt.Fprintf(stderr, "warn: run %s status is still 'running' (state.json was not updated before SIGKILL). Run `conductor rm %s` to clean up.\n", runID, runID)
	}
	return nil
}

type cancelFlags struct {
	Force bool
}
