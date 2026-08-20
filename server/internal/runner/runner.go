package runner

import (
	"context"
	"os"
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
func Invoke(ctx context.Context, specID, prompt string, runID string, store storage.Storage, onEvent EventHandler, sigCh <-chan os.Signal) (*protocol.AgentTurnResult, error) {
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

	// Record the host process id so `conductor cancel <runId>` can
	// find us. Best-effort: uses context.Background() so the PID
	// record survives even if the CLI's signal.NotifyContext has
	// canceled the invCtx.
	if err := store.UpdateRun(context.Background(), runID, func(rs *storage.RunState) {
		rs.PID = os.Getpid()
	}); err != nil {
		// Non-fatal — cancel will refuse gracefully if PID == 0.
		fmt.Fprintf(os.Stderr, "warn: record pid: %s\n", err)
	}

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

	// If the caller passed a signal channel, install a watcher that
	// marks the run cancelled + asks codex to stop on first signal.
	// The watcher mutates state.json so markFailed's idempotency
	// check sees the transition and doesn't overwrite back to failed.
	if sigCh != nil {
		invokeDone := make(chan struct{})
		defer close(invokeDone)
		go watchCancelSignal(ctx, sigCh, invokeDone, store, runID, sess)
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
func InvokeWithSessionId(ctx context.Context, specID, sessionID, prompt, runID string, store storage.Storage, onEvent EventHandler, sigCh <-chan os.Signal) (*protocol.AgentTurnResult, error) {
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

	if sigCh != nil {
		invokeDone := make(chan struct{})
		defer close(invokeDone)
		go watchCancelSignal(ctx, sigCh, invokeDone, store, runID, sess)
	}

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
// Idempotent wrt cancellation: if the user already triggered a
// cancel (status=cancelled), we don't overwrite that with failed.
//
// Uses context.Background() so the storage update can complete
// even when the Invoke ctx is canceled (the CLI's
// signal.NotifyContext cancels invCtx on SIGTERM; the markFailed
// that follows Send's ctx.Err return must still be able to write
// state.json).
func markFailed(_ context.Context, store storage.Storage, runID string, err error) error {
	msg := err.Error()
	return store.UpdateRun(context.Background(), runID, func(rs *storage.RunState) {
		if rs.Status == storage.RunStatusCancelled {
			return // user already cancelled; don't overwrite
		}
		finish := timeNow()
		rs.Status = storage.RunStatusFailed
		rs.FinishedAt = &finish
		rs.Error = msg
	})
}

// timeNow returns the current UTC time. Stubbed as a var so
// tests can pin timestamps.
var timeNow = func() time.Time { return time.Now().UTC() }


// watchCancelSignal is the cancel hook installed when Invoke is
// called with a non-nil sigCh. On the first signal it:
//   1. Marks the run as cancelled in storage (so markFailed in the
//      main flow doesn't overwrite back to failed).
//   2. Calls sess.Cancel to ask codex to stop (sends turn/interrupt
//      or, if that doesn't get a response in 2s, falls through to
//      Close which sends SIGTERM to the codex subprocess).
//
// We close cancelDone when the signal is processed so the deferred
// <-cancelDone in Invoke returns (it just waits for the watcher to
// finish its work, not for Send to return).
func watchCancelSignal(invCtx context.Context, sigCh <-chan os.Signal, invokeDone <-chan struct{}, store storage.Storage, runID string, sess *codex.Session) {
	// invCtx is the Invoke ctx, which main.go's signal.NotifyContext
	// may have already canceled by the time the signal fires. Use a
	// fresh ctx for the storage update + sess.Cancel so the cancel
	// path itself can't fail because of the cancellation.
	ctx := context.Background()
	select {
	case sig, ok := <-sigCh:
		if !ok {
			return
		}
	_ = store.UpdateRun(ctx, runID, func(rs *storage.RunState) {
			if rs.Status == storage.RunStatusRunning {
				rs.Status = storage.RunStatusCancelled
				now := timeNow()
				rs.FinishedAt = &now
				rs.Error = "cancelled by signal: " + sig.String()
			}
		})
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sess.Cancel(cancelCtx)
	case <-invokeDone:
		return // normal completion; nothing to do
	case <-ctx.Done():
		return
	}
}
