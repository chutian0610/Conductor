package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"conductor/server/internal/cli"
	"conductor/server/internal/home"
	"conductor/server/internal/spec"
)

// captureCLI runs fn while swapping cli.Stdout / cli.Stderr to
// bytes.Buffers, returning (stdout, stderr, err). Resets the
// writers on exit so other tests aren't affected.
func captureCLI(t *testing.T, fn func() error) (string, string, error) {
	t.Helper()
	origOut, origErr := cli.Stdout, cli.Stderr
	var outBuf, errBuf bytes.Buffer
	cli.Stdout, cli.Stderr = &outBuf, &errBuf
	defer func() { cli.Stdout, cli.Stderr = origOut, origErr }()
	err := fn()
	return outBuf.String(), errBuf.String(), err
}

// writeFakeCodexConfig creates a $CODEX_HOME with a config.toml
// declaring the named ones and returns the dir.
func writeFakeCodexConfig(t *testing.T, providers map[string]struct {
	baseURL string
	envKey  string
}) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	var b strings.Builder
	b.WriteString("# test fixture\n")
	for name, p := range providers {
		b.WriteString("\n[model_providers." + name + "]\n")
		if p.baseURL != "" {
			b.WriteString(`base_url = "` + p.baseURL + `"` + "\n")
		}
		if p.envKey != "" {
			b.WriteString(`env_key = "` + p.envKey + `"` + "\n")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	return dir
}

// TestSpecCreateHappyPath covers the full create flow: flag parse,
// provider resolve from codex config, spec.Create, output check.
func TestSpecCreateHappyPath(t *testing.T) {
	resetConductorHome(t)
	writeFakeCodexConfig(t, map[string]struct {
		baseURL string
		envKey  string
	}{
		"openrouter": {"https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"},
	})

	stdout, stderr, err := captureCLI(t, func() error {
		return runSpecCreate(context.Background(), []string{
			"--name", "opus-planner",
			"--provider", "openrouter",
			"--model", "anthropic/claude-opus-4-6",
			"--system-prompt", "stay terse",
			"--thinking", "high",
		})
	})
	if err != nil {
		t.Fatalf("runSpecCreate: %v (stderr=%q)", err, stderr)
	}
	if !strings.Contains(stdout, "created spec") {
		t.Errorf("stdout should announce creation, got %q", stdout)
	}
	if !strings.Contains(stdout, "home:") {
		t.Errorf("stdout should print home path, got %q", stdout)
	}

	// Spec should actually be on disk.
	records, err := spec.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d specs, want 1", len(records))
	}
	if records[0].Spec.Model != "anthropic/claude-opus-4-6" {
		t.Errorf("Model = %q", records[0].Spec.Model)
	}
	if records[0].Spec.SystemPrompt != "stay terse" {
		t.Errorf("SystemPrompt = %q", records[0].Spec.SystemPrompt)
	}
	if records[0].Spec.Thinking != "high" {
		t.Errorf("Thinking = %q", records[0].Spec.Thinking)
	}
}

// TestSpecCreateBuiltinCodex verifies that --provider codex works
// without any user config.toml (built-in fallback).
func TestSpecCreateBuiltinCodex(t *testing.T) {
	resetConductorHome(t)
	t.Setenv("CODEX_HOME", t.TempDir()) // empty home, no config.toml

	stdout, stderr, err := captureCLI(t, func() error {
		return runSpecCreate(context.Background(), []string{
			"--name", "default",
			"--model", "openai/gpt-5",
		})
	})
	if err != nil {
		t.Fatalf("runSpecCreate: %v (stderr=%q)", err, stderr)
	}
	if !strings.Contains(stdout, "created spec") {
		t.Errorf("stdout should announce creation, got %q", stdout)
	}
}

// TestSpecCreateRequiresModel guards the contract that --model is
// mandatory (Spec without a model is unusable).
func TestSpecCreateRequiresModel(t *testing.T) {
	resetConductorHome(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	_, stderr, err := captureCLI(t, func() error {
		return runSpecCreate(context.Background(), []string{
			"--name", "no-model",
		})
	})
	if err == nil {
		t.Fatalf("expected error for missing --model, got nil")
	}
	if !strings.Contains(stderr, "--model is required") {
		t.Errorf("stderr should mention --model, got %q", stderr)
	}
}

// TestSpecCreateMissingProvider covers the case where the user's
// config.toml doesn't declare the requested provider.
func TestSpecCreateMissingProvider(t *testing.T) {
	resetConductorHome(t)
	writeFakeCodexConfig(t, map[string]struct {
		baseURL string
		envKey  string
	}{
		"openai": {"https://api.openai.com/v1", "OPENAI_API_KEY"},
	})

	_, stderr, err := captureCLI(t, func() error {
		return runSpecCreate(context.Background(), []string{
			"--name", "foo",
			"--provider", "openrouter",
			"--model", "x",
		})
	})
	if err == nil {
		t.Fatalf("expected error for missing provider, got nil")
	}
	if !strings.Contains(stderr, "provider not found") {
		t.Errorf("stderr should mention provider not found, got %q", stderr)
	}
}

// TestSpecCreateAlreadyExists verifies that running create twice
// with the same input returns ErrAlreadyExists (and a friendly
// stderr message that includes the would-be id).
func TestSpecCreateAlreadyExists(t *testing.T) {
	resetConductorHome(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	args := []string{"--name", "dup", "--model", "x"}
	if _, _, err := captureCLI(t, func() error {
		return runSpecCreate(context.Background(), args)
	}); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	_, stderr, err := captureCLI(t, func() error {
		return runSpecCreate(context.Background(), args)
	})
	if !errors.Is(err, spec.ErrAlreadyExists) {
		t.Errorf("err = %v, want ErrAlreadyExists", err)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr should announce already-exists, got %q", stderr)
	}
}

// TestSpecCreateCommaSeparatedLists verifies --tools-allow=bash,read
// produces a []string with both elements (StringSliceFlag works).
func TestSpecCreateCommaSeparatedLists(t *testing.T) {
	resetConductorHome(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	if _, _, err := captureCLI(t, func() error {
		return runSpecCreate(context.Background(), []string{
			"--name", "listy",
			"--model", "x",
			"--tools-allow", "bash,read,write",
			"--skills", "review,ship",
		})
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	records, _ := spec.List(context.Background())
	if len(records) != 1 {
		t.Fatalf("want 1 spec, got %d", len(records))
	}
	want := []string{"bash", "read", "write"}
	if len(records[0].Spec.ToolsAllow) != 3 {
		t.Fatalf("ToolsAllow = %v, want 3 entries", records[0].Spec.ToolsAllow)
	}
	for i, w := range want {
		if records[0].Spec.ToolsAllow[i] != w {
			t.Errorf("ToolsAllow[%d] = %q, want %q", i, records[0].Spec.ToolsAllow[i], w)
		}
	}
	if len(records[0].Spec.Skills) != 2 ||
		records[0].Spec.Skills[0] != "review" ||
		records[0].Spec.Skills[1] != "ship" {
		t.Errorf("Skills = %v, want [review ship]", records[0].Spec.Skills)
	}
}

// TestSpecListEmpty covers the "no specs registered" branch.
func TestSpecListEmpty(t *testing.T) {
	resetConductorHome(t)

	stdout, _, err := captureCLI(t, func() error {
		return runSpecList(context.Background(), nil)
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(stdout, "no specs registered") {
		t.Errorf("stdout = %q, want 'no specs registered' message", stdout)
	}
}

// TestSpecListMultiple verifies table output with multiple specs.
func TestSpecListMultiple(t *testing.T) {
	resetConductorHome(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if _, _, err := captureCLI(t, func() error {
			return runSpecCreate(context.Background(), []string{
				"--name", name, "--model", "m-" + name,
			})
		}); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	stdout, _, err := captureCLI(t, func() error {
		return runSpecList(context.Background(), nil)
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, want := range []string{"alpha", "bravo", "charlie"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout should mention %q, got %q", want, stdout)
		}
	}
	for _, want := range []string{"SPEC ID", "NAME", "MODEL", "PROVIDER"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout should have header %q, got %q", want, stdout)
		}
	}
}

// TestSpecShowTextOutput verifies the human-readable show format.
func TestSpecShowTextOutput(t *testing.T) {
	resetConductorHome(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	if _, _, err := captureCLI(t, func() error {
		return runSpecCreate(context.Background(), []string{
			"--name", "show-me",
			"--model", "openai/gpt-5",
			"--system-prompt", "be terse",
		})
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	records, _ := spec.List(context.Background())
	specId := records[0].Spec.SpecId

	stdout, _, err := captureCLI(t, func() error {
		return runSpecShow(context.Background(), []string{specId})
	})
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	for _, want := range []string{
		"SpecId:",
		"Provider:",
		"Model:",
		"HomePath:",
		"ConfigToml:",
		"CreatedAt:",
		"openai/gpt-5",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout should contain %q, got %q", want, stdout)
		}
	}
}

// TestSpecShowJSON verifies the --json flag produces parseable JSON.
func TestSpecShowJSON(t *testing.T) {
	resetConductorHome(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	if _, _, err := captureCLI(t, func() error {
		return runSpecCreate(context.Background(), []string{
			"--name", "json-test", "--model", "x",
		})
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	records, _ := spec.List(context.Background())
	specId := records[0].Spec.SpecId

	stdout, _, err := captureCLI(t, func() error {
		return runSpecShow(context.Background(), []string{"--json", specId})
	})
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	// Top-level JSON object with SpecId / HomePath keys.
	if !strings.Contains(stdout, `"specId"`) || !strings.Contains(stdout, `"homePath"`) {
		t.Errorf("stdout should contain JSON keys, got %q", stdout)
	}
	// Should be valid JSON (re-marshal test).
	if _, err := decodeJSONLoose(stdout); err != nil {
		t.Errorf("stdout not valid JSON: %v\n%s", err, stdout)
	}
}

// TestSpecShowNotFound covers the error path.
func TestSpecShowNotFound(t *testing.T) {
	resetConductorHome(t)
	_, stderr, err := captureCLI(t, func() error {
		return runSpecShow(context.Background(), []string{"nonexistent"})
	})
	if !errors.Is(err, spec.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr should mention not found, got %q", stderr)
	}
}

// TestSpecRm covers delete + verifies the spec is gone afterwards.
func TestSpecRm(t *testing.T) {
	resetConductorHome(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	if _, _, err := captureCLI(t, func() error {
		return runSpecCreate(context.Background(), []string{
			"--name", "rm-me", "--model", "x",
		})
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	records, _ := spec.List(context.Background())
	specId := records[0].Spec.SpecId

	stdout, _, err := captureCLI(t, func() error {
		return runSpecRm(context.Background(), []string{specId})
	})
	if err != nil {
		t.Fatalf("Rm: %v", err)
	}
	if !strings.Contains(stdout, "removed spec") {
		t.Errorf("stdout should announce removal, got %q", stdout)
	}

	if _, err := os.Stat(home.SpecMetaPath(specId)); !os.IsNotExist(err) {
		t.Errorf("spec.json should be gone, got err = %v", err)
	}
	if _, err := os.Stat(records[0].HomePath); !os.IsNotExist(err) {
		t.Errorf("HOME should be gone, got err = %v", err)
	}
}

// TestSpecRmNotFound covers the error path.
func TestSpecRmNotFound(t *testing.T) {
	resetConductorHome(t)
	_, stderr, err := captureCLI(t, func() error {
		return runSpecRm(context.Background(), []string{"does-not-exist"})
	})
	if !errors.Is(err, spec.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr should mention not found, got %q", stderr)
	}
}

// TestSpecHelpAction covers the `conductor spec --help` (no action)
// branch.
func TestSpecHelpAction(t *testing.T) {
	stdout, _, err := captureCLI(t, func() error {
		return runSpec(context.Background(), nil)
	})
	if err != nil {
		t.Fatalf("runSpec(nil): %v", err)
	}
	if !strings.Contains(stdout, "conductor spec <action>") {
		t.Errorf("stdout should print usage, got %q", stdout)
	}
}

// TestSpecUnknownAction covers a bad subcommand.
func TestSpecUnknownAction(t *testing.T) {
	_, stderr, err := captureCLI(t, func() error {
		return runSpec(context.Background(), []string{"frobnicate"})
	})
	if err == nil {
		t.Fatalf("expected error for unknown action")
	}
	if !strings.Contains(stderr, "unknown action") {
		t.Errorf("stderr should mention unknown action, got %q", stderr)
	}
}

// decodeJSONLoose validates that s parses as JSON without caring
// about the shape (used by the --json show test).
func decodeJSONLoose(s string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// resetConductorHome points $CONDUCTOR_HOME at a fresh temp dir
// so spec writes don't leak between cases.
func resetConductorHome(t *testing.T) {
	t.Helper()
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
}
