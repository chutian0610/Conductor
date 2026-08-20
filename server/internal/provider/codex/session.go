package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"conductor/server/internal/protocol"
)

// Session is the high-level wrapper over Client that turns
// Codex-specific thread/turn RPCs into protocol.AgentStreamEvent
// streams. One Session per agent invocation.
//
// Lifecycle:
//
//	sess, err := codex.NewSession(ctx, codex.SessionConfig{...})
//	defer sess.Close()
//	for ev := range sess.Events() { ... }
//	result, err := sess.Send(ctx, prompt)
//	sess.Close()
//
// Send blocks until either:
//   - turn/completed notification arrives → result populated
//   - ctx is canceled → ctx.Err() returned
//   - subprocess exits unexpectedly → error returned
//
// During a Send, intermediate notifications (item/*) are pushed
// onto Events() in arrival order; the final turn/completed is NOT
// pushed as a separate event — its payload becomes Send's return.
//
// See §5.1 of docs/design.md for the Session shape this implements.
type Session struct {
	client *Client

	events     chan protocol.AgentStreamEvent
	closeOnce  sync.Once
	closeErr   error
	pumpDone   chan struct{} // closed when pumpNotifications exits
	done       chan struct{} // closed when Session is fully closed
	cancelPump context.CancelFunc
	mu         sync.Mutex
	closed     bool
	threadID   string
	// turnWaiter receives the AgentTurnResult when a turn/completed
	// notification arrives. Set by Send; cleared on Send return.
	// Held under mu.
	turnWaiter chan *protocol.AgentTurnResult
}

// SessionConfig builds a Session. Fields are flat (not embedded)
// because both ClientConfig and protocol.AgentSessionConfig define
// Cwd — Go's field-promotion rules forbid embedding both.
type SessionConfig struct {
	// Subprocess fields (forwarded to ClientConfig).
	Bin  string   // "codex" default
	Args []string // extra flags before "app-server"
	Home string   // per-spec HOME (§6.2.5); required
	Cwd  string   // subprocess working directory
	Env  []string // appended to os.Environ() in the subprocess

	// Agent session fields (forwarded as thread/* params).
	Model        string
	SystemPrompt string
	Thinking     string   // "minimal" | "low" | "medium" | "high"
	ToolsAllow   []string
	ToolsExclude []string
	MCPConfig    string

	// SessionId, if non-empty, switches NewSession to thread/resume
	// instead of thread/start (so a previously-stored session can be
	// replayed). See protocol.AgentPersistenceHandle.
	SessionId string
}

// NewSession spawns the codex app-server subprocess and performs
// the thread handshake:
//
//   - SessionId == "" → thread/start
//   - SessionId != "" → thread/resume
//
// The returned Session is ready to Send(). Close() MUST be called
// to release the subprocess.
func NewSession(ctx context.Context, cfg SessionConfig) (*Session, error) {
	client, err := NewClient(ctx, ClientConfig{
		Bin:  cfg.Bin,
		Args: cfg.Args,
		Home: cfg.Home,
		Cwd:  cfg.Cwd,
		Env:  cfg.Env,
	})
	if err != nil {
		return nil, err
	}

	pumpCtx, cancelPump := context.WithCancel(context.Background())

	s := &Session{
		client:     client,
		events:     make(chan protocol.AgentStreamEvent, 64),
		pumpDone:   make(chan struct{}),
		done:       make(chan struct{}),
		cancelPump: cancelPump,
	}

	method := "thread/start"
	params := map[string]any{
		"model":        cfg.Model,
		"systemPrompt": cfg.SystemPrompt,
		"thinking":     cfg.Thinking,
		"toolsAllow":   cfg.ToolsAllow,
		"toolsExclude": cfg.ToolsExclude,
		"cwd":          cfg.Cwd,
	}
	if cfg.MCPConfig != "" {
		params["mcpConfig"] = cfg.MCPConfig
	}
	if cfg.SessionId != "" {
		method = "thread/resume"
		params["sessionId"] = cfg.SessionId
	}

	var result struct {
		ThreadID string `json:"threadId"`
	}
	if err := client.Call(ctx, method, params, &result); err != nil {
		cancelPump()
		_ = client.Close()
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	s.threadID = result.ThreadID

	go s.pumpNotifications(pumpCtx)
	return s, nil
}

// Events returns the typed event stream. Closes when the Session
// is fully closed (via Close, subprocess exit, or unrecoverable
// pump failure).
func (s *Session) Events() <-chan protocol.AgentStreamEvent { return s.events }

// Done returns a channel closed when the Session is fully closed.
func (s *Session) Done() <-chan struct{} { return s.done }

// ThreadID returns the id assigned by thread/start (or thread/resume).
// Empty only if NewSession failed.
func (s *Session) ThreadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

// Send submits a prompt as a new turn and blocks until the
// matching turn/completed notification arrives (or ctx is canceled,
// or the subprocess exits).
//
// Send is single-flight per Session: concurrent Sends will race on
// the turnWaiter slot. Callers that need concurrent turns should
// spawn multiple Sessions.
func (s *Session) Send(ctx context.Context, prompt string) (*protocol.AgentTurnResult, error) {
	turnCh := make(chan *protocol.AgentTurnResult, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("session closed")
	}
	if s.turnWaiter != nil {
		s.mu.Unlock()
		return nil, errors.New("another Send is already in flight")
	}
	s.turnWaiter = turnCh
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.turnWaiter = nil
		s.mu.Unlock()
	}()

	params := map[string]any{
		"threadId": s.threadID,
		"prompt":   prompt,
	}
	if err := s.client.Call(ctx, "turn/start", params, nil); err != nil {
		return nil, fmt.Errorf("turn/start: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-turnCh:
		return result, nil
	case <-s.done:
		return nil, errors.New("codex session exited before turn/completed")
	}
}

// Cancel asks Codex to stop gracefully via turn/interrupt, then
// closes the Session if the subprocess hasn't exited within a
// short grace window. Cancel itself does not block on the close.
func (s *Session) Cancel(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	threadID := s.threadID
	s.mu.Unlock()

	if threadID != "" {
		// Best-effort: if the subprocess is already gone, this errors
		// and we fall through to Close().
		_ = s.client.Call(ctx, "turn/interrupt",
			map[string]any{"threadId": threadID}, nil)
	}

	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
	return s.Close()
}

// Close tears down the Session and the underlying Client. Safe to
// call multiple times. Blocks until the pump goroutine has exited.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.cancelPump()
		s.closeErr = s.client.Close()
		<-s.pumpDone
		close(s.events)
		close(s.done)
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	})
	return s.closeErr
}

// pumpNotifications consumes Client.Events(), maps each notification
// into a protocol.AgentStreamEvent, and:
//
//   - forwards text/tool/permission events to Events()
//   - routes turn/completed payload to the active Send() waiter
//     (instead of pushing it as a stream event)
//   - skips unknown / malformed notifications silently
//
// Phase 2 should add a logger hook here; for now, drops are silent.
func (s *Session) pumpNotifications(ctx context.Context) {
	defer close(s.pumpDone)
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-s.client.Events():
			if !ok {
				return
			}
			mapped := mapNotification(raw)
			if mapped.completion != nil {
				s.mu.Lock()
				waiter := s.turnWaiter
				s.mu.Unlock()
				if waiter != nil {
					select {
					case waiter <- mapped.completion:
					default:
						// No active Send; drop the completion.
						// (This shouldn't happen in normal use, but
						// is defensible if Send already returned.)
					}
				}
				continue
			}
			if mapped.event.Kind == "" {
				continue
			}
			select {
			case s.events <- mapped.event:
			case <-ctx.Done():
				return
			}
		}
	}
}

// mappedNotification is the result of mapNotification. At most one
// of event or completion is non-zero:
//
//   - completion != nil → this is a turn/completed; deliver to the
//     active Send() waiter instead of pushing event to Events().
//   - completion == nil && event.Kind != "" → push event to Events().
//   - completion == nil && event.Kind == "" → unknown; skip.
type mappedNotification struct {
	event      protocol.AgentStreamEvent
	completion *protocol.AgentTurnResult
}

// mapNotification is a pure function so the mapping logic can be
// tested without spawning a subprocess. It accepts the raw JSON
// object delivered by Client.Events() and returns either:
//
//   - a typed event (forward to Events())
//   - a completion payload (deliver to Send() waiter)
//   - nothing (unknown notification; skip)
//
// Recognized Codex app-server notification shapes:
//
//	{"method":"item/agentMessage/delta","params":{"text":"..."}}
//	{"method":"item/toolCall","params":{"name":"...","id":"...","arguments":{...}}}
//	{"method":"item/toolResult","params":{"result":"...","error":"..."}}
//	{"method":"item/commandExecution/requestApproval","params":{...}}
//	{"method":"item/fileChange/requestApproval","params":{...}}
//	{"method":"turn/completed","params":{"usage":{...},"finish":{...},"threadId":"..."}}
func mapNotification(raw map[string]any) mappedNotification {
	method, _ := raw["method"].(string)
	params, _ := raw["params"].(map[string]any)

	switch method {
	case "item/agentMessage/delta":
		// Some Codex builds use "text", others "delta". Accept both.
		text, _ := stringField(params, "text", "delta")
		return mappedNotification{event: protocol.AgentStreamEvent{
			Kind: protocol.EventText,
			Text: text,
		}}

	case "item/toolCall":
		name, _ := stringField(params, "name", "toolName")
		id, _ := stringField(params, "id", "toolCallId")
		var args map[string]any
		if a, ok := params["arguments"].(map[string]any); ok {
			args = a
		}
		return mappedNotification{event: protocol.AgentStreamEvent{
			Kind:       protocol.EventToolCall,
			ToolName:   name,
			ToolCallID: id,
			ToolArgs:   args,
		}}

	case "item/toolResult":
		result, _ := stringField(params, "result", "output")
		errmsg, _ := stringField(params, "error", "errorMessage")
		return mappedNotification{event: protocol.AgentStreamEvent{
			Kind:       protocol.EventToolResult,
			ToolResult: result,
			ToolError:  errmsg,
		}}

	case "item/commandExecution/requestApproval",
		"item/fileChange/requestApproval":
		// Surface as permission_request so the runner can decide
		// whether to auto-approve or block. Detail payload is
		// intentionally dropped (Phase 2 may surface it).
		return mappedNotification{event: protocol.AgentStreamEvent{
			Kind: protocol.EventPermission,
		}}

	case "turn/completed":
		return mappedNotification{completion: extractTurnResult(params)}
	}

	// Unknown / no method. Caller skips silently.
	return mappedNotification{}
}

// extractTurnResult builds AgentTurnResult from a turn/completed
// notification's params. Defensive against partial / future shapes.
func extractTurnResult(params map[string]any) *protocol.AgentTurnResult {
	r := &protocol.AgentTurnResult{}
	if usage, ok := params["usage"].(map[string]any); ok {
		if n, ok := numberField(usage, "inputTokens"); ok {
			r.Usage.InputTokens = int(n)
		}
		if n, ok := numberField(usage, "outputTokens"); ok {
			r.Usage.OutputTokens = int(n)
		}
		if f, ok := numberField(usage, "costUsd", "costUSD"); ok {
			r.Usage.CostUSD = f
		}
	}
	if finish, ok := params["finish"].(map[string]any); ok {
		if s, ok := finish["reason"].(string); ok {
			r.Finish.Reason = s
		}
		if b, ok := finish["success"].(bool); ok {
			r.Finish.Success = b
		}
	}
	if s, ok := stringField(params, "threadId"); ok {
		r.SessionID = s
	}
	return r
}

// stringField returns params[name] as a string if present. With
// multiple names, the first that resolves wins (used to be tolerant
// of naming variants).
func stringField(params map[string]any, names ...string) (string, bool) {
	if params == nil {
		return "", false
	}
	for _, n := range names {
		if v, ok := params[n].(string); ok {
			return v, true
		}
	}
	return "", false
}

// numberField returns params[name] as float64 if present. JSON
// numbers always decode as float64 in Go's map[string]any; we also
// accept json.Number for callers that decode with UseNumber.
func numberField(params map[string]any, names ...string) (float64, bool) {
	if params == nil {
		return 0, false
	}
	for _, n := range names {
		if v, ok := params[n].(float64); ok {
			return v, true
		}
		if v, ok := params[n].(json.Number); ok {
			if f, err := v.Float64(); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}
