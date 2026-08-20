package protocol

import "time"

// AgentSpec is the user-defined, static description of an agent
// invocation. It captures everything that determines "what kind
// of agent is this" — provider, model, skill list, MCP config, worktree,
// etc. — without runtime parameters like the prompt.
//
// The spec is what Conductor's per-spec HOME is keyed on (§6.2.5 of
// docs/design.md). Two AgentSpecs that hash to the same SpecId are
// considered the same agent for HOME-sharing purposes.
//
// SpecId is derived from the user-given name (preferred) plus a
// content hash for disambiguation. See server/internal/spec for the
// exact derivation.
type AgentSpec struct {
	// SpecId is the canonical identifier. Set by the spec package.
	SpecId string `json:"specId"`

	// Name is the user-given human-readable label. Optional; if empty
	// the spec is still addressable by SpecId.
	Name string `json:"name,omitempty"`

	// Provider / Model are the agent runtime choices.
	Provider AgentProvider `json:"provider"`
	Model    string        `json:"model"`

	// Skills is a list of skill names (resolved via the spec's HOME
	// .codex/skills/ directory at runtime).
	Skills []string `json:"skills,omitempty"`

	// MCPConfig is a path to an MCP config JSON, or "" to disable MCP.
	MCPConfig string `json:"mcpConfig,omitempty"`

	// SystemPrompt is appended to the provider default.
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// Thinking / ToolsAllow / ToolsExclude forwarded to Codex as-is.
	Thinking     string   `json:"thinking,omitempty"`
	ToolsAllow   []string `json:"toolsAllow,omitempty"`
	ToolsExclude []string `json:"toolsExclude,omitempty"`

	// Cwd is the default working directory. May be overridden by a
	// worktree at invoke time.
	Cwd string `json:"cwd,omitempty"`

	// Worktree, if non-nil, requests a git-worktree isolated workspace
	// per spec invocation.
	Worktree *WorktreeSpec `json:"worktree,omitempty"`

	// CreatedAt is when the spec was first registered. Filled by the
	// spec package; zero-value means "unset".
	CreatedAt time.Time `json:"createdAt,omitempty"`

	// UpdatedAt is when the spec was last modified. Filled by the
	// spec package; zero-value means "same as CreatedAt".
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// WorktreeSpec requests an isolated git worktree when invoking a spec.
type WorktreeSpec struct {
	// Branch is the new branch name (required when Mode is BranchOff).
	Branch string `json:"branch,omitempty"`

	// BaseBranch is the commit / branch to fork from when Mode is
	// BranchOff. Empty means "current HEAD".
	BaseBranch string `json:"baseBranch,omitempty"`

	// Mode is one of "branch_off" (default), "checkout_branch",
	// "checkout_pr".
	Mode string `json:"mode,omitempty"`
}

// SpecRecord is what the spec package persists: the spec itself plus
// the resolved HomePath. AgentSpec is content-addressable; SpecRecord
// is the on-disk shape that carries the per-spec HOME location.
type SpecRecord struct {
	Spec     AgentSpec `json:"spec"`
	HomePath string    `json:"homePath"` // absolute path to specs/<specId>/home/

	// ConfigToml is the path inside HomePath where the Codex config
	// was written. Typically ".codex/config.toml".
	ConfigToml string `json:"configToml"`
}
