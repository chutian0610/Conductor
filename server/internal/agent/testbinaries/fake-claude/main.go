//go:build testbinaries

// fake-claude is the test fixture that stands in for the real Claude
// Code CLI. Build with `-tags testbinaries`; tests build it on
// demand and point Config.ExecutablePath at the resulting binary.
//
// USAGE
//
//	fake-claude --script=/path/to/scenario.jsonl [--exit=N]
//	fake-claude --block [--exit=N]
//	fake-claude --argv=/path/to/argv.txt --script=...
//
// See binrunner for the semantics.
package main

import (
	"conductor/server/internal/agent/testbinaries/binrunner"
)

func main() {
	binrunner.Run(binrunner.Config{
		Name:        "claude",
		ScriptFlag:  "script",
		ArgvFlag:    "argv",
		StdinFlag:   "stdin",
		BlockFlag:   "block",
		ExitFlag:    "exit",
		DefaultExit: 0,
	})
}
