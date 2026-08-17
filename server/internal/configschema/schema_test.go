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

	prompt, err := s.TaskPrompt("")
	if err != nil {
		t.Fatalf("TaskPrompt: %v", err)
	}
	if !strings.Contains(prompt, "Always be terse.") {
		t.Errorf("prompt = %q, missing skill content", prompt)
	}
	if !strings.Contains(prompt, "skill: skill.md") {
		t.Errorf("prompt = %q, missing skill header", prompt)
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
	// TaskPrompt should produce the merged text with cli last.
	full, err := s.TaskPrompt("cli prompt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full, "yaml prompt") || !strings.Contains(full, "cli prompt") {
		t.Errorf("TaskPrompt = %q, want both yaml and cli prompts", full)
	}
	if !strings.HasSuffix(full, "cli prompt") {
		t.Errorf("TaskPrompt = %q, cli prompt should be appended last", full)
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
