// Command conductor is the CLI for the Conductor AI worker. It loads a
// conductor.yaml agent definition, instantiates the matching LLM backend
// from conductor/server/internal/backend, and runs the configured prompt.
//
// V1.x subcommands:
//
//	conductor run --config <agent.yaml> [--prompt "<extra>"] [--no-record]
//	  Run the agent once. Streams events to stderr; final result to stdout.
//	  By default the run is also recorded in the registry (auto-registers
//	  the agent on first sight); pass --no-record to opt out.
//
//	conductor agent list | show | register | update | archive | runs | events | run
//	  Manage the persistent agent layer (see package agentregistry).
//
// V2 will add: HTTP transport, DAG scheduler.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"conductor/server/internal/backend"
	"conductor/server/internal/agentregistry"
	"conductor/server/internal/ansiclean"
	"conductor/server/internal/configschema"

	"github.com/spf13/cobra"
)


// Char-budget used by renderMessage to clip long LLM tool
// outputs before they reach the operator's terminal. Lifted from
// the inline numeric literal so the value is visible at the call
// site and assertable in tests without rebuilding golden strings.
const toolOutputPreviewChars = 400
func main() {
	if err := root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "conductor:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conductor",
		Short: "Conductor AI worker — drive a Claude/Codex CLI as a subprocess",
		Long: `Conductor drives Claude Code / Codex CLI as a subprocess and
streams a uniform event stream back. V1.x adds the agent layer
(persistent registry, runs, events) under "conductor agent ...".`,
	}
	cmd.AddCommand(runCmd())
	cmd.AddCommand(agentCmd())
	cmd.AddCommand(auditCmd())
	return cmd
}

func runCmd() *cobra.Command {
	var (
		configPath  string
		extraPrompt string
		resumeID    string
		streamJSON  bool
		quiet       bool
		noRecord    bool
	)
	c := &cobra.Command{
		Use:   "run",
		Short: "Run the agent defined by --config",
		Long: `Load a conductor.yaml, spawn the matching LLM CLI, and stream events.

By default the run is recorded in the agent registry (auto-registering
the agent on first sight). Pass --no-record to opt out of both the
registry write and the auto-register.

Examples:
  conductor run --config examples/code-review-agent.yaml
  conductor run --config agent.yaml --prompt "Focus on the auth module"
  conductor run --config agent.yaml --no-record`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doRun(cmd.OutOrStdout(), cmd.OutOrStderr(),
				configPath, extraPrompt, resumeID, streamJSON, quiet, noRecord)
		},
	}
	c.Flags().StringVar(&configPath, "config", "conductor.yaml",
		"path to the conductor.yaml agent definition")
	c.Flags().StringVar(&extraPrompt, "prompt", "",
		"extra prompt appended to agent.prompt (CLI wins over YAML)")
	c.Flags().StringVar(&resumeID, "resume", "",
		"session id from a previous run to continue (empty = start fresh)")
	c.Flags().BoolVar(&streamJSON, "json", false,
		"emit one JSON object per line for events and the final result")
	c.Flags().BoolVar(&quiet, "quiet", false,
		"suppress human-readable event rendering; final result still printed")
	c.Flags().BoolVar(&noRecord, "no-record", false,
		"skip writing this run to the agent registry (and skip auto-register of new agents)")
	return c
}

// doRun is the V1 `conductor run` entry. When noRecord is false (the
// default), the run is recorded in the registry under an agent whose
// name matches schema.Agent.Name. If no such agent is registered
// yet, one is auto-created with Name + Backend + Description from
// the YAML. Subsequent `conductor run` calls reuse the existing row
// (operators revise metadata via `conductor agent update`).
func doRun(stdout, stderr io.Writer, configPath, extraPrompt, resumeID string, asJSON, quiet, noRecord bool) error {
	schema, err := configschema.Load(configPath)
	if err != nil {
		return err
	}
	prompt, err := schema.TaskPrompt(extraPrompt)
	if err != nil {
		return err
	}
	opts, err := schema.ToExecOptions(extraPrompt, resumeID)
	if err != nil {
		return err
	}

	reg, rec, regErr := openRecorderForRun(!noRecord, schema, prompt, "")
	if regErr != nil && !noRecord {
		// Registry failures are non-fatal: log to stderr and run
		// the agent anyway. Matches the best-effort stance
		// documented in agentregistry.
		fmt.Fprintf(stderr, "registry: %v (continuing without recording)\n", regErr)
		rec = nil
	}
	if reg != nil {
		defer reg.Close()
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	backend, err := backend.New(schema.Agent.Backend, backend.Config{Logger: logger})
	if err != nil {
		return err
	}
	return executeBackend(ctx, stdout, stderr, schema.Agent.Backend,
		backend, prompt, opts, rec, asJSON, quiet)
}

// openRecorderForRun resolves or auto-registers the YAML's agent and
// returns a ready-to-use recorder. The store handle ownership stays
// with the caller — they must Close() the returned *agentregistry.Store
// exactly once, regardless of rec being nil.
//
// On any failure we swallow the error and return (nil, err): the caller
// logs to stderr and proceeds without recording (mirrors multica's
// "registry is secondary" stance).
func openRecorderForRun(enabled bool, schema *configschema.Schema, prompt string, cwd string) (*agentregistry.Store, *runRecorder, error) {
	if !enabled {
		return nil, nil, nil
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	reg, err := agentregistry.Open(cwd)
	if err != nil {
		return nil, nil, fmt.Errorf("open registry: %w", err)
	}
	a, err := reg.EnsureAgent(context.Background(), agentregistry.Agent{
		Name:        schema.Agent.Name,
		Backend:     schema.Agent.Backend,
		Description: schema.Agent.Description,
	})
	if err != nil {
		_ = reg.Close()
		return nil, nil, fmt.Errorf("ensure agent %q: %w", schema.Agent.Name, err)
	}
	rec := newRunRecorder(reg, a.ID, prompt)
	if err := rec.start(context.Background()); err != nil {
		_ = reg.Close()
		return nil, nil, fmt.Errorf("start run: %w", err)
	}
	return reg, rec, nil
}

// renderMessage writes one event to w in a human-readable form. String
// fields are passed through ansiclean.Strip so any ANSI / CSI escapes
// the LLM emitted (or upstream parsers mangled) don't surface as
// literal `[1m`-style artifacts in the operator's terminal. The raw
// payload still lives in the registry's events table for replay.
func renderMessage(w io.Writer, msg backend.Message) {
	switch msg.Type {
	case backend.MessageText:
		fmt.Fprintln(w, "▸", ansiclean.Strip(msg.Content))
	case backend.MessageToolUse:
		fmt.Fprintf(w, "🔧 %s %v\n", msg.Tool, compactJSON(sanitizeMap(msg.Input)))
	case backend.MessageToolResult:
		fmt.Fprintln(w, "↳", truncate(ansiclean.Strip(msg.Output), toolOutputPreviewChars))
	case backend.MessageStatus:
		fmt.Fprintln(w, "·", ansiclean.Strip(msg.Status))
	case backend.MessageError:
		fmt.Fprintln(w, "⚠", ansiclean.Strip(msg.Content))
	case backend.MessageLog:
		fmt.Fprintf(w, "[%s] %s\n", ansiclean.Strip(msg.Level), ansiclean.Strip(msg.Content))
	}
}

// sanitizeMap strips ANSI from every string-valued leaf of a JSON
// map before it is printed via compactJSON. Keys are unchanged.
func sanitizeMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = ansiclean.Strip(t)
		default:
			out[k] = t
		}
	}
	return out
}

// renderResult writes the terminal outcome to stderr (status / error) and
// the final user-facing output to stdout. This split is the convention
// `conductor run` follows so users can `conductor run ... > out.md`.
func renderResult(stderr, stdout io.Writer, res backend.Result) {
	fmt.Fprintf(stderr, "\n— %s (%d ms) —\n", strings.ToUpper(res.Status), res.DurationMs)
	if res.SessionID != "" {
		fmt.Fprintf(stderr, "session: %s\n", res.SessionID)
	}
	if len(res.Usage) > 0 {
		emitUsage(stderr, res.Usage)
	}
	if res.Error != "" {
		fmt.Fprintln(stderr, "error:", res.Error)
	}
	if res.Output != "" {
		fmt.Fprintln(stdout, res.Output)
	}
}

func emitUsage(w io.Writer, usage map[string]backend.TokenUsage) {
	for model, u := range usage {
		// Strip removes ESC-prefixed ANSI from the model name.
		// It does NOT touch bracket-y content: Claude's variant
		// identifiers (`MiniMax-M3[1m]`) survive verbatim — see
		// the package comment in internal/ansiclean for the
		// rationale and ADR-0008 "Update log" item (e).
		fmt.Fprintf(w, "tokens[%s]: in=%d out=%d cache_r=%d cache_w=%d\n",
			ansiclean.Strip(model), u.InputTokens, u.OutputTokens,
			u.CacheReadTokens, u.CacheWriteTokens)
	}
}

// emitJSON writes one JSON object per line. Used by --json.
func emitJSON(w io.Writer, kind string, payload any) {
	out := map[string]any{"kind": kind, "payload": payload}
	data, _ := json.Marshal(out)
	fmt.Fprintln(w, string(data))
}

// compactJSON returns a single-line JSON rendering of v.
func compactJSON(v map[string]any) string {
	if len(v) == 0 {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(data)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// executeBackend drives one backend.Execute call, streaming events to
// stderr (or JSON) and forwarding every event to rec when rec is set.
// On success rec.finish() is called.
//
// This helper is shared between `conductor run` (main.go) and
// `conductor agent run <ref>` (agentcmd.go) so the two paths always
// emit the same wire format.
func executeBackend(
	ctx context.Context,
	stdout, stderr io.Writer,
	backendType string,
	backend backend.Backend,
	prompt string,
	opts backend.ExecOptions,
	rec *runRecorder,
	asJSON, quiet bool,
) error {
	session, err := backend.Execute(ctx, prompt, opts)
	if err != nil {
		return fmt.Errorf("execute %s: %w", backendType, err)
	}
	for msg := range session.Messages {
		if asJSON {
			emitJSON(stderr, "event", msg)
		} else if !quiet {
			renderMessage(stderr, msg)
		}
		if b, mErr := json.Marshal(msg); mErr == nil && rec != nil {
			rec.recordEvent(ctx, string(msg.Type), b)
		}
	}
	res := <-session.Result
	if asJSON {
		emitJSON(stderr, "result", res)
	} else {
		renderResult(stderr, stdout, res)
	}
	if rec != nil {
		rec.finish(ctx, res)
	}
	if res.Status != "completed" {
		return errors.New(res.Status + ": " + res.Error)
	}
	return nil
}
