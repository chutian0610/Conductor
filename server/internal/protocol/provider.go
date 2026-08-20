package protocol

// AgentProvider identifies which agent runtime is being used.
//
// Phase 1 supports only "codex" (the OpenAI Codex CLI / app-server).
// Multi-provider support is dropped in v0.13 because:
//
//   - Codex covers multi-model needs via OpenAI-compatible
//     [model_providers.<id>] blocks in ~/.codex/config.toml
//     (Mistral / Ollama / OpenRouter / LiteLLM / custom proxy).
//   - A second provider integration would roughly double Phase 1
//     maintenance for marginal gain.
type AgentProvider string

const (
	ProviderCodex AgentProvider = "codex"
)

// AgentSessionConfig is the static configuration handed to an
// AgentClient when creating a session. See §4 of docs/design.md.
type AgentSessionConfig struct {
	// Provider is the runtime identifier ("codex" today).
	Provider AgentProvider `json:"provider"`

	// Model identifies the model behind the provider, in the
	// provider-native format. For Codex this is the model id the
	// Codex app-server expects (e.g. "gpt-5", "claude-opus-4-6" when
	// routing through OpenRouter).
	Model string `json:"model"`

	// Cwd is the working directory for the session. Per-spec HOME
	// isolation lives under the SpecRecord.HomePath, not here.
	Cwd string `json:"cwd"`

	// SystemPrompt is appended to the provider's default system
	// prompt. Empty means "use provider default".
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// Thinking controls reasoning effort. Empty means provider default.
	// Codex accepts "minimal" / "low" / "medium" / "high".
	Thinking string `json:"thinking,omitempty"`

	// ToolsAllow / ToolsExclude are forwarded to Codex as
	// --tools / --exclude-tools. Empty means provider default.
	ToolsAllow   []string `json:"toolsAllow,omitempty"`
	ToolsExclude []string `json:"toolsExclude,omitempty"`

	// MCPConfig is an optional path to an MCP config JSON file.
	// Empty means "no MCP servers attached to this session".
	MCPConfig string `json:"mcpConfig,omitempty"`

	// SessionId optionally pre-seeds the Codex session id; empty
	// means "let Codex generate a new id". When resuming, set this
	// to the previously-stored id (see AgentPersistenceHandle).
	SessionId string `json:"sessionId,omitempty"`
}

// AgentStreamEvent is a single event streamed from a session. The
// concrete provider maps its native event format into this union.
// Provider-native variants that don't fit (e.g. tool approval
// requests) are modelled as dedicated kinds rather than loosely-typed
// payloads.
//
// v0.13 deliberately omits per-provider detail; if we later add a
// second provider we'd extend this with provider-specific event
// variants.
type AgentStreamEvent struct {
	// Kind discriminates the union. Empty kind means the event
	// couldn't be classified (caller should log and skip).
	Kind AgentStreamEventKind `json:"kind"`

	// Text event fields.
	Text string `json:"text,omitempty"`

	// ToolCall event fields.
	ToolName   string         `json:"toolName,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	ToolArgs   map[string]any `json:"toolArgs,omitempty"`

	// ToolResult event fields.
	ToolResult string `json:"toolResult,omitempty"`
	ToolError  string `json:"toolError,omitempty"`

	// Finish event fields.
	FinishReason string `json:"finishReason,omitempty"`

	// Error event fields.
	ErrorCode    string `json:"errorCode,omitempty,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty,omitempty"`
}

type AgentStreamEventKind string

const (
	EventText       AgentStreamEventKind = "text"
	EventToolCall   AgentStreamEventKind = "tool_call"
	EventToolResult AgentStreamEventKind = "tool_result"
	EventPermission AgentStreamEventKind = "permission_request"
	EventFinish     AgentStreamEventKind = "finish"
	EventError      AgentStreamEventKind = "error"
)

// AgentPersistenceHandle identifies a persisted session that can be
// resumed via AgentClient.ResumeSession. For Codex this wraps the
// session id and the directory holding the session JSONL.
type AgentPersistenceHandle struct {
	Provider  AgentProvider `json:"provider"`
	SessionId string        `json:"sessionId"`
	// SessionDir is where the Codex session JSONL lives. For Conductor
	// this is typically the per-spec HOME's .codex/sessions/ dir.
	SessionDir string `json:"sessionDir"`
}

// AgentTurnResult is returned by AgentSession.Send after a single
// turn completes (or fails). Streaming events are received via
// AgentSession.Events(); this struct is the final summary.
type AgentTurnResult struct {
	SessionID string             `json:"sessionId"`
	Usage     AgentUsage          `json:"usage"`
	Finish    AgentTurnFinish     `json:"finish"`
}

type AgentUsage struct {
	// CostUSD is the approximate USD cost reported by the provider.
	// Zero if the provider doesn't report it.
	CostUSD float64 `json:"costUsd"`
	// InputTokens / OutputTokens are the raw token counts.
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

type AgentTurnFinish struct {
	Reason  string `json:"reason"`  // "end_turn", "max_tokens", "tool_error", ...
	Success bool   `json:"success"`
}
