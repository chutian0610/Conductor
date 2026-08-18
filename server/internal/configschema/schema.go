// Package configschema defines the conductor.yaml document and produces an
// backend.ExecOptions from it.
//
// The schema is deliberately YAML-shaped (not Go struct tags) so future
// versions can validate documents with a JSON Schema validator if that
// becomes useful — for now we validate by hand in Validate().
//
// v0 contract — what every field means:
//
//	version    MUST be 1.
//	agent      The single agent definition this conductor run will use.
//	           Multi-agent dispatch is a V2 concern (DAG / plan layer).
//	agent.backend   One of backend.IsSupportedType()'s list. Drives which
//	                Backend implementation handles Execute().
//	agent.model     Model identifier (passed verbatim to the CLI's
//	                --model flag). Empty means "let the CLI choose".
//	agent.thinking  Backend-native effort level ("low|medium|high|...").
//	agent.cwd       Working directory for the spawned CLI. Relative
//	                paths resolve against the directory containing
//	                conductor.yaml.
//	agent.max_turns 0 = unlimited.
//	agent.timeout   "30m" / "1h" / "0s" (disable). Empty disables.
//	agent.prompt    The system brief. Concatenated with anything passed
//	                via --prompt on the CLI.
//	agent.skills    List of file/dir paths whose contents are appended
//	                to agent.prompt, in order. Relative paths resolve
//	                against the conductor.yaml directory. Skills are
//	                plain text — conductor does not interpret them.
//	agent.args      Extra CLI flags appended verbatim. Each backend has
//	                a blocklist of flags it owns and silently drops any
//	                it sees here.
//	agent.env       Extra environment variables for the spawned CLI.
//	                Overrides inherited values.
//	agent.mcp.servers   MCP servers to expose. Each entry is forwarded
//	                    to the CLI's --mcp-config. If absent, the CLI
//	                    inherits whatever the user has configured
//	                    locally.
package configschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"conductor/server/internal/backend"

	"gopkg.in/yaml.v3"
)

// Schema is the top-level conductor.yaml document.
type Schema struct {
	Version int   `yaml:"version"`
	Agent   Agent `yaml:"agent"`
}

// Agent is the single agent definition.
type Agent struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description,omitempty"`
	Backend     string            `yaml:"backend"`
	Model       string            `yaml:"model,omitempty"`
	Thinking    string            `yaml:"thinking,omitempty"`
	Cwd         string            `yaml:"cwd,omitempty"`
	MaxTurns    int               `yaml:"max_turns,omitempty"`
	Timeout     string            `yaml:"timeout,omitempty"`
	Prompt      string            `yaml:"prompt,omitempty"`
	Skills      []string          `yaml:"skills,omitempty"`
	Args        []string          `yaml:"args,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"`
	MCP         MCP               `yaml:"mcp,omitempty"`
}

// MCP is the MCP server block.
type MCP struct {
	Servers []MCPServer `yaml:"servers,omitempty"`
}

// MCPServer is one entry of the MCP config. Both transport shapes
// supported by the Claude CLI today are accepted: stdio (Command + Args +
// Cwd) and HTTP (URL + Headers).
type MCPServer struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport,omitempty"` // "stdio" (default) or "http"
	Command   string            `yaml:"command,omitempty"`
	Args      []string          `yaml:"args,omitempty"`
	Cwd       string            `yaml:"cwd,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	URL       string            `yaml:"url,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
}

// Load reads, parses, and validates a conductor.yaml at path. The returned
// Schema is ready to convert to backend.ExecOptions via ToExecOptions().
func Load(path string) (*Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data, filepath.Dir(path))
}

// Parse parses and validates raw YAML. baseDir is the directory the YAML
// was loaded from; relative Skill / Cwd paths resolve against it.
func Parse(raw []byte, baseDir string) (*Schema, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var s Schema
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if baseDir != "" {
		s.resolvePaths(baseDir)
	}
	return &s, nil
}

// Validate enforces the schema contract. It runs before path resolution,
// so paths are still in their YAML-relative form when reported in errors.
func (s *Schema) Validate() error {
	if s.Version == 0 {
		s.Version = 1 // default; matches the documented current version
	}
	if s.Version != 1 {
		return fmt.Errorf("schema: unsupported version %d (conductor supports version 1)", s.Version)
	}
	if s.Agent.Backend == "" {
		return errors.New("schema: agent.backend is required")
	}
	if !backend.IsSupportedType(s.Agent.Backend) {
		return fmt.Errorf("schema: agent.backend %q is not supported (try: %v)",
			s.Agent.Backend, backend.SupportedTypes)
	}
	if s.Agent.MaxTurns < 0 {
		return errors.New("schema: agent.max_turns must be >= 0")
	}
	for i, sk := range s.Agent.Skills {
		if strings.TrimSpace(sk) == "" {
			return fmt.Errorf("schema: agent.skills[%d] is empty", i)
		}
	}
	for i, srv := range s.Agent.MCP.Servers {
		if srv.Name == "" {
			return fmt.Errorf("schema: agent.mcp.servers[%d].name is required", i)
		}
		if srv.URL != "" {
			// HTTP transport — command/args/cwd must not be set.
			if srv.Command != "" || len(srv.Args) > 0 || srv.Cwd != "" {
				return fmt.Errorf("schema: agent.mcp.servers[%d] (%q) mixes http and stdio fields", i, srv.Name)
			}
		} else {
			if srv.Command == "" {
				return fmt.Errorf("schema: agent.mcp.servers[%d] (%q) requires either url (http) or command (stdio)", i, srv.Name)
			}
		}
	}
	return nil
}

// resolvePaths rewrites relative paths so the rest of conductor can treat
// them as absolute. baseDir is the directory containing conductor.yaml.
func (s *Schema) resolvePaths(baseDir string) {
	if s.Agent.Cwd != "" && !filepath.IsAbs(s.Agent.Cwd) {
		s.Agent.Cwd = filepath.Join(baseDir, s.Agent.Cwd)
	}
	for i, sk := range s.Agent.Skills {
		if !filepath.IsAbs(sk) {
			s.Agent.Skills[i] = filepath.Join(baseDir, sk)
		}
	}
}

// RuntimeBrief returns the brief portion of the agent definition:
// agent.prompt concatenated with the contents of every skill file, with a
// header on each skill. This is the content persisted to the workdir's
// per-provider context file (claude → CLAUDE.md, codex → AGENTS.md) by
// the backend before spawning the CLI, so the LLM reads it natively
// instead of receiving it twice (once on disk, once via stdin).
//
// Routing decision lives in conductor/server/internal/backend/runtime_config.go.
func (s *Schema) RuntimeBrief() (string, error) {
	var b strings.Builder
	if s.Agent.Prompt != "" {
		b.WriteString(s.Agent.Prompt)
	}
	for i, sk := range s.Agent.Skills {
		data, err := os.ReadFile(sk)
		if err != nil {
			return "", fmt.Errorf("read skill %s: %w", sk, err)
		}
		if i > 0 || s.Agent.Prompt != "" {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "--- skill: %s ---\n", filepath.Base(sk))
		b.Write(data)
	}
	return b.String(), nil
}

// TaskPrompt returns the full prompt passed to the spawned CLI: brief +
// skills + any extra text from --prompt. This is what the LLM sees as
// its turn input. For providers that read from disk, the brief portion
// will duplicate content from CLAUDE.md/AGENTS.md — that is intentional
// for V1 to preserve the existing "everything via stdin" semantics; V2
// will split cleanly so the prompt parameter carries only the task.
func (s *Schema) TaskPrompt(extraPrompt string) (string, error) {
	brief, err := s.RuntimeBrief()
	if err != nil {
		return "", err
	}
	if extraPrompt == "" {
		return brief, nil
	}
	if brief == "" {
		return extraPrompt, nil
	}
	return brief + "\n\n---\n\n" + extraPrompt, nil
}

// McpConfigJSON renders the MCP block as the JSON object expected by the
// Claude CLI's --mcp-config flag. Returns nil if there are no servers
// (so the CLI inherits whatever the user has configured locally).
func (s *Schema) McpConfigJSON() (json.RawMessage, error) {
	if len(s.Agent.MCP.Servers) == 0 {
		return nil, nil
	}
	mcpConfig := struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}{
		MCPServers: make(map[string]json.RawMessage, len(s.Agent.MCP.Servers)),
	}
	for _, srv := range s.Agent.MCP.Servers {
		raw, err := json.Marshal(srv.toWire())
		if err != nil {
			return nil, fmt.Errorf("marshal mcp server %q: %w", srv.Name, err)
		}
		mcpConfig.MCPServers[srv.Name] = raw
	}
	out, err := json.Marshal(mcpConfig)
	if err != nil {
		return nil, fmt.Errorf("marshal mcp config: %w", err)
	}
	return out, nil
}

// mcpserverWire is the JSON shape Claude expects per-server. Untyped so
// the user can extend it (e.g. type fields, trust flags) without breaking
// the Go side.
func (srv MCPServer) toWire() map[string]any {
	wire := map[string]any{}
	if srv.URL != "" {
		wire["type"] = "http"
		wire["url"] = srv.URL
		if len(srv.Headers) > 0 {
			hdrs := make(map[string]string, len(srv.Headers))
			for k, v := range srv.Headers {
				hdrs[k] = v
			}
			wire["headers"] = hdrs
		}
		return wire
	}
	wire["type"] = "stdio"
	wire["command"] = srv.Command
	if len(srv.Args) > 0 {
		wire["args"] = append([]string(nil), srv.Args...)
	}
	if srv.Cwd != "" {
		wire["cwd"] = srv.Cwd
	}
	if len(srv.Env) > 0 {
		env := make(map[string]string, len(srv.Env))
		for k, v := range srv.Env {
			env[k] = v
		}
		wire["env"] = env
	}
	return wire
}

// ToExecOptions converts the YAML document into an backend.ExecOptions
// ready for backend.New(backend, cfg).Execute(ctx, prompt, opts).
//
// SystemPrompt is set to the runtime brief (agent.prompt + skills). The
// caller is responsible for passing the full user-visible prompt
// (brief + extraPrompt) as the `prompt` parameter to Execute, e.g. via
// Schema.TaskPrompt. See backend.ExecOptions.SystemPrompt for the routing
// decision.
func (s *Schema) ToExecOptions(extraPrompt, resumeID string) (backend.ExecOptions, error) {
	brief, err := s.RuntimeBrief()
	if err != nil {
		return backend.ExecOptions{}, err
	}

	mcpRaw, err := s.McpConfigJSON()
	if err != nil {
		return backend.ExecOptions{}, err
	}

	var timeout time.Duration
	if s.Agent.Timeout != "" {
		t, err := time.ParseDuration(s.Agent.Timeout)
		if err != nil {
			return backend.ExecOptions{}, fmt.Errorf("schema: agent.timeout %q: %w", s.Agent.Timeout, err)
		}
		timeout = t
	}

	return backend.ExecOptions{
		Cwd:             s.Agent.Cwd,
		Model:           s.Agent.Model,
		SystemPrompt:    brief,
		ResumeSessionID: resumeID,
		ThreadName:      s.Agent.Name,
		MaxTurns:        s.Agent.MaxTurns,
		Timeout:         timeout,
		ThinkingLevel:   s.Agent.Thinking,
		CustomArgs:      s.Agent.Args,
		Env:             s.Agent.Env,
		McpConfig:       mcpRaw,
	}, nil
}
