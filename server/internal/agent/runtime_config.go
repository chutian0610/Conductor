package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runtime_config.go writes per-provider context files into the workdir so
// the spawned CLI picks them up natively. claude reads CLAUDE.md from the
// workdir; codex reads AGENTS.md from the workdir. The brief travels
// through this file path instead of being appended to the spawned CLI's
// prompt, which avoids duplication when the CLI would otherwise see the
// same content twice (claude/codex both already pick up CLAUDE.md/AGENTS.md
// natively — see multica server/pkg/agent/agent.go:751).
//
// V1 scope: writes only the bare context file. Multica's execenv does
// much more (shell_environment_policy, per-provider config layers,
// daemon-managed MCP block markers, runtime GC). Those are out of scope
// here; add them as separate helpers if V2 needs them.

// providerNeedsInlineSystemPrompt mirrors multica's
// daemon.providerNeedsInlineSystemPrompt decision. It reports whether a
// backend should prepend SystemPrompt to the spawned CLI's prompt
// (because the CLI does NOT read a per-workdir context file). V1 has
// no provider that needs this — claude and codex both read CLAUDE.md
// and AGENTS.md respectively — so the function always returns false.
// Add cases here when a new backend arrives that lacks disk-based
// delivery.
func providerNeedsInlineSystemPrompt(provider string) bool {
	switch provider {
	// V1: claude (CLAUDE.md), codex (AGENTS.md). Neither needs inline.
	// Future cases (matching multica's list): openclaw, kimi, traecli,
	// qwenpaw, etc.
	default:
		return false
	}
}

// InjectRuntimeConfig writes the per-provider context file under
// workDir, creating it if needed. Returns nil for providers that read
// no per-workdir file. The file mode is 0o600 because runtime briefs
// frequently carry project context that the user wants to keep private.
//
// Mirrors multica server/internal/daemon/execenv.InjectRuntimeConfig
// stripped to V1's needs.
func InjectRuntimeConfig(workDir, provider, brief string) error {
	path := runtimeConfigPath(workDir, provider)
	if path == "" {
		// Provider reads no per-workdir file — nothing to do.
		return nil
	}
	// An empty workdir means "use the current process cwd". This keeps
	// ad-hoc CLI invocations (conductor run without a configured cwd)
	// working without forcing callers to set ExecOptions.Cwd.
	if workDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("runtime config: resolve cwd: %w", err)
		}
		workDir = cwd
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("runtime config: mkdir %s: %w", workDir, err)
	}
	// Re-resolve the path with the (possibly defaulted) workdir so
	// callers passing "" get a CLAUDE.md / AGENTS.md in their cwd, not
	// at the relative path "CLAUDE.md" which os.WriteFile would reject.
	path = runtimeConfigPath(workDir, provider)
	content := renderRuntimeConfig(provider, brief)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("runtime config: write %s: %w", path, err)
	}
	return nil
}

// runtimeConfigPath returns the absolute path to the per-provider
// context file, or "" when the provider has no file convention.
// Centralised so adding a new provider is a one-line table update.
func runtimeConfigPath(workDir, provider string) string {
	switch provider {
	case "claude":
		return filepath.Join(workDir, "CLAUDE.md")
	case "codex":
		return filepath.Join(workDir, "AGENTS.md")
	default:
		return ""
	}
}

// renderRuntimeConfig builds the file content for a given provider. The
// brief comes verbatim; we add a single trailing newline so file viewers
// don't complain about missing final EOL.
//
// V1 does not split the brief into multiple sections (no "system
// instructions" vs "skills" vs "task" delimiters). Future V2 may grow
// structured sections per multica's execenv pattern.
func renderRuntimeConfig(provider, brief string) string {
	var b strings.Builder
	b.WriteString(brief)
	if !strings.HasSuffix(brief, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}
