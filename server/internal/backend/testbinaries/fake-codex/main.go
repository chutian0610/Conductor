//go:build testbinaries

// fake-codex is the test fixture that stands in for `codex exec --json`.
// Build with `-tags testbinaries`. The real codex CLI accepts the
// prompt as a trailing positional arg; fake-codex ignores it (the
// runner reads it via flag.Args() and records it via --argv).
//
// USAGE
//
//	fake-codex --script=/path/to/scenario.jsonl [--exit=N]
//	fake-codex --block [--exit=N]
//	fake-codex --argv=/path/to/argv.txt --script=... "the prompt"
package main

import (
	"conductor/server/internal/backend/testbinaries/binrunner"
)

func main() {
	binrunner.Run(binrunner.Config{
		Name:        "codex",
		ScriptFlag:  "script",
		ArgvFlag:    "argv",
		StdinFlag:   "stdin",
		BlockFlag:   "block",
		ExitFlag:    "exit",
		DefaultExit: 0,
	})
}
