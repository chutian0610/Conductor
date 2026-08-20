// Conductor daemon + CLI entry point.
//
// Subcommands:
//   conductor daemon         Start the local Player Daemon (foreground)
//   conductor daemon --hub   Start the Player Hub (Phase 2+)
//   conductor run            Run a workflow / invoke a spec
//   conductor spec            Manage specs (create / list / show / rm)
//   conductor ls              List active agents / runs
//   conductor logs            Show logs of a run / agent
//   conductor cancel          Cancel a run
//   conductor --version       Print version
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

// Version is set at build time via -ldflags "-X main.Version=...".
var Version = "0.1.0-dev"

func main() {
	// Root-context with signal-aware cancellation. Daemon subcommands
	// derive their long-running contexts from this; CLI subcommands ignore
	// it (they run to completion synchronously).
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if !errors.Is(err, context.Canceled) {
			// Subcommands that produced user-facing output (via cli.Errorf)
			// should return an error without re-printing. Subcommands that
			// didn't print anything should still surface a generic error.
			// We rely on the convention that returning an error means
			// "something already told the user"; main does NOT re-print.
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "--version", "-v":
		os.Stdout.WriteString("conductor " + Version + "\n")
		return nil
	case "--help", "-h", "help":
		printUsage()
		return nil
	}

	// Subcommand dispatch. Each subcommand is implemented in its own
	// sub-package so this file stays small. Phase 1 wires up `daemon`,
	// `run`, `ls`, `cancel`, and `spec`; `hub` is Phase 2+.
	switch args[0] {
	case "daemon":
		return runDaemon(ctx, args[1:])
	case "spec":
		return runSpec(ctx, args[1:])
	case "run":
		return runRun(ctx, args[1:])
	case "ls":
		return runLs(ctx, args[1:])
	case "cancel":
		return runCancel(ctx, args[1:])
	default:
		printUsage()
		return nil
	}
}

func printUsage() {
	os.Stdout.WriteString(`conductor — local multi-agent orchestrator

Usage:
  conductor daemon         start the Player Daemon
  conductor spec <action>  manage agent specs (create/list/show/rm)
  conductor run [flags]    invoke a spec or run a workflow
  conductor ls             list active runs / agents
  conductor cancel <id>    cancel a run
  conductor --version      print version

Run 'conductor <subcommand> --help' for subcommand-specific flags.
`)
}
