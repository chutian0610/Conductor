// Package cli provides shared helpers for the conductor command-line
// interface. Each subcommand in server/cmd/conductor/ imports this
// package for flag parsing, output formatting, and stderr helpers.
package cli

import (
	"fmt"
	"io"
	"os"
)

// Stderr is the writer used for diagnostic output. Defaults to
// os.Stderr; tests can swap it via SetStderr.
var Stderr io.Writer = os.Stderr

// SetStderr swaps the writer used by Errorf / Warn. Intended for tests.
func SetStderr(w io.Writer) { Stderr = w }

// Errorf writes "error: <msg>\n" to Stderr. Used by subcommands that
// want to surface a fatal error. The caller in main() detects the
// returned error and exits non-zero, so Errorf already carries the
// "error: " prefix to avoid double-printing.
func Errorf(format string, args ...any) {
	fmt.Fprintf(Stderr, "error: "+format+"\n", args...)
}

// Warn writes "warn: <msg>\n" to Stderr.
func Warn(format string, args ...any) {
	fmt.Fprintf(Stderr, "warn: "+format+"\n", args...)
}
