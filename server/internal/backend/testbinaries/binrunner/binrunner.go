// Package binrunner is the test-fixture CLI runner used by both
// fake-claude and fake-codex. Each fake main.go is a thin wrapper that
// calls Run() with its own Config — the shared behaviour is:
//
//   - read a JSONL "script" of {delay_ms, event} steps and emit each
//     event to stdout, one per line (line-delimited JSON, matching
//     what the real Claude / Codex CLIs send)
//   - record argv so tests can assert on what conductor built
//   - record stdin so tests can assert on what conductor wrote (Claude
//     sends its prompt as stdin JSON; we mirror that)
//   - block until killed when --block is set (for cancellation tests)
//   - exit with --exit=<N> when the script ends (default 0)
//
// This package is imported only by build-tagged fake mains; "go build
// ./cmd/conductor" never sees it.
package binrunner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Config names the flags for a particular fake. All flags are optional.
// Different fakes use different flag names so the test can grep argv
// unambiguously.
type Config struct {
	Name        string // "claude" or "codex", used only in log lines
	ScriptFlag  string // e.g. "script"
	ArgvFlag    string // e.g. "argv" — write argv to this path
	StdinFlag   string // e.g. "stdin" — copy stdin to this path
	BlockFlag   string // e.g. "block" — ignore script and block forever
	ExitFlag    string // e.g. "exit" — exit code on completion (default 0)
	DefaultExit int    // usually 0
}

// ScriptStep is one row in the JSONL scenario file. DelayMs is the
// wait before emitting the step; 0 means immediate. Event is the JSON
// object to write to stdout (followed by '\n').
type ScriptStep struct {
	DelayMs int             `json:"delay_ms,omitempty"`
	Event   json.RawMessage `json:"event"`
}

// flagTarget describes one named flag the fake binary understands. The
// value pointer points at the variable that ends up holding the parsed
// value (or true for a no-arg boolean). takesValue tells scanFlags
// whether the flag needs a following argv element.
type flagTarget struct {
	value      any // *string, *bool, or *int
	takesValue bool
}

// Run is the fake binary's main loop. It never returns; callers exit
// via the OS calls below.
func Run(cfg Config) {
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "fake-"+cfg.Name+": "+format+"\n", a...)
	}

	var scriptPath string
	var argvPath string
	var stdinPath string
	var block bool
	var exitCode = cfg.DefaultExit

	// drainDone tracks the stdin-drain goroutine. We wait on it
	// before calling [os.Exit] at the end of [Run] so buffered
	// writes hit the inode: [os.Exit] skips goroutine defers, so an
	// unflushed [io.Copy] in drainStdin would lose its tail bytes.
	// Without this, integration tests that read the captured stdin
	// (e.g. TestClaudeIntegration_ControlRequest_AllowsAndForcesForeground)
	// were flaky on Linux: macOS pipe scheduling lets the drain
	// finish before exit, Linux does not.
	var drainDone sync.WaitGroup

	flags := map[string]*flagTarget{
		cfg.ScriptFlag: {value: &scriptPath, takesValue: true},
		cfg.ArgvFlag:   {value: &argvPath, takesValue: true},
		cfg.StdinFlag:  {value: &stdinPath, takesValue: true},
		cfg.BlockFlag:  {value: &block, takesValue: false},
		cfg.ExitFlag:   {value: &exitCode, takesValue: true},
	}
	scanFlags(os.Args[1:], flags)
	fmt.Fprintf(os.Stderr, "fake-`${cfg.Name}` parsed: script=%q argv=%q stdin=%q block=%v exit=%d\n", scriptPath, argvPath, stdinPath, block, exitCode)
	fmt.Fprintf(os.Stderr, "fake-`${cfg.Name}` full argv: %v\n", os.Args[1:])

	if argvPath != "" {
		if err := writeArgvJSONL(argvPath, os.Args[1:]); err != nil {
			logf("write argv: %v", err)
			os.Exit(2)
		}
	}
	if stdinPath != "" {
		// Drain stdin in the background so the parent (which writes
		// the prompt and may close stdin once written) does not block.
		drainDone.Add(1)
		go func() {
			defer drainDone.Done()
			if err := drainStdin(stdinPath); err != nil && !errors.Is(err, io.EOF) {
				logf("read stdin: %v", err)
			}
		}()
	}

	if block {
		// Block forever. SIGTERM (from the controller's process-group
		// cancel) and SIGKILL (after the 5 s grace) both reap us.
		select {}
	}
	if scriptPath == "" {
		os.Exit(exitCode)
	}
	if err := runScript(scriptPath); err != nil {
		logf("run script: %v", err)
		os.Exit(1)
	}
	// Ensure drainStdin has finished and its defer [f.Close] has
	// run before exit. See the drainDone declaration above for
	// the race that motivated this wait.
	drainDone.Wait()
	os.Exit(exitCode)
}

// scanFlags walks argv looking for any of the named flags in either
// "-name=value" / "--name=value" or "-name value" / "--name value"
// form. Unrecognised args (the entire production CLI's flag set:
// "-p", "--output-format stream-json", ...) are silently skipped.
func scanFlags(argv []string, flags map[string]*flagTarget) {
	type entry struct {
		name   string
		target *flagTarget
	}
	var names []entry
	for n, t := range flags {
		names = append(names, entry{n, t})
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		for _, e := range names {
			t := e.target
			name := e.name
			for _, dash := range []string{"--", "-"} {
				key := dash + name
				if v, ok := strings.CutPrefix(a, key+"="); ok {
					setFlag(t, v)
					goto consumed
				}
				if a == key {
					if !t.takesValue {
						setFlag(t, "true")
						goto consumed
					}
					if i+1 < len(argv) {
						setFlag(t, argv[i+1])
						i++
						goto consumed
					}
					// flag with required value but no value follows;
					// leave zero, drop through.
					goto consumed
				}
			}
		}
		// unmatched argv element — silently ignore
	consumed:
	}
}

func setFlag(t *flagTarget, raw string) {
	switch p := t.value.(type) {
	case *string:
		*p = raw
	case *bool:
		*p = (raw == "true" || raw == "1")
	case *int:
		var n int
		_, _ = fmt.Sscanf(raw, "%d", &n)
		*p = n
	}
}

func writeArgvJSONL(path string, argv []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, a := range argv {
		if err := enc.Encode(a); err != nil {
			return err
		}
	}
	return nil
}

func drainStdin(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, os.Stdin)
	return err
}

func runScript(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Large event tool outputs can be 100s of KiB; cap at 16 MiB to
	// match the production agent stream scanner.
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	bw := bufio.NewWriter(os.Stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var step ScriptStep
		if err := json.Unmarshal([]byte(line), &step); err != nil {
			return fmt.Errorf("parse script line: %w", err)
		}
		if step.DelayMs > 0 {
			time.Sleep(time.Duration(step.DelayMs) * time.Millisecond)
		}
		if len(step.Event) > 0 {
			if _, err := bw.Write(step.Event); err != nil {
				return err
			}
			if err := bw.WriteByte('\n'); err != nil {
				return err
			}
			// Flush after each event so tests reading stdout via the
			// parent's scanner don't have to wait for buffer fill.
			if err := bw.Flush(); err != nil {
				return err
			}
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	return sc.Err()
}
