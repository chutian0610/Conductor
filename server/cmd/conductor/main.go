// Command conductor is the CLI for the Conductor AI worker. It loads a
// conductor.yaml agent definition, instantiates the matching LLM backend
// from conductor/server/internal/agent, and runs the configured prompt.
//
// V1 subcommands:
//
//	conductor run --config <agent.yaml> [--prompt "<extra>"]
//	  Run the agent once. Streams events to stderr; final result to stdout.
//
// V2 will add: conductor plan / conductor attach / conductor status.
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

	"conductor/server/internal/agent"
	"conductor/server/internal/configschema"

	"github.com/spf13/cobra"
)

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
	}
	cmd.AddCommand(runCmd())
	return cmd
}

func runCmd() *cobra.Command {
	var (
		configPath  string
		extraPrompt string
		resumeID    string
		streamJSON  bool
		quiet       bool
	)
	c := &cobra.Command{
		Use:   "run",
		Short: "Run the agent defined by --config",
		Long: `Load a conductor.yaml, spawn the matching LLM CLI, and stream events.

Examples:
  conductor run --config examples/code-review-agent.yaml
  conductor run --config agent.yaml --prompt "Focus on the auth module"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doRun(cmd.OutOrStdout(), cmd.ErrOrStderr(),
				configPath, extraPrompt, resumeID, streamJSON, quiet)
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
	return c
}

func doRun(stdout, stderr io.Writer, configPath, extraPrompt, resumeID string, asJSON, quiet bool) error {
	schema, err := configschema.Load(configPath)
	if err != nil {
		return err
	}
	// Build the user-visible prompt (brief + skills + --prompt) for the
	// CLI's stdin. ToExecOptions fills opts.SystemPrompt with the brief
	// for the backend to persist to CLAUDE.md/AGENTS.md; the prompt
	// argument here is what the LLM actually sees as its turn input.
	prompt, err := schema.TaskPrompt(extraPrompt)
	if err != nil {
		return err
	}
	opts, err := schema.ToExecOptions(extraPrompt, resumeID)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	backend, err := agent.New(schema.Agent.Backend, agent.Config{
		Logger: logger,
	})
	if err != nil {
		return err
	}

	// Honour SIGINT/SIGTERM by cancelling the agent's context. The
	// backend's process-group machinery turns the cancellation into a
	// graceful SIGTERM→SIGKILL on the spawned CLI + its descendants.
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	session, err := backend.Execute(ctx, prompt, opts)
	if err != nil {
		return fmt.Errorf("execute %s: %w", schema.Agent.Backend, err)
	}

	// Drain events.
	for msg := range session.Messages {
		if asJSON {
			emitJSON(stderr, "event", msg)
		} else if !quiet {
			renderMessage(stderr, msg)
		}
	}
	// Drain result.
	res := <-session.Result
	if asJSON {
		emitJSON(stderr, "result", res)
	} else {
		renderResult(stderr, stdout, res)
	}
	if res.Status != "completed" {
		return errors.New(res.Status + ": " + res.Error)
	}
	return nil
}

// renderMessage writes one event to w in a human-readable form.
func renderMessage(w io.Writer, msg agent.Message) {
	switch msg.Type {
	case agent.MessageText:
		fmt.Fprintln(w, "▸", msg.Content)
	case agent.MessageThinking:
		fmt.Fprintln(w, "…", truncate(msg.Content, 200))
	case agent.MessageToolUse:
		fmt.Fprintf(w, "🔧 %s %v\n", msg.Tool, compactJSON(msg.Input))
	case agent.MessageToolResult:
		fmt.Fprintln(w, "↳", truncate(msg.Output, 400))
	case agent.MessageStatus:
		fmt.Fprintln(w, "·", msg.Status)
	case agent.MessageError:
		fmt.Fprintln(w, "⚠", msg.Content)
	case agent.MessageLog:
		fmt.Fprintf(w, "[%s] %s\n", msg.Level, msg.Content)
	}
}

// renderResult writes the terminal outcome to stderr (status / error) and
// the final user-facing output to stdout. This split is the convention
// `conductor run` follows so users can `conductor run ... > out.md`.
func renderResult(stderr, stdout io.Writer, res agent.Result) {
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

func emitUsage(w io.Writer, usage map[string]agent.TokenUsage) {
	for model, u := range usage {
		fmt.Fprintf(w, "tokens[%s]: in=%d out=%d cache_r=%d cache_w=%d\n",
			model, u.InputTokens, u.OutputTokens,
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
