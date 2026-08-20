// Subcommand: conductor ls
//
//	conductor ls                            # list all runs (table)
//	conductor ls --spec <id>                # filter by spec
//	conductor ls --status running,failed    # filter by status (csv)
//	conductor ls --limit 5                 # newest N only
//	conductor ls --json                     # one JSON RunState per line
//	conductor ls <runId>                    # show state + timeline for one run
//
// Phase 1 storage is backed by JSON files under
// $CONDUCTOR_HOME/runs/<runId>/, so this is essentially a
// pretty-printer around storage.JsonFileStorage.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"conductor/server/internal/cli"
	"conductor/server/internal/storage"
)

func runLs(ctx context.Context, args []string) error {
	return runLsWithWriter(ctx, args, cli.Stdout, cli.Stderr)
}

// runLsWithWriter is the testable form: takes explicit io.Writers
// instead of relying on the global cli.Stdout / cli.Stderr.
func runLsWithWriter(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := cli.NewFlagSet("conductor ls")
	f := &lsFlags{}
	fs.StringVar(&f.SpecID, "spec", "", "filter by spec id")
	fs.StringVar(&f.Status, "status", "", "filter by status (comma-separated: running,completed,failed,cancelled)")
	fs.IntVar(&f.Limit, "limit", 0, "max number of runs to show (0 = no limit)")
	fs.BoolVar(&f.JSON, "json", false, "emit one RunState JSON per line (machine-readable)")

	fs.Usage = func() {
		fmt.Fprint(stderr, `conductor ls — list stored runs

Usage:
  conductor ls [flags]
  conductor ls <runId> [flags]

List mode (no positional arg):
  Filter runs by --spec and/or --status, ordered newest first.

Show mode (<runId> positional):
  Print the run's state.json + timeline.ndjson contents.

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

	store, err := storage.NewJsonFileStorage()
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return err
	}

	// Show mode: <runId> given as positional.
	if runID := strings.TrimSpace(fs.Arg(0)); runID != "" {
		if fs.NArg() > 1 {
			fmt.Fprintf(stderr, "error: at most one runId argument allowed\n")
			return errors.New("too many arguments")
		}
		return lsShow(ctx, store, runID, f.JSON, stdout, stderr)
	}

	// List mode.
	filter := storage.RunFilter{
		SpecID: f.SpecID,
		Limit:  f.Limit,
	}
	if f.Status != "" {
		valid := map[string]bool{
			string(storage.RunStatusRunning):   true,
			string(storage.RunStatusCompleted): true,
			string(storage.RunStatusFailed):    true,
			string(storage.RunStatusCancelled): true,
		}
		for _, s := range strings.Split(f.Status, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if !valid[s] {
				fmt.Fprintf(stderr, "error: --status %q is not one of running/completed/failed/cancelled\n", s)
				return errors.New("invalid --status value")
			}
			filter.Status = append(filter.Status, storage.RunStatus(s))
		}
		if len(filter.Status) == 0 {
			fmt.Fprintf(stderr, "error: --status has no valid values\n")
			return errors.New("invalid --status")
		}
	}

	runs, err := store.ListRuns(ctx, filter)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return err
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "(no runs)")
		return nil
	}

	if f.JSON {
		for _, r := range runs {
			data, err := json.Marshal(r)
			if err != nil {
				fmt.Fprintf(stderr, "error: marshal %s: %s\n", r.RunID, err)
				continue
			}
			fmt.Fprintln(stdout, string(data))
		}
		return nil
	}

	lsPrintTable(stdout, runs)
	return nil
}

type lsFlags struct {
	SpecID string
	Status string
	Limit  int
	JSON   bool
}

// lsPrintTable renders runs as a tabwriter-aligned table.
//
// RUN ID             SPEC ID         STATUS      STARTED                DURATION  PROMPT                   SESSION
// 9d147836e1c435ff   resume-smoke-*  completed   2026-08-20 07:32:47   17ms      first prompt             thr-x
//
// Times are UTC and ISO-like (no 'Z' suffix to keep the column
// tidy); durations are short-form (<1s as "123ms", longer as "1.2s").
func lsPrintTable(w io.Writer, runs []storage.RunState) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN ID\tSPEC ID\tSTATUS\tSTARTED\tDURATION\tPROMPT\tSESSION")
	for _, r := range runs {
		dur := formatDuration(r)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.RunID,
			truncate(r.SpecID, 20),
			r.Status,
			r.StartedAt.UTC().Format("2006-01-02 15:04:05"),
			dur,
			truncate(r.Prompt, 32),
			truncate(r.SessionID, 16),
		)
	}
	_ = tw.Flush()
}

// formatDuration returns the wall-clock duration of a run. For
// still-running runs, "now - StartedAt" is reported (so the
// duration keeps ticking up between ls calls). For completed
// runs, "FinishedAt - StartedAt" is used.
func formatDuration(r storage.RunState) string {
	var d time.Duration
	switch r.Status {
	case storage.RunStatusRunning:
		d = time.Since(r.StartedAt)
	case storage.RunStatusCompleted, storage.RunStatusFailed, storage.RunStatusCancelled:
		if r.FinishedAt != nil {
			d = r.FinishedAt.Sub(r.StartedAt)
		}
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// truncate shortens s to n runes, adding an ellipsis when
// truncated. n <= 3 returns s unchanged (no room for ellipsis).
func truncate(s string, n int) string {
	if n <= 3 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// lsShow prints one run's full state.json + timeline.ndjson contents.
// --json emits the raw RunState + a {"timeline":[...]} envelope.
func lsShow(ctx context.Context, store storage.Storage, runID string, asJSON bool, stdout, stderr io.Writer) error {
	state, err := store.GetRun(ctx, runID)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return err
	}

	if asJSON {
		r, err := store.ReadTimeline(ctx, runID)
		if err != nil {
			fmt.Fprintf(stderr, "error: read timeline: %s\n", err)
			return err
		}
		defer r.Close()
		var items []storage.TimelineItem
		for {
			item, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fmt.Fprintf(stderr, "error: read timeline: %s\n", err)
				return err
			}
			items = append(items, item)
		}
		envelope := map[string]any{
			"state":    state,
			"timeline": items,
		}
		data, err := json.MarshalIndent(envelope, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(data))
		return nil
	}

	// Pretty text mode.
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "--- state.json ---")
	fmt.Fprintln(stdout, string(data))

	r, err := store.ReadTimeline(ctx, runID)
	if err != nil {
		fmt.Fprintf(stderr, "warn: read timeline: %s\n", err)
		return nil
	}
	defer r.Close()
	fmt.Fprintln(stdout, "\n--- timeline.ndjson ---")
	for {
		item, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Fprintf(stderr, "warn: read timeline: %s\n", err)
			return nil
		}
		// Re-marshal so the on-disk format matches what we'd write
		// (single-line JSON object). Easier for users to grep.
		line, err := json.Marshal(item)
		if err != nil {
			fmt.Fprintf(stderr, "warn: marshal timeline item: %s\n", err)
			continue
		}
		fmt.Fprintln(stdout, string(line))
	}
	return nil
}
