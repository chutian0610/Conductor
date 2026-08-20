package runner

import (
	"context"
	"fmt"
	"time"

	"conductor/server/internal/provider/codex"
	"conductor/server/internal/protocol"
	"conductor/server/internal/spec"
	"conductor/server/internal/storage"
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
// store records the run. Pass storage.NoopStorage{} to skip
// persistence. The store is updated on three events:
//
//   1. start: CreateRun (Status=running, StartedAt=now)
//   2. each stream event: AppendTimeline
//   3. end: UpdateRun (Status=completed + Result OR Status=failed + Error)
//
// Streaming events are also delivered via onEvent (if non-nil)
// for callers that want to print them as they arrive (the CLI
// uses this). Events are persisted via the store regardless of
// whether onEvent is set.
//
// Errors:
//   - spec.ErrNotFound: unknown specId
//   - storage: failed to record run start (before any codex work)
//   - codex.NewSession failure (HOME missing, binary missing, ...)
//   - turn/start or turn/completed failure
//   - ctx cancellation
func Invoke(ctx context.Context, specID, prompt string, runID string, store storage.Storage, onEvent EventHandler) (*protocol.AgentTurnResult, error) {
	if specID == "" {
		return nil, fmt.Errorf("runner: specID required")
	}
	if prompt == "" {
		return nil, fmt.Errorf("runner: prompt required")
	}
	if runID == "" {
		return nil, fmt.Errorf("runner: runID required")
	}

	record, err := spec.Get(ctx, specID)
	if err != nil {
		return nil, err
	}

	// Record run start. Failure here is fatal — the user wants
	// history, not silent drops.
	state, err := store.CreateRun(ctx, runID, specID, prompt)
	if err != nil {
		return nil, fmt.Errorf("storage CreateRun: %w", err)
	}
	_ = state // currently unused; reserved for future spec metadata snapshots

	sess, err := codex.NewSession(ctx, codex.SessionConfig{
		// Per-spec HOME (so codex reads its config.toml + .codex.json
		// from the right place — §6.2.5).
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
		_ = markFailed(ctx, store, runID, err)
		return nil, fmt.Errorf("open codex session: %w", err)
	}

	// Tee each event to both the caller's handler AND the storage
	// timeline. Done in a single goroutine (per pump-tap) so the
	// ordering is preserved across both sinks.
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		for ev := range sess.Events() {
			_ = store.AppendTimeline(ctx, runID, storage.TimelineItem{
				TS:    timeNow(),
				Event: ev,
			})
			if onEvent != nil {
				onEvent(ev)
			}
		}
	}()

	result, err := sess.Send(ctx, prompt)

	// Close session, then wait for handler to drain remaining events.
	sess.Close()
	<-handlerDone

	if err != nil {
		_ = markFailed(ctx, store, runID, err)
		return nil, err
	}

	// Persist the final state under the per-runID mutex (so a
	// concurrent reader can't see partial fields).
	if result != nil {
		finish := timeNow()
		err = store.UpdateRun(ctx, runID, func(rs *storage.RunState) {
			rs.Status = storage.RunStatusCompleted
			rs.FinishedAt = &finish
			rs.SessionID = result.SessionID
			rs.Result = result
		})
	} else {
		_ = markFailed(ctx, store, runID, fmt.Errorf("nil result"))
	}
	if err != nil {
		return result, fmt.Errorf("storage UpdateRun: %w", err)
	}
	return result, nil
}

// InvokeWithSessionId is like Invoke but resumes an existing
// Codex thread instead of starting a new one. sessionId is the
// thread id previously returned from turn/completed (see
// AgentTurnResult.SessionID).
//
// Phase 1 keeps this separate so the runner's "start fresh" path
// stays obvious.
func InvokeWithSessionId(ctx context.Context, specID, sessionID, prompt, runID string, store storage.Storage, onEvent EventHandler) (*protocol.AgentTurnResult, error) {
	if specID == "" {
		return nil, fmt.Errorf("runner: specID required")
	}
	if prompt == "" {
		return nil, fmt.Errorf("runner: prompt required")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("runner: sessionID required")
	}
	if runID == "" {
		return nil, fmt.Errorf("runner: runID required")
	}

	record, err := spec.Get(ctx, specID)
	if err != nil {
		return nil, err
	}

	if _, err := store.CreateRun(ctx, runID, specID, prompt); err != nil {
		return nil, fmt.Errorf("storage CreateRun: %w", err)
	}

	sess, err := codex.NewSession(ctx, codex.SessionConfig{
		Home:         record.HomePath,
		Cwd:          record.Spec.Cwd,
		Model:        record.Spec.Model,
		SystemPrompt: record.Spec.SystemPrompt,
		Thinking:     record.Spec.Thinking,
		ToolsAllow:   record.Spec.ToolsAllow,
		ToolsExclude: record.Spec.ToolsExclude,
		MCPConfig:    record.Spec.MCPConfig,
		SessionId:    sessionID, // routes to thread/resume
	})
	if err != nil {
		_ = markFailed(ctx, store, runID, err)
		return nil, fmt.Errorf("open codex session: %w", err)
	}

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		for ev := range sess.Events() {
			_ = store.AppendTimeline(ctx, runID, storage.TimelineItem{
				TS:    timeNow(),
				Event: ev,
			})
			if onEvent != nil {
				onEvent(ev)
			}
		}
	}()

	result, err := sess.Send(ctx, prompt)
	sess.Close()
	<-handlerDone

	if err != nil {
		_ = markFailed(ctx, store, runID, err)
		return nil, err
	}

	if result != nil {
		finish := timeNow()
		err = store.UpdateRun(ctx, runID, func(rs *storage.RunState) {
			rs.Status = storage.RunStatusCompleted
			rs.FinishedAt = &finish
			rs.SessionID = result.SessionID
			rs.Result = result
		})
	} else {
		_ = markFailed(ctx, store, runID, fmt.Errorf("nil result"))
	}
	if err != nil {
		return result, fmt.Errorf("storage UpdateRun: %w", err)
	}
	return result, nil
}

// markFailed transitions a run to status=failed with the given
// error message. Best-effort — caller doesn't act on the result.
func markFailed(ctx context.Context, store storage.Storage, runID string, err error) error {
	msg := err.Error()
	return store.UpdateRun(ctx, runID, func(rs *storage.RunState) {
		finish := timeNow()
		rs.Status = storage.RunStatusFailed
		rs.FinishedAt = &finish
		rs.Error = msg
	})
}

// timeNow returns the current UTC time. Stubbed as a var so
// tests can pin timestamps.
var timeNow = func() time.Time { return time.Now().UTC() }
