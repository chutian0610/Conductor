package configschema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse_MinimalValid(t *testing.T) {
	yaml := `
agent:
  backend: claude
`
	s, err := Parse([]byte(yaml), "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Version != 1 {
		t.Errorf("Version = %d, want 1", s.Version)
	}
	if s.Agent.Backend != "claude" {
		t.Errorf("Backend = %q, want claude", s.Agent.Backend)
	}
}

func TestParse_UnknownBackend(t *testing.T) {
	yaml := `
agent:
  backend: gpt-9000
`
	if _, err := Parse([]byte(yaml), ""); err == nil {
		t.Fatal("expected error for unknown backend")
	} else if !strings.Contains(err.Error(), "gpt-9000") {
		t.Errorf("error %q should mention the offending backend", err)
	}
}

func TestParse_KnownFieldsStrict(t *testing.T) {
	// Unknown top-level field is rejected — protects users from typos.
	yaml := `
version: 1
agents:
  - backend: claude
`
	if _, err := Parse([]byte(yaml), ""); err == nil {
		t.Fatal("expected error for unknown top-level field")
	}
}

func TestParse_SkillsAndMCP(t *testing.T) {
	dir := t.TempDir()
	skill := filepath.Join(dir, "skill.md")
	if err := os.WriteFile(skill, []byte("Always be terse."), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `
version: 1
agent:
  backend: claude
  skills:
    - skill.md
  mcp:
    servers:
      - name: fs
        command: npx
        args: ["-y", "@anthropic/server-filesystem", "."]
      - name: remote
        url: https://example.com/mcp
        headers:
          Authorization: "Bearer xyz"
`
	s, err := Parse([]byte(yaml), dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := filepath.Base(s.Agent.Skills[0]); got != "skill.md" {
		t.Errorf("skill path = %s, want .../skill.md", s.Agent.Skills[0])
	}
	if len(s.Agent.MCP.Servers) != 2 {
		t.Fatalf("MCP servers = %d, want 2", len(s.Agent.MCP.Servers))
	}
	if s.Agent.MCP.Servers[1].URL == "" {
		t.Errorf("expected url on second server")
	}

	// V2 brief-routing split (ADR-0005): TaskPrompt carries ONLY the
	// per-turn task. The brief + skills live in CLAUDE.md / AGENTS.md
	// via opts.SystemPrompt — see split contract pinned by the new
	// TestTaskPrompt_DoesNotContainBrief below. Here we just verify
	// the empty-override path.
	prompt, err := s.TaskPrompt("")
	if err != nil {
		t.Fatalf("TaskPrompt: %v", err)
	}
	if prompt != "" {
		t.Errorf("TaskPrompt with empty override = %q, want empty", prompt)
	}

	raw, err := s.McpConfigJSON()
	if err != nil {
		t.Fatalf("McpConfigJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal MCP JSON: %v", err)
	}
	servers, ok := got["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong shape: %v", got)
	}
	if _, ok := servers["fs"]; !ok {
		t.Errorf("fs server missing from MCP JSON")
	}
	if _, ok := servers["remote"]; !ok {
		t.Errorf("remote server missing from MCP JSON")
	}
}

func TestParse_MCP_MixedTransportRejected(t *testing.T) {
	yaml := `
agent:
  backend: claude
  mcp:
    servers:
      - name: bad
        url: https://example.com/mcp
        command: npx
`
	if _, err := Parse([]byte(yaml), ""); err == nil {
		t.Fatal("expected error for mixed http+stdio MCP entry")
	}
}

func TestToExecOptions_BadTimeout(t *testing.T) {
	// Bad timeouts are caught at ToExecOptions, not at Parse — Parse is
	// for structural validation only.
	yaml := `
agent:
  backend: claude
  timeout: not-a-duration
`
	s, err := Parse([]byte(yaml), "")
	if err != nil {
		t.Fatalf("Parse should accept any string for timeout: %v", err)
	}
	if _, err := s.ToExecOptions("", ""); err == nil {
		t.Fatal("expected ToExecOptions to reject bad timeout")
	}
}

func TestToExecOptions_PromptPrecedence(t *testing.T) {
	yaml := `
agent:
  backend: claude
  prompt: "yaml prompt"
`
	s, err := Parse([]byte(yaml), "")
	if err != nil {
		t.Fatal(err)
	}
	opts, err := s.ToExecOptions("cli prompt", "")
	if err != nil {
		t.Fatal(err)
	}
	// SystemPrompt carries only the YAML brief (NOT the cli override).
	// The cli override goes into the prompt parameter via TaskPrompt.
	if !strings.Contains(opts.SystemPrompt, "yaml prompt") {
		t.Errorf("SystemPrompt = %q, missing yaml prompt", opts.SystemPrompt)
	}
	if strings.Contains(opts.SystemPrompt, "cli prompt") {
		t.Errorf("SystemPrompt = %q, should NOT contain cli prompt (that goes to the prompt parameter)", opts.SystemPrompt)
	}
	// V2 split: TaskPrompt returns ONLY the cli override (no brief
	// duplication). The yaml brief lives in opts.SystemPrompt below.
	full, err := s.TaskPrompt("cli prompt")
	if err != nil {
		t.Fatal(err)
	}
	if full != "cli prompt" {
		t.Errorf("TaskPrompt with cli override = %q, want %q", full, "cli prompt")
	}
	// opts.SystemPrompt still carries the yaml brief (unaffected).
	if !strings.Contains(opts.SystemPrompt, "yaml prompt") {
		t.Errorf("SystemPrompt = %q, missing yaml prompt", opts.SystemPrompt)
	}
	if strings.Contains(opts.SystemPrompt, "cli prompt") {
		t.Errorf("SystemPrompt = %q, should NOT contain cli prompt (that goes via TaskPrompt)", opts.SystemPrompt)
	}
}

func TestToExecOptions_NoMCPLeavesNil(t *testing.T) {
	yaml := `
agent:
  backend: claude
`
	s, _ := Parse([]byte(yaml), "")
	opts, err := s.ToExecOptions("", "")
	if err != nil {
		t.Fatal(err)
	}
	if opts.McpConfig != nil {
		t.Errorf("McpConfig should be nil when no servers declared")
	}
}

// TestTaskPrompt_DoesNotContainBrief pins the V2 brief-routing split
// (ADR-0005): TaskPrompt returns ONLY the per-turn override. The
// brief travels through opts.SystemPrompt + InjectRuntimeConfig →
// CLAUDE.md/AGENTS.md. This split avoids the V1 duplication where the
// same brief content was delivered both as the context file and as
// the user-prompt argument to the spawned CLI.
func TestTaskPrompt_DoesNotContainBrief(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, "skill.md")
	if err := os.WriteFile(skillPath, []byte("Always be terse.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`agent:
  backend: claude
  prompt: "the yaml brief"
  skills:
    - skill.md
`)
	s, err := Parse(yaml, dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// 1. TaskPrompt with empty override returns "" (pure per-turn path).
	got, err := s.TaskPrompt("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("TaskPrompt with empty override = %q, want empty", got)
	}

	// 2. TaskPrompt with override returns only the override (no brief).
	got, err = s.TaskPrompt("per turn question")
	if err != nil {
		t.Fatal(err)
	}
	if got != "per turn question" {
		t.Errorf("TaskPrompt with override = %q, want %q", got, "per turn question")
	}
	if strings.Contains(got, "yaml brief") {
		t.Errorf("TaskPrompt leaks the brief into the per-turn prompt: %q", got)
	}
	if strings.Contains(got, "skill") {
		t.Errorf("TaskPrompt leaks skill content into the per-turn prompt: %q", got)
	}

	// 3. ToExecOptions still routes the brief into opts.SystemPrompt.
	opts, err := s.ToExecOptions("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opts.SystemPrompt, "the yaml brief") {
		t.Errorf("SystemPrompt = %q, missing yaml brief", opts.SystemPrompt)
	}
	if !strings.Contains(opts.SystemPrompt, "Always be terse.") {
		t.Errorf("SystemPrompt = %q, missing skill content", opts.SystemPrompt)
	}
}
