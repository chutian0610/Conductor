// Subcommand: conductor run
//
//	conductor run --spec <id> [prompt]
//	conductor run --spec <id> --resume <sessionId> [prompt]
//	conductor run --spec <id> --from-run <runId> [prompt]
//
// One-shot invocation: opens a codex Session against the spec's
// per-spec HOME, sends the prompt, streams events to stdout, and
// prints the final AgentTurnResult.
//
// Phase 1 doesn't yet persist timeline or run state; subsequent
// runs against the same spec start a fresh thread (unless --resume
// is given). Phase 2 will add runs/<runId>/ state directories.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"conductor/server/internal/cli"
	"conductor/server/internal/protocol"
	"conductor/server/internal/storage"
	"conductor/server/internal/runner"
)

func runRun(ctx context.Context, args []string) error {
	return runRunWithWriter(ctx, args, cli.Stdout, cli.Stderr)
}

// runRunWithWriter is the testable form: takes explicit io.Writers
// instead of relying on the global cli.Stdout / cli.Stderr.
func runRunWithWriter(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := cli.NewFlagSet("conductor run")
	f := &runFlags{}
	fs.StringVar(&f.Spec, "spec", "", "spec id to invoke")
	fs.StringVar(&f.Resume, "resume", "", "session id to resume (raw threadId; routes to thread/resume)")
	fs.StringVar(&f.FromRun, "from-run", "", "runId to resume (looks up the prior run's sessionId from storage)")
	fs.BoolVar(&f.JSON, "json", false, "stream events as JSON lines (machine-readable)")
	fs.BoolVar(&f.Quiet, "quiet", false, "suppress event streaming; only print final result")

	fs.Usage = func() {
		fmt.Fprint(stderr, `conductor run — invoke a spec with a prompt

Usage:
  conductor run --spec <id> [prompt]
  conductor run --spec <id> --resume <sessionId> [prompt]
  conductor run --spec <id> --from-run <runId> [prompt]

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
	if f.Spec == "" {
		fmt.Fprintln(stderr, "error: --spec is required")
		return errors.New("--spec required")
	}

	// Positional args: the prompt. We accept the prompt as a
	// single trailing string (joined with spaces if shell-split).
	prompt := ""
	if fs.NArg() > 0 {
		prompt = fs.Arg(0)
	}
	if f.Resume != "" && f.FromRun != "" {
		fmt.Fprintln(stderr, "error: --resume and --from-run are mutually exclusive")
		return errors.New("--resume and --from-run are mutually exclusive")
	}
	if prompt == "" && f.Resume == "" && f.FromRun == "" {
		fmt.Fprintln(stderr, "error: prompt argument required (or use --resume / --from-run to continue an existing session)")
		return errors.New("prompt required")
	}

	handler := buildEventPrinter(stdout, f.JSON, f.Quiet)

	// Watch for SIGTERM/SIGINT so `conductor cancel <runId>` can
	// ask us to stop gracefully. The signal fires once and the
	// runner transitions the run to status=cancelled.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	fmt.Fprintln(os.Stderr, "[run.go] pid=", os.Getpid(), " PATH=", os.Getenv("PATH"))

	// Open storage. Phase 1 ships JsonFileStorage; future SqliteStorage
	// drops in transparently via the Storage interface.
	store, err := storage.NewJsonFileStorage()
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return err
	}
	runID := storage.NewRunID()

	// If --from-run <runId> was given, translate it to a
	// sessionId first (looked up from the prior run's state.json).
	sessionID := f.Resume
	if f.FromRun != "" {
		sid, lookupErr := store.LookupSessionID(ctx, f.FromRun)
		if lookupErr != nil {
			fmt.Fprintf(stderr, "error: --from-run %s: %s\n", f.FromRun, lookupErr)
			return lookupErr
		}
		sessionID = sid
		fmt.Fprintf(stdout, "resuming session %s (from run %s)\n", sid, f.FromRun)
	}

	var result *protocol.AgentTurnResult
	if sessionID != "" {
		result, err = runner.InvokeWithSessionId(ctx, f.Spec, sessionID, prompt, runID, store, handler, sigCh)
	} else {
		result, err = runner.Invoke(ctx, f.Spec, prompt, runID, store, handler, sigCh)
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return err
	}

	// Print the final result summary unless --quiet.
	// --quiet suppresses event streaming only; the final summary
	// is always printed (otherwise there is no way to know the run
	// finished or what the result was).
	printRunSummary(stdout, result)
	if result != nil && result.SessionID != "" {
		fmt.Fprintf(stdout, "runId:     %s\n", runID)
		fmt.Fprintf(stdout, "sessionId: %s\n", result.SessionID)
	}
	return nil
}

type runFlags struct {
	Spec      string
	Resume    string // raw threadId (advanced / copy-paste from logs)
	FromRun string // runId (typical UX — references a prior run)
	JSON      bool
	Quiet     bool
}

// buildEventPrinter returns an EventHandler that formats events
// as human-readable text, JSON lines, or nil (for --quiet).
//
// The returned handler never blocks — it writes to w directly
// (no internal channel) so the runner's pump goroutine can't
// backpressure.
func buildEventPrinter(w io.Writer, asJSON, quiet bool) runner.EventHandler {
	if quiet {
		// Drain events silently.
		return func(ev protocol.AgentStreamEvent) {}
	}
	if asJSON {
		return func(ev protocol.AgentStreamEvent) {
			data, err := json.Marshal(ev)
			if err != nil {
				return
			}
			fmt.Fprintln(w, string(data))
		}
	}
	return func(ev protocol.AgentStreamEvent) {
		switch ev.Kind {
		case protocol.EventText:
			fmt.Fprintf(w, "%s", ev.Text)
		case protocol.EventToolCall:
			args := ""
			if len(ev.ToolArgs) > 0 {
				b, _ := json.Marshal(ev.ToolArgs)
				args = string(b)
			}
			fmt.Fprintf(w, "\n→ %s(%s)", ev.ToolName, args)
		case protocol.EventToolResult:
			if ev.ToolError != "" {
				fmt.Fprintf(w, "\n← %s [error: %s]", ev.ToolResult, ev.ToolError)
			} else {
				fmt.Fprintf(w, "\n← %s", ev.ToolResult)
			}
		case protocol.EventPermission:
			fmt.Fprintln(w, "\n[permission request — auto-approved in Phase 1]")
		case protocol.EventFinish:
			// Don't print; the summary block covers finish.
		case protocol.EventError:
			fmt.Fprintf(w, "\nERROR [%s]: %s", ev.ErrorCode, ev.ErrorMessage)
		default:
			// Unknown kind — drop silently. Logger hook in Phase 2.
		}
	}
}

// printRunSummary writes the final AgentTurnResult block. Format:
//
//	--- result ---
//	session:  thr-xxx
//	usage:    10 input / 5 output tokens (~$0.0012)
//	finish:   end_turn (success)
func printRunSummary(w io.Writer, r *protocol.AgentTurnResult) {
	if r == nil {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "--- result ---")
	if r.SessionID != "" {
		fmt.Fprintf(w, "session:  %s\n", r.SessionID)
	}
	fmt.Fprintf(w, "usage:    %d input / %d output tokens",
		r.Usage.InputTokens, r.Usage.OutputTokens)
	if r.Usage.CostUSD > 0 {
		fmt.Fprintf(w, " (~$%.4f)", r.Usage.CostUSD)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "finish:   %s", r.Finish.Reason)
	if r.Finish.Success {
		fmt.Fprint(w, " (success)")
	} else {
		fmt.Fprint(w, " (failed)")
	}
	fmt.Fprintln(w)
}
