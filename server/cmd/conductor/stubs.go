// Stubs for subcommand entry points. Phase 1 fills these in incrementally.
//
// Each subcommand lives in its own file once it grows beyond trivial:
//   server/cmd/conductor/daemon.go
//   server/cmd/conductor/spec.go
//   server/cmd/conductor/run.go
//   server/cmd/conductor/ls.go
//   server/cmd/conductor/cancel.go
//
// For now they all return ErrNotImplemented so the binary compiles and
// reports which subcommand still needs wiring.
package main

import (
	"context"
	"errors"

	"conductor/server/internal/cli"
)

// ErrNotImplemented marks a subcommand as a Phase 1 TODO. Returned from
// every stub until that subcommand lands in its own file.
var ErrNotImplemented = errors.New("not implemented yet")

func runDaemon(_ context.Context, _ []string) error {
	cli.Errorf("conductor daemon: %s", ErrNotImplemented)
	return ErrNotImplemented
}

func runLs(_ context.Context, _ []string) error {
	cli.Errorf("conductor ls: %s", ErrNotImplemented)
	return ErrNotImplemented
}

func runCancel(_ context.Context, _ []string) error {
	cli.Errorf("conductor cancel: %s", ErrNotImplemented)
	return ErrNotImplemented
}
