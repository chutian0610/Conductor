package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"conductor/server/internal/agent"
	"conductor/server/internal/agentregistry"
	"conductor/server/internal/configschema"

	"github.com/spf13/cobra"
)

// conductor-agent subcommand tree. The CLI is the only `conductor`
// surface today; multica exposes the same operations as a Python CLI
// (`goal_manager.py set | list | complete | ...`), and Conductor mirrors
// that one-to-one at `conductor agent ...`.
//
// Subcommands:
//
//	conductor agent list      [--backend <type>] [--all]
//	conductor agent show      <ref>
//	conductor agent register  <name> --backend <type> [--parent <ref>] [--description <text>]
//	conductor agent update    <ref> [--backend <type>] [--description <text>] [--parent <ref>|--clear-parent]
//	conductor agent archive   <ref>
//	conductor agent runs      <ref> [--status <status>] [--limit N]
//	conductor agent events    <run-id>
//	conductor agent run       <ref> --config <agent.yaml> [--prompt "<extra>"] [--resume <sid>]
//
// Each subcommand resolves the registry file from the user's CWD; pass
// `--registry <dir>` to point at an alternate cwd (used by tests).

func agentCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "agent",
		Short: "Manage the agent layer (persistent registry, runs, events, audit)",
		Long: `The "agent layer" sits above the backend driver (claude/codex). It
keeps a persistent catalog of registered agents, records every run with
its event stream, and stitches parent/child relationships across
subagent spawns via CONDUCTOR_* environment variables.

The registry lives at <cwd>/.conductor/registry.db.`,
	}
	c.PersistentFlags().String("registry", "",
		"override registry cwd (default: $PWD; mostly for tests)")
	c.AddCommand(
		agentListCmd(), agentShowCmd(), agentRegisterCmd(),
		agentUpdateCmd(), agentArchiveCmd(),
		agentRunsCmd(), agentEventsCmd(),
		agentRunCmd(),
	)
	return c
}

// withStore opens the registry rooted at the effective cwd
// (`--registry` flag > $PWD > ".").
func withStore(cmd *cobra.Command) (*agentregistry.Store, error) {
	dir, _ := cmd.Flags().GetString("registry")
	if dir == "" {
		dir, _ = os.Getwd()
	}
	return agentregistry.Open(dir)
}

// --- list ----------------------------------------------------------------

func agentListCmd() *cobra.Command {
	var (
		backend string
		all     bool
		asJSON  bool
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List registered agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := withStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			items, err := st.ListAgents(cmd.Context(), agentregistry.ListAgentOpts{
				Backend: backend, IncludeArchived: all,
			})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), items)
			}
			if len(items) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "(no agents)")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tBACKEND\tPARENT\tUPDATED")
			for _, a := range items {
				parent := "-"
				if a.ParentID > 0 {
					parent = fmt.Sprintf("@%d", a.ParentID)
				}
				updated := a.UpdatedAt.Format(time.RFC3339)
				if a.ArchivedAt != nil {
					updated = "archived " + a.ArchivedAt.Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
					a.ID, a.Name, a.Backend, parent, updated)
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&backend, "backend", "", "filter by backend (claude|codex)")
	c.Flags().BoolVar(&all, "all", false, "include archived agents")
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of a table")
	return c
}

// --- show ----------------------------------------------------------------

func agentShowCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "show <ref>",
		Short: "Show one registered agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := withStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			a, err := st.GetAgent(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), a)
			}
			parent := "(none)"
			if a.ParentID > 0 {
				parent = fmt.Sprintf("@%d", a.ParentID)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"%s  (%s)  id=%d  parent=%s\n  created: %s\n  updated: %s\n",
				a.Name, a.Backend, a.ID, parent,
				a.CreatedAt.Format(time.RFC3339), a.UpdatedAt.Format(time.RFC3339),
			)
			if a.Description != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  description: %s\n", a.Description)
			}
			if a.ArchivedAt != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "  archived: %s\n", a.ArchivedAt.Format(time.RFC3339))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of human-readable text")
	return c
}

// --- register ------------------------------------------------------------

func agentRegisterCmd() *cobra.Command {
	var (
		backend     string
		description string
		parentRef   string
	)
	c := &cobra.Command{
		Use:   "register <name>",
		Short: "Register a new agent in the registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if backend == "" {
				return fmt.Errorf("--backend is required")
			}
			st, err := withStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			a := agentregistry.Agent{
				Name:        args[0],
				Backend:     backend,
				Description: description,
			}
			if parentRef != "" {
				parent, err := st.GetAgent(cmd.Context(), parentRef)
				if err != nil {
					return fmt.Errorf("parent: %w", err)
				}
				a.ParentID = parent.ID
			}
			id, err := st.RegisterAgent(cmd.Context(), a)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "registered @%d  %s  backend=%s\n",
				id, a.Name, a.Backend)
			return nil
		},
	}
	c.Flags().StringVar(&backend, "backend", "",
		"agent backend (claude|codex) — required")
	c.Flags().StringVar(&description, "description", "",
		"optional human description")
	c.Flags().StringVar(&parentRef, "parent", "",
		"parent agent ref (name or @id)")
	return c
}

// --- update --------------------------------------------------------------

func agentUpdateCmd() *cobra.Command {
	var (
		backend     string
		description string
		parentRef   string
		clearParent bool
	)
	c := &cobra.Command{
		Use:   "update <ref>",
		Short: "Update an existing agent (backend / description / parent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := withStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			a, err := st.GetAgent(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			patch := agentregistry.AgentPatch{}
			if cmd.Flags().Changed("backend") {
				patch.Backend = &backend
			}
			if cmd.Flags().Changed("description") {
				patch.Description = &description
			}
			if clearParent {
				patch.ClearParent = true
			} else if parentRef != "" {
				parent, err := st.GetAgent(cmd.Context(), parentRef)
				if err != nil {
					return fmt.Errorf("parent: %w", err)
				}
				patch.ParentID = &parent.ID
			}
			if err := st.UpdateAgent(cmd.Context(), a.ID, patch); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "updated @%d  %s\n", a.ID, a.Name)
			return nil
		},
	}
	c.Flags().StringVar(&backend, "backend", "",
		"new backend (claude|codex)")
	c.Flags().StringVar(&description, "description", "",
		"new description (empty string clears)")
	c.Flags().StringVar(&parentRef, "parent", "",
		"new parent agent ref (name or @id)")
	c.Flags().BoolVar(&clearParent, "clear-parent", false,
		"remove the parent link")
	return c
}

// --- archive -------------------------------------------------------------

func agentArchiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archive <ref>",
		Short: "Soft-delete an agent (historical Runs are preserved)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := withStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			a, err := st.GetAgent(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if err := st.ArchiveAgent(cmd.Context(), a.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "archived @%d  %s\n", a.ID, a.Name)
			return nil
		},
	}
}

// --- runs / events -------------------------------------------------------

func agentRunsCmd() *cobra.Command {
	var (
		status string
		limit  int
		asJSON bool
	)
	c := &cobra.Command{
		Use:   "runs [agent-ref]",
		Short: "List runs for an agent (or all agents if ref is omitted)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			st, err := withStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			items, err := st.ListRuns(cmd.Context(), ref,
				agentregistry.ListRunOpts{Status: status, Limit: limit})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), items)
			}
			if len(items) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "(no runs)")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tAGENT\tSTATUS\tSESSION\tDUR(ms)\tSTARTED")
			for _, r := range items {
				fmt.Fprintf(tw, "%d\t@%d\t%s\t%s\t%d\t%s\n",
					r.ID, r.AgentID, r.Status,
					truncDot(r.SessionID, 16), r.DurationMs,
					r.StartedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&status, "status", "", "filter by status (running|completed|failed|timeout|cancelled)")
	c.Flags().IntVar(&limit, "limit", 0, "limit rows (0 = no limit)")
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of a table")
	return c
}

func agentEventsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "events <run-id>",
		Short: "Stream events recorded for one run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("bad run id: %w", err)
			}
			st, err := withStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			evs, err := st.Events(cmd.Context(), runID)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), evs)
			}
			for _, e := range evs {
				fmt.Fprintf(cmd.OutOrStdout(),
					"seq=%d ts=%s kind=%s payload=%s\n",
					e.Seq, e.TS.Format(time.RFC3339), e.Kind, string(e.Payload))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of human-readable text")
	return c
}

// --- run (register-then-run convenience) ---------------------------------

func agentRunCmd() *cobra.Command {
	var (
		configPath  string
		extraPrompt string
		resumeID    string
		asJSON      bool
		quiet       bool
		skipReg     bool
	)
	c := &cobra.Command{
		Use:   "run <agent-ref> --config <agent.yaml> [--prompt \"...\"]",
		Short: "Run a registered agent and record the result in the registry",
		Long: `Combines get + start-record + execute + finish-record in one
invocation. The agent must already be registered (see "conductor agent
register").

Use --no-record to skip writing this run to the registry (the underlying
backend still runs). Useful for one-off ad-hoc invocations that should
not pollute the audit trail.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := withStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			a, err := st.GetAgent(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			schema, err := configschema.Load(configPath)
			if err != nil {
				return err
			}
			// The registry records the agent's canonical backend regardless
			// of what --config says; callers register the agent with the
			// backend they intend to invoke.
			if schema.Agent.Backend != a.Backend {
				return fmt.Errorf(
					"config backend %q does not match agent backend %q",
					schema.Agent.Backend, a.Backend)
			}
			prompt, err := schema.TaskPrompt(extraPrompt)
			if err != nil {
				return err
			}
			opts, err := schema.ToExecOptions(extraPrompt, resumeID)
			if err != nil {
				return err
			}

			rec := newRunRecorder(st, a.ID, prompt)
			if !skipReg {
				if err := rec.start(cmd.Context()); err != nil {
					return err
				}
			}

			return runWithRecorder(cmd.Context(),
				cmd.OutOrStdout(), cmd.OutOrStderr(), schema.Agent.Backend,
				prompt, opts, rec, asJSON, quiet)
		},
	}
	c.Flags().StringVar(&configPath, "config", "conductor.yaml",
		"path to the conductor.yaml agent definition")
	c.Flags().StringVar(&extraPrompt, "prompt", "", "extra prompt appended to agent.prompt")
	c.Flags().StringVar(&resumeID, "resume", "", "session id from a previous run to continue")
	c.Flags().BoolVar(&asJSON, "json", false, "emit one JSON object per line for events and the final result")
	c.Flags().BoolVar(&quiet, "quiet", false, "suppress human-readable event rendering")
	c.Flags().BoolVar(&skipReg, "no-record", false,
		"do not write this run to the registry (event stream and final result still emit)")
	return c
}

// --- recorder ------------------------------------------------------------

// runRecorder best-effort records a run to the registry. It is best-
// effort: errors are logged but never block the agent run. We treat the
// registry as a *secondary* signal — the backend result is the source
// of truth (mirrors multica's preference for the underlying Claude
// session as truth and the registry as audit).
type runRecorder struct {
	store   *agentregistry.Store
	agentID int64
	prompt  string
	runID   int64
}

func newRunRecorder(s *agentregistry.Store, agentID int64, prompt string) *runRecorder {
	return &runRecorder{store: s, agentID: agentID, prompt: prompt}
}

func (r *runRecorder) start(ctx context.Context) error {
	runID, err := r.store.StartRun(ctx, agentregistry.Run{
		AgentID:   r.agentID,
		PromptSHA: agentregistry.ParsePrompt(r.prompt),
		SessionID: agentregistry.CurrentSessionID(os.Environ()),
	})
	if err != nil {
		return err
	}
	r.runID = runID
	return nil
}

// recordEvent is async-on-error: a write failure does not surface to
// the agent run. The caller invokes it from the message-drain loop.
func (r *runRecorder) recordEvent(ctx context.Context, kind string, payload []byte) {
	if r.runID == 0 {
		return
	}
	if err := r.store.AppendEvent(ctx, r.runID, kind, payload); err != nil {
		fmt.Fprintf(os.Stderr, "registry: append event: %v\n", err)
	}
}

// finish writes the terminal outcome; same best-effort stance as
// recordEvent. Errors never escape.
func (r *runRecorder) finish(ctx context.Context, res agent.Result) {
	if r.runID == 0 {
		return
	}
	fin := agentregistry.RunFinish{
		Status:     res.Status,
		DurationMs: res.DurationMs,
		Error:      res.Error,
		SessionID:  res.SessionID,
	}
	if len(res.Usage) > 0 {
		if b, err := json.Marshal(res.Usage); err == nil {
			fin.Usage = b
		}
	}
	if err := r.store.FinishRun(ctx, r.runID, fin); err != nil {
		fmt.Fprintf(os.Stderr, "registry: finish run: %v\n", err)
	}
}

// --- helpers -------------------------------------------------------------

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// truncDot returns s truncated to max runes, replacing interior runs
// of dots with an ellipsis-friendly form (used for backend session
// ids in CLI tables).
func truncDot(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max >= 3 {
		return s[:max-1] + "…"
	}
	return s[:max]
}

// --- doRunRecorded -------------------------------------------------------

// runWithRecorder is the thin wiring for `conductor agent run <ref>` —
// it injects identity env into the spawned CLI and then funnels the
// event + result stream through the shared executeBackend helper. The
// common path is identical to top-level `conductor run`; the only
// differences are: (a) the recorder's lifecycle is owned by the
// caller (agentcmd), and (b) identity env is propagated so the LLM
// can read its own parent run id.
func runWithRecorder(
	ctx context.Context,
	stdout, stderr io.Writer,
	backendType, prompt string,
	opts agent.ExecOptions,
	rec *runRecorder,
	asJSON, quiet bool,
) error {
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	backend, err := agent.New(backendType, agent.Config{Logger: logger})
	if err != nil {
		return err
	}
	// Inject identity env so the LLM CLI (or its children) can read
	// who is running. Mirrors multica's CONDUCTOR_* chain.
	if rec != nil && rec.runID > 0 {
		for _, e := range agentregistry.IdentityEnv(
			"", rec.runID, opts.ResumeSessionID,
		) {
			opts.Env = addKV(opts.Env, e)
		}
	}
	return executeBackend(ctx, stdout, stderr, backendType,
		backend, prompt, opts, rec, asJSON, quiet)
}

func addKV(env map[string]string, kv string) map[string]string {
	eq := strings.IndexByte(kv, '=')
	if eq < 0 {
		return env
	}
	if env == nil {
		env = map[string]string{}
	}
	env[kv[:eq]] = kv[eq+1:]
	return env
}
