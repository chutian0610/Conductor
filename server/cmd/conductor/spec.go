// Subcommand: conductor spec
//
//	conductor spec create --name <n> --provider <p> --model <m> [flags]
//	conductor spec list
//	conductor spec show [--json] <specId>
//	conductor spec rm <specId>
//
// Each subcommand runs synchronously and uses cli.ParseFlags so
// --help returns ErrHelp (handled in runSpec as "no-op").
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"conductor/server/internal/cli"
	"conductor/server/internal/codexconfig"
	"conductor/server/internal/protocol"
	"conductor/server/internal/spec"
)

func runSpec(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printSpecUsage()
		return nil
	}
	switch args[0] {
	case "create":
		return runSpecCreate(ctx, args[1:])
	case "list", "ls":
		return runSpecList(ctx, args[1:])
	case "show":
		return runSpecShow(ctx, args[1:])
	case "rm", "remove", "delete":
		return runSpecRm(ctx, args[1:])
	case "--help", "-h", "help":
		printSpecUsage()
		return nil
	default:
		cli.Errorf("conductor spec: unknown action %q", args[0])
		printSpecUsage()
		return errors.New("unknown spec action")
	}
}

func printSpecUsage() {
	cli.Println(`conductor spec <action> — manage agent specs

Actions:
  create             register a new spec (writes HOME + spec.json)
  list (ls)          list registered specs
  show [--json] <id> print one spec's details
  rm (remove) <id>   delete a spec and its HOME

Run 'conductor spec <action> --help' for action-specific flags.`)
}

// --- spec create ---

type specCreateFlags struct {
	Name         string
	Provider     string
	Model        string
	SystemPrompt string
	Thinking     string
	Skills       []string
	MCPConfig    string
	ToolsAllow   []string
	ToolsExclude []string
	Cwd          string
	Worktree     string
}

func runSpecCreate(ctx context.Context, args []string) error {
	fs := cli.NewFlagSet("conductor spec create")
	f := &specCreateFlags{}
	fs.StringVar(&f.Name, "name", "", "human-readable name (used as SpecId prefix)")
	fs.StringVar(&f.Provider, "provider", "codex", "provider id (e.g. codex, openrouter)")
	fs.StringVar(&f.Model, "model", "", "model id (e.g. anthropic/claude-opus-4-6)")
	fs.StringVar(&f.SystemPrompt, "system-prompt", "", "system prompt override")
	fs.StringVar(&f.Thinking, "thinking", "", "reasoning effort (minimal/low/medium/high)")
	fs.Var(cli.StringSliceFlag{Dest: &f.Skills}, "skills", "comma-separated skill names")
	fs.StringVar(&f.MCPConfig, "mcp-config", "", "path to MCP config JSON")
	fs.Var(cli.StringSliceFlag{Dest: &f.ToolsAllow}, "tools-allow", "comma-separated tool allow list")
	fs.Var(cli.StringSliceFlag{Dest: &f.ToolsExclude}, "tools-exclude", "comma-separated tool exclude list")
	fs.StringVar(&f.Cwd, "cwd", "", "default working directory")
	fs.StringVar(&f.Worktree, "worktree", "", "git worktree branch (implies mode=branch_off)")

	fs.Usage = func() {
		fmt.Fprint(cli.Stderr, `conductor spec create — register a new agent spec

Usage:
  conductor spec create --name <name> --provider <p> --model <m> [flags]

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

	if f.Model == "" {
		cli.Errorf("--model is required")
		return errors.New("--model required")
	}

	pc, err := codexconfig.Resolve("", f.Provider)
	if err != nil {
		cli.Errorf("resolve provider %q: %s", f.Provider, err)
		return fmt.Errorf("resolve provider %q: %w", f.Provider, err)
	}
	if pc.BaseURL == "" {
		cli.Errorf("provider %q has no base_url; add [model_providers.%s] base_url = \"...\" to your codex config.toml",
			f.Provider, f.Provider)
		return errors.New("provider has no base_url")
	}

	in := spec.CreateInput{
		Spec: protocol.AgentSpec{
			Provider:     protocol.AgentProvider(f.Provider),
			Model:        f.Model,
			Name:         f.Name,
			SystemPrompt: f.SystemPrompt,
			Thinking:     f.Thinking,
			Skills:       f.Skills,
			MCPConfig:    f.MCPConfig,
			ToolsAllow:   f.ToolsAllow,
			ToolsExclude: f.ToolsExclude,
			Cwd:          f.Cwd,
		},
		BaseURL:            pc.BaseURL,
		EnvKey:             pc.EnvKey,
		WireAPI:            pc.WireAPI,
		RequiresOpenAIAuth: pc.RequiresOpenAIAuth,
	}
	if f.Worktree != "" {
		in.Spec.Worktree = &protocol.WorktreeSpec{
			Branch: f.Worktree,
			Mode:   "branch_off",
		}
	}

	res, err := spec.Create(ctx, in)
	if err != nil {
		if errors.Is(err, spec.ErrAlreadyExists) {
			cli.Errorf("spec already exists (SpecId: %s)", resOrEmptySpecId(res))
			return err
		}
		cli.Errorf("%s", err)
		return err
	}
	cli.Println("created spec %s", res.SpecId)
	cli.Println("  home: %s", res.Record.HomePath)
	cli.Println("  config.toml: %s", res.Record.ConfigToml)
	return nil
}

// resOrEmptySpecId returns the spec id from a partial CreateResult
// even on error (so error messages can include the would-be id).
func resOrEmptySpecId(res spec.CreateResult) string {
	return res.SpecId
}

// --- spec list ---

func runSpecList(ctx context.Context, args []string) error {
	fs := cli.NewFlagSet("conductor spec list")
	if err := cli.ParseFlags(fs, args); err != nil {
		if errors.Is(err, cli.ErrHelp) {
			return nil
		}
		return err
	}

	records, err := spec.List(ctx)
	if err != nil {
		cli.Errorf("%s", err)
		return err
	}
	if len(records) == 0 {
		cli.Println("(no specs registered)")
		return nil
	}

	tw := tabwriter.NewWriter(cli.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SPEC ID\tNAME\tMODEL\tPROVIDER\tCREATED")
	for _, r := range records {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.Spec.SpecId,
			emptyAsDash(r.Spec.Name),
			r.Spec.Model,
			r.Spec.Provider,
			r.Spec.CreatedAt.Local().Format("2006-01-02 15:04"),
		)
	}
	return tw.Flush()
}

func emptyAsDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// --- spec show ---

func runSpecShow(ctx context.Context, args []string) error {
	fs := cli.NewFlagSet("conductor spec show")
	jsonOut := fs.Bool("json", false, "print as JSON instead of human-readable fields")
	if err := cli.ParseFlags(fs, args); err != nil {
		if errors.Is(err, cli.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() < 1 {
		cli.Errorf("usage: conductor spec show [--json] <specId>")
		return errors.New("missing specId argument")
	}
	rec, err := spec.Get(ctx, fs.Arg(0))
	if err != nil {
		cli.Errorf("%s", err)
		return err
	}

	if *jsonOut {
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return err
		}
		cli.Println("%s", string(data))
		return nil
	}

	cli.Println("SpecId:      %s", rec.Spec.SpecId)
	cli.Println("Name:        %s", emptyAsDash(rec.Spec.Name))
	cli.Println("Provider:    %s", rec.Spec.Provider)
	cli.Println("Model:       %s", rec.Spec.Model)
	if rec.Spec.SystemPrompt != "" {
		cli.Println("System:      %s", rec.Spec.SystemPrompt)
	}
	if rec.Spec.Thinking != "" {
		cli.Println("Thinking:    %s", rec.Spec.Thinking)
	}
	if len(rec.Spec.Skills) > 0 {
		cli.Println("Skills:      %v", rec.Spec.Skills)
	}
	if rec.Spec.MCPConfig != "" {
		cli.Println("MCPConfig:   %s", rec.Spec.MCPConfig)
	}
	if rec.Spec.Cwd != "" {
		cli.Println("Cwd:         %s", rec.Spec.Cwd)
	}
	if rec.Spec.Worktree != nil {
		cli.Println("Worktree:    branch=%s mode=%s",
			emptyAsDash(rec.Spec.Worktree.Branch), emptyAsDash(rec.Spec.Worktree.Mode))
	}
	cli.Println("HomePath:    %s", rec.HomePath)
	cli.Println("ConfigToml:  %s", rec.ConfigToml)
	cli.Println("CreatedAt:   %s", rec.Spec.CreatedAt.UTC().Format(time.RFC3339))
	cli.Println("UpdatedAt:   %s", rec.Spec.UpdatedAt.UTC().Format(time.RFC3339))
	return nil
}

// --- spec rm ---

func runSpecRm(ctx context.Context, args []string) error {
	fs := cli.NewFlagSet("conductor spec rm")
	if err := cli.ParseFlags(fs, args); err != nil {
		if errors.Is(err, cli.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() < 1 {
		cli.Errorf("usage: conductor spec rm <specId>")
		return errors.New("missing specId argument")
	}
	specId := fs.Arg(0)
	if err := spec.Remove(ctx, specId); err != nil {
		cli.Errorf("%s", err)
		return err
	}
	cli.Println("removed spec %s", specId)
	return nil
}
