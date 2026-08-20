// Package cli provides shared helpers for the conductor command-line
// interface. Each subcommand in server/cmd/conductor/ imports this
// package for flag parsing, output formatting, and stderr helpers.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// Stderr is the writer used for diagnostic output. Defaults to
// os.Stderr; tests can swap it via SetStderr.
var Stderr io.Writer = os.Stderr

// SetStderr swaps the writer used by Errorf / Warn. Intended for tests.
func SetStderr(w io.Writer) { Stderr = w }

// Stdout is the writer used for normal command output (success
// messages, table-formatted listings). Defaults to os.Stdout;
// tests can swap it via SetStdout.
var Stdout io.Writer = os.Stdout

// SetStdout swaps the writer used by Print / Println. Intended for tests.
func SetStdout(w io.Writer) { Stdout = w }

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

// Print writes to Stdout (no trailing newline added).
func Print(format string, args ...any) {
	fmt.Fprintf(Stdout, format, args...)
}

// Println writes to Stdout with a trailing newline.
func Println(format string, args ...any) {
	fmt.Fprintf(Stdout, format+"\n", args...)
}

// NewFlagSet returns a *flag.FlagSet with ContinueOnError behavior
// and output wired to cli.Stderr (not os.Stderr directly, so tests
// that swap cli.Stderr also capture usage output on --help / parse
// failures). Usage text is suppressed by default — subcommands
// should set fs.Usage to their own function so they control the
// formatting (and don't all print the same boilerplate).
func NewFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(Stderr)
	return fs
}

// ErrHelp is returned by ParseFlags when --help is seen. Callers
// should treat it as "user asked for help, no action needed".
var ErrHelp = errors.New("cli: help requested")

// ParseFlags wraps fs.Parse to:
//   - intercept --help and any flag.Parse error of flag.ErrHelp,
//     returning cli.ErrHelp so callers can do `errors.Is(err, cli.ErrHelp)`
//   - leave parse errors untouched otherwise (caller surfaces them)
func ParseFlags(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return ErrHelp
	}
	return err
}

// StringSliceFlag implements flag.Value for comma-separated lists
// (--tools-allow=bash,read). Empty entries are skipped. Resetting
// to "" clears the slice. The target slice is shared with the caller
// so reads after Parse see the final state.
type StringSliceFlag struct {
	Dest *[]string
}

// String implements flag.Value.
func (s StringSliceFlag) String() string {
	if s.Dest == nil {
		return ""
	}
	return strings.Join(*s.Dest, ",")
}

// Set implements flag.Value.
func (s StringSliceFlag) Set(v string) error {
	if s.Dest == nil {
		return nil
	}
	if v == "" {
		*s.Dest = nil
		return nil
	}
	parts := strings.Split(v, ",")
	out := (*s.Dest)[:0]
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	*s.Dest = out
	return nil
}
