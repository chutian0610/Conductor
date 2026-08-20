package runner

import (
	"context"
	"fmt"

	"conductor/server/internal/provider/codex"
	"conductor/server/internal/protocol"
	"conductor/server/internal/spec"
)

// EventHandler is called once per AgentStreamEvent as it arrives
// from the codex Session. Runs in a dedicated goroutine; the handler
// must be safe for concurrent invocation and must NOT block on
// anything that would backpressure the pump goroutine (writes to
// the events channel).
//
// Pass nil to disable the callback — events are still consumed
// internally so the Session's pump doesn't block, but nothing is
// surfaced to the caller.
type EventHandler func(ev protocol.AgentStreamEvent)

// Invoke runs a single prompt against a registered spec. The
// session is opened against the spec's per-spec HOME (so codex
// picks up its config.toml + auth symlink), the prompt is sent,
// and the call blocks until turn/completed arrives.
//
// Streaming events are delivered via onEvent (if non-nil). The
// final AgentTurnResult (token usage, finish reason, session id)
// is returned.
//
// Errors:
//   - spec.ErrNotFound: unknown specId
//   - codex.NewSession failure (HOME missing, binary missing, ...)
//   - turn/start or turn/completed failure
//   - ctx cancellation
//
// Phase 1 doesn't yet persist anything to disk; if the caller
// needs a timeline, it must capture events via onEvent.
func Invoke(ctx context.Context, specId, prompt string, onEvent EventHandler) (*protocol.AgentTurnResult, error) {
	if specId == "" {
		return nil, fmt.Errorf("runner: specId required")
	}
	if prompt == "" {
		return nil, fmt.Errorf("runner: prompt required")
	}

	record, err := spec.Get(ctx, specId)
	if err != nil {
		return nil, err
	}

	sess, err := codex.NewSession(ctx, codex.SessionConfig{
		Home:          record.HomePath,
		Cwd:           record.Spec.Cwd,
		Model:         record.Spec.Model,
		SystemPrompt:  record.Spec.SystemPrompt,
		Thinking:      record.Spec.Thinking,
		ToolsAllow:    record.Spec.ToolsAllow,
		ToolsExclude:  record.Spec.ToolsExclude,
		MCPConfig:     record.Spec.MCPConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("open codex session: %w", err)
	}

	// handlerDone is closed when the onEvent goroutine exits.
	// We MUST wait on it before returning so callers can rely on
	// all events being processed (the CLI uses this to print the
	// final result summary without racing the handler for the
	// output writer).
	handlerDone := make(chan struct{})
	if onEvent != nil {
		go func() {
			defer close(handlerDone)
			for ev := range sess.Events() {
				onEvent(ev)
			}
		}()
	} else {
		close(handlerDone)
	}

	result, err := sess.Send(ctx, prompt)

	// Close the session BEFORE waiting on the handler. Close()
	// terminates the events channel, which lets the handler
	// goroutine exit, which closes handlerDone. Order matters:
	// Close -> handler exits -> done closes.
	sess.Close()
	<-handlerDone

	return result, err
}

func InvokeWithSessionId(ctx context.Context, specId, sessionId, prompt string, onEvent EventHandler) (*protocol.AgentTurnResult, error) {
	if specId == "" {
		return nil, fmt.Errorf("runner: specId required")
	}
	if prompt == "" {
		return nil, fmt.Errorf("runner: prompt required")
	}
	if sessionId == "" {
		return nil, fmt.Errorf("runner: sessionId required")
	}

	record, err := spec.Get(ctx, specId)
	if err != nil {
		return nil, err
	}

	sess, err := codex.NewSession(ctx, codex.SessionConfig{
		Home:          record.HomePath,
		Cwd:           record.Spec.Cwd,
		Model:         record.Spec.Model,
		SystemPrompt:  record.Spec.SystemPrompt,
		Thinking:      record.Spec.Thinking,
		ToolsAllow:    record.Spec.ToolsAllow,
		ToolsExclude:  record.Spec.ToolsExclude,
		MCPConfig:     record.Spec.MCPConfig,
		SessionId:     sessionId, // routes to thread/resume
	})
	if err != nil {
		return nil, fmt.Errorf("open codex session: %w", err)
	}
	defer sess.Close()

	if onEvent != nil {
		go func() {
			for ev := range sess.Events() {
				onEvent(ev)
			}
		}()
	}

	return sess.Send(ctx, prompt)
}
