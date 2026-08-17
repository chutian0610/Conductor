// Package agent is the unified interface for executing prompts against coding
// agents (Claude Code, Codex CLI). Each backend spawns an LLM CLI as a
// subprocess, parses its streaming protocol, and exposes a uniform event
// stream + terminal result to the caller.
//
// Design note: the Backend contract is deliberately small. V1 only carries
// what every backend actually consumes (cwd, prompt, model, args, env, MCP,
// timeout). Provider-specific knobs (resume session ids, service tiers,
// plugin settings, etc.) belong in ExecOptions fields the relevant backend
// reads; absent fields stay absent rather than being pre-declared.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Backend executes a prompt against an LLM CLI.
//
// Execute is non-blocking: it returns a *Session immediately. The caller
// drains Session.Messages for live events and waits on Session.Result for
// the terminal outcome (exactly one value, then the channel closes).
type Backend interface {
	Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error)
}

// ExecOptions configures a single execution.
//
// Zero values are meaningful: Cwd="" runs in the caller's cwd, Model=""
// uses the backend's default, MaxTurns<=0 means unbounded, Timeout==0
// disables the wall-clock bound (only the inactivity watchdog still fires).
type ExecOptions struct {
	// Cwd is the working directory for the spawned CLI.
	Cwd string
	// Model is the model identifier (e.g. "claude-sonnet-4-5"); empty lets
	// the CLI pick its default.
	Model string
	// SystemPrompt is the task brief. Backends that read from a per-workdir
	// context file (claude → CLAUDE.md, codex → AGENTS.md) persist it there
	// via InjectRuntimeConfig and ignore it for the spawned CLI's input.
	// Backends that don't read from disk (e.g. grok, kimi) prepend it to
	// the prompt passed to the CLI, gated by providerNeedsInlineSystemPrompt.
	//
	// The split mirrors multica's daemon/providerNeedsInlineSystemPrompt:
	// the same string can be routed to two places depending on the
	// provider's protocol convention, and this field is the one place
	// every backend reads from.
	SystemPrompt string
	// ThreadName becomes the agent's display name when supported.
	ThreadName string
	// MaxTurns caps the number of agent turns; 0 means unlimited.
	MaxTurns int
	// ResumeSessionID, if non-empty, continues a previous agent session
	// instead of starting fresh. The session id format depends on the
	// backend (claude → session_id from `system` event, codex → thread_id
	// from `thread.started` notification). Backend translation lives in
	// each backend's argv construction; this field is the one place
	// every backend reads from.
	//
	ResumeSessionID string
	// ResumeExpected signals the backend that the caller EXPECTS the
	// resume to succeed. When true, a permanent resume failure
	// (session gone, schema drift, image too large) triggers an
	// automatic fallback to a fresh session with the
	// ResumeContinuityNotice prepended to the prompt. When false,
	// resume failures surface as the run's error.
	//
	// Set this true when the operator (or scheduler) considers the
	// session a soft hint, not a hard requirement. Set it false when
	// the operator specifically needs the original session.
	ResumeExpected bool
	// ResumeContinuityNotice, when non-empty, is prepended to the
	// prompt on the fallback retry so the LLM knows it was meant to
	// continue a prior session and doesn't fabricate false context.
	// The caller typically writes something like
	//   "You were meant to continue session X. Treat this as a
	//    fresh conversation; do not assume any prior context."
	ResumeContinuityNotice string
	// Timeout is the wall-clock bound for the entire execution. Zero
	// disables it (liveness falls back to the inactivity watchdog).
	Timeout time.Duration
	// SemanticInactivityTimeout fires when the stream goes silent mid-run.
	// Zero disables it.
	SemanticInactivityTimeout time.Duration
	// ThinkingLevel is a backend-native effort value (Claude's
	// "low|medium|high|xhigh|max", Codex's "minimal|low|medium|high"). Empty
	// means "use the runtime default".
	ThinkingLevel string
	// CustomArgs is appended verbatim to the backend's CLI invocation. Each
	// backend maintains a blocklist of flags it owns (--model, --output-format,
	// --mcp-config, ...) and refuses to be overridden by user input.
	CustomArgs []string
	// Env is merged into the spawned process environment, overriding any
	// inherited value. Keys must not be empty.
	Env map[string]string
	// McpConfig, if non-nil, is written to a temp file and passed to the CLI
	// via its --mcp-config flag. Empty/nil means the CLI inherits whatever
	// the user has configured locally.
	McpConfig json.RawMessage
}

// Session represents a running agent execution. The Messages channel is
// closed when the agent finishes; the Result channel carries exactly one
// value before closing.
type Session struct {
	Messages <-chan Message
	Result   <-chan Result
}

// MessageType tags the kind of event emitted on Session.Messages.
type MessageType string

const (
	MessageText       MessageType = "text"        // assistant text
	MessageThinking   MessageType = "thinking"    // reasoning block
	MessageToolUse    MessageType = "tool-use"    // tool invocation start
	MessageToolResult MessageType = "tool-result" // tool invocation result
	MessageStatus     MessageType = "status"      // lifecycle ("running", ...)
	MessageError      MessageType = "error"       // error event from the agent
	MessageLog        MessageType = "log"         // structured log line
)

// Message is a single event emitted by the agent. Only the fields relevant
// to Type are populated; everything else is the zero value.
type Message struct {
	Type      MessageType
	Content   string         // text (Text), error message (Error), log message (Log)
	Tool      string         // tool name (ToolUse, ToolResult)
	CallID    string         // tool call id (ToolUse, ToolResult)
	Input     map[string]any // tool input (ToolUse)
	Output    string         // tool output (ToolResult)
	Status    string         // agent status string (Status)
	Level     string         // log level (Log)
	SessionID string         // backend session id (Status), for early resume pinning
}

// TokenUsage tracks per-model token consumption. CacheRead/Write reflect
// Anthropic's prompt-cache semantics; backends that don't report them leave
// the fields at zero.
type TokenUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Result is the terminal outcome of an execution. Exactly one value is sent
// on Session.Result before the channel closes.
type Result struct {
	// Status is one of "completed", "failed", "timeout", "cancelled",
	// "aborted".
	Status     string
	Output     string                // final user-facing output
	Error      string                // populated when Status != "completed"
	DurationMs int64                 // wall-clock duration
	SessionID  string                // backend session id (stable across resumes)
	Usage      map[string]TokenUsage // keyed by model name
}

// Config configures a Backend instance.
type Config struct {
	// ExecutablePath is the path to the CLI binary. Empty means the binary
	// is resolved via $PATH (default for each backend).
	ExecutablePath string
	// Env is merged into the spawned process environment. CustomArgs / MCP
	// file paths etc. are derived from this map when relevant.
	Env map[string]string
	// Logger receives protocol metadata, lifecycle events, and stderr lines.
	// Nil falls back to slog.Default().
	Logger *slog.Logger
}

// SupportedTypes lists the agent types this build of conductor knows how to
// drive. Adding a new backend requires (1) implementing Backend and (2)
// wiring it into New().
var SupportedTypes = []string{
	"claude",
	"codex",
}

// IsSupportedType reports whether agentType is in SupportedTypes.
func IsSupportedType(agentType string) bool {
	for _, t := range SupportedTypes {
		if t == agentType {
			return true
		}
	}
	return false
}

// New returns a Backend for the given agent type.
func New(agentType string, cfg Config) (Backend, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if !IsSupportedType(agentType) {
		return nil, fmt.Errorf("agent: unknown type %q (supported: %v)", agentType, SupportedTypes)
	}
	switch agentType {
	case "claude":
		return &claudeBackend{cfg: cfg}, nil
	case "codex":
		return &codexBackend{cfg: cfg}, nil
	default:
		// IsSupportedType already filtered this; unreachable in practice.
		return nil, errors.New("agent: unhandled type")
	}
}

// runContext derives the execution context for a subprocess from the
// per-run timeout. Zero (or negative) timeout imposes no deadline —
// liveness is left entirely to the inactivity watchdog.
func runContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}
