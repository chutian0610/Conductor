package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"conductor/server/internal/agentregistry"
	"conductor/server/internal/audit"

	"github.com/spf13/cobra"
)

// auditCmd implements `conductor audit <run-id>` and
// `conductor audit --pending`.
func auditCmd() *cobra.Command {
	var (
		asJSON bool
		force  bool
		model  string
		pending bool
		limit  int
	)
	c := &cobra.Command{
		Use:   "audit <run-id> [--json] [--force] [--model <name>]\n       audit --pending [--limit N] [--json]",
		Short: "Adversarial audit of a recorded run (ADR-0009)",
		Long: `Spawn a fresh LLM subprocess to read a recorded run's events
and emit a single-line JSON verdict (pass | fail | unverifiable) +
one-sentence evidence. The verdict is judgment, not enforcement — it
never blocks re-runs or changes the run's status. Operators read it
and decide.

Use --pending to list runs that have not been audited yet (or whose
last audit was a 'pending' row from a crashed mid-flight).`,
		Args: func(cmd *cobra.Command, args []string) error {
			if pending {
				if len(args) != 0 {
					return fmt.Errorf("--pending takes no <run-id>")
				}
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("expected 1 arg: <run-id>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, _ := cmd.Flags().GetString("registry")
			if dir == "" {
				dir, _ = os.Getwd()
			}
			reg, err := agentregistry.Open(dir)
			if err != nil {
				return err
			}
			defer reg.Close()
			ctx := cmd.Context()

			if pending {
				return runAuditPending(ctx, reg, limit, asJSON)
			}
			runID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("bad run id %q: %w", args[0], err)
			}
			return runAuditOne(ctx, reg, runID, audit.Options{
				Force: force,
				Model: model,
			}, asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit one JSON object per line on stdout; quiet stderr")
	c.Flags().BoolVar(&force, "force", false, "re-audit even if the run was already audited")
	c.Flags().StringVar(&model, "model", "", "override the auditor's model (default: claude's pick)")
	c.Flags().BoolVar(&pending, "pending", false, "list run-ids that haven't been audited yet; ignores <run-id>")
	c.Flags().IntVar(&limit, "limit", 50, "for --pending: max run-ids to print (0 = no limit)")
	return c
}

// runAuditOne executes the audit and prints the result.
func runAuditOne(ctx context.Context, reg *agentregistry.Store, runID int64, opts audit.Options, asJSON bool) error {
	res, err := audit.Run(ctx, reg, runID, opts)
	if err != nil {
		if errors.Is(err, audit.ErrAlreadyAudited) && !asJSON {
			fmt.Fprintln(os.Stderr,
				"# run "+strconv.FormatInt(runID, 10)+
					" already audited; pass --force to re-audit")
		}
		return err
	}
	if asJSON {
		return writeJSON(os.Stdout, res)
	}
	// Human-readable summary on stdout.
	fmt.Printf("audited run #%d  verdict=%s\n", res.RunID, res.Verdict)
	if res.Evidence != "" {
		fmt.Printf("  evidence: %s\n", res.Evidence)
	}
	if res.AuditID != 0 {
		fmt.Printf("  audit_id=%d  at=%d\n", res.AuditID, res.AuditedAt)
	}
	return nil
}

// runAuditPending prints the queue of runs that have no completed
// audit. stdout is empty (these are operator-facing machine-readable ids).
func runAuditPending(ctx context.Context, reg *agentregistry.Store, limit int, asJSON bool) error {
	ids, err := reg.ListPendingAudits(ctx, agentregistry.ListRunOpts{Limit: limit})
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(os.Stdout, ids)
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "(no pending audits)")
		return nil
	}
	for _, id := range ids {
		fmt.Println(id)
	}
	return nil
}
