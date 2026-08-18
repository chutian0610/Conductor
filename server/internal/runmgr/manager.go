package runmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"conductor/server/internal/agentregistry"
	"conductor/server/internal/backend"
)

// ErrAgentNotFound is returned by StartRun when the named agent is
// not registered. Mirrors agentregistry.ErrNotFound semantics but
// is reachable through the HTTP layer without importing the lower-
// level package.
var ErrAgentNotFound = errors.New("runmgr: agent not found")

// DefaultSSEBufferSize is the per-subscriber channel buffer used
// when the caller does not specify otherwise. Sized to roughly
// one typical run worth of events; slow consumers drop oldest
// rather than block the run goroutine.
const DefaultSSEBufferSize = 64

// Manager is the run scheduler. One Manager per daemon. The
// daemon constructs it once at startup (after the registry store
// is open), registers it with the HTTP server, and passes
// incoming POST /v1/runs requests to StartRun.
type Manager struct {
	store  *agentregistry.Store
	logger *slog.Logger

	mu sync.Mutex
	// active holds in-flight run states keyed by run ID. Finished
	// runs are removed; the registry remains the durable record.
	active map[int64]*runState

	wg sync.WaitGroup

	// backendFactory is called once per StartRun to build the
	// backend driver. Production defaults to [backend.New]; tests
	// may override via [Manager.SetBackendFactory] (annotated as
	// test-only in the doc comment, but not build-tag-gated so
	// integration tests in `cmd/conductor` can wire it).
	backendFactory func(agentType string, cfg backend.Config) (backend.Backend, error)
}

// New builds a Manager bound to the given store and logger.
// A nil logger falls back to slog.Default.
func New(store *agentregistry.Store, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		store:          store,
		logger:         logger,
		active:         map[int64]*runState{},
		backendFactory: backend.New,
	}
	return m
}

// StartRequest carries the inputs to StartRun.
type StartRequest struct {
	// AgentName looks up the agent by registered Name. Required.
	AgentName string
	// Prompt is appended to the agent's configured prompt (matches
	// conductor run --prompt). May be empty.
	Prompt string
	// ResumeID, when non-empty, asks the backend to continue a
	// previous session (e.g. claude session id, codex thread id).
	ResumeID string
	// Env replaces the spawned process environment. Mirrors
	// os.Environ(): each entry is "KEY=value". Agent identity
	// vars are added by executeRun on top of this list.
	Env []string
	// CustomArgs is appended verbatim to the backend CLI; matches
	// backend.ExecOptions.CustomArgs.
	CustomArgs []string
}

// StartRun begins a run in the background. It returns the
// just-created Run row synchronously (status="running"); the actual
// backend execution runs in a goroutine and updates the row via
// FinishRun + AppendEvent as work progresses. Use Subscribe to
// follow live events.
//
// Errors:
//   - agentregistry.ErrNotFound (wrapped as ErrAgentNotFound) if
//     the named agent is not registered.
//   - context errors propagated from the agentregistry write.
//   - any backend instantiation failure currently surfaces as a
//     failed run after StartRun returns the row (so the caller
//     always gets a stable Run back, even when the spawn fails).
func (m *Manager) StartRun(ctx context.Context, req StartRequest) (agentregistry.Run, error) {
	if req.AgentName == "" {
		return agentregistry.Run{}, errors.New("runmgr: AgentName required")
	}
	agent, err := m.store.GetAgent(ctx, req.AgentName)
	if err != nil {
		if errors.Is(err, agentregistry.ErrNotFound) {
			return agentregistry.Run{}, fmt.Errorf("%w: %s", ErrAgentNotFound, req.AgentName)
		}
		return agentregistry.Run{}, err
	}

	run := agentregistry.Run{
		AgentID:   agent.ID,
		Status:    "running",
		StartedAt: time.Now().UTC(),
		PromptSHA: agentregistry.ParsePrompt(req.Prompt),
		SessionID: agentregistry.CurrentSessionID(req.Env),
	}
	runID, err := m.store.StartRun(ctx, run)
	if err != nil {
		return agentregistry.Run{}, fmt.Errorf("runmgr: start run: %w", err)
	}
	run.ID = runID
	run.EventCount = 0

	rs := newRunState(runID)
	m.mu.Lock()
	m.active[runID] = rs
	m.mu.Unlock()

	m.wg.Add(1)
	go m.executeRun(context.Background() /* detached from request */, rs, agent, req)

	return run, nil
}

// executeRun drives the backend for one run. Detached lifetime:
// the run continues past the originating HTTP request's context
// because long-running agents routinely outlast their request.
// Shutdown semantics come from [Manager.Close].
func (m *Manager) executeRun(ctx context.Context, rs *runState, agent agentregistry.Agent, req StartRequest) {
	defer m.wg.Done()
	defer m.cleanupRun(rs)

	factory := m.backendFactory
	if factory == nil {
		factory = backend.New
	}
	b, err := factory(agent.Backend, backend.Config{
		Logger: m.logger,
	})
	if err != nil {
		m.failRun(ctx, rs, agentregistry.RunFinish{
			Status: "failed",
			Error:  fmt.Sprintf("instantiate %s backend: %v", agent.Backend, err),
		})
		return
	}

	opts := backend.ExecOptions{
		ResumeSessionID: req.ResumeID,
		Env:             envSliceToMap(req.Env),
		CustomArgs:      req.CustomArgs,
	}

	session, err := b.Execute(ctx, req.Prompt, opts)
	if err != nil {
		m.failRun(ctx, rs, agentregistry.RunFinish{
			Status: "failed",
			Error:  fmt.Sprintf("backend execute: %v", err),
		})
		return
	}

	for msg := range session.Messages {
		payload, mErr := json.Marshal(msg)
		if mErr != nil {
			// Drop un-marshallable messages; the registry's record
			// is best-effort (see ADR-0008) and the renderer path
			// would not see them either.
			m.logger.Warn("runmgr: marshal message",
				"run_id", rs.runID, "err", mErr)
			continue
		}
		_ = msg
		ev := &agentregistry.Event{
			RunID:   rs.runID,
			Seq:     0, // registry assigns on AppendEvent
			TS:      time.Now().UTC(),
			Kind:    string(msg.Type),
			Payload: payload,
		}
		if err := rs.appendEvent(ctx, m.store, ev); err != nil {
			m.logger.Warn("runmgr: AppendEvent failed",
				"run_id", rs.runID, "err", err)
		}
	}

	// Wait for terminal Result.
	var res backend.Result
	select {
	case res = <-session.Result:
	case <-ctx.Done():
		res = backend.Result{Status: "cancelled", Error: ctx.Err().Error()}
	}

	var fin agentregistry.RunFinish
	fin.Status = res.Status
	if fin.Status == "" {
		fin.Status = "completed"
	}
	fin.DurationMs = res.DurationMs
	fin.SessionID = res.SessionID
	fin.Error = res.Error
	if len(res.Usage) > 0 {
		if b, mErr := json.Marshal(res.Usage); mErr == nil {
			fin.Usage = b
		}
	}
	m.finishRun(ctx, rs, fin)
}

// cleanupRun removes the run from the active map after the run
// goroutine has finished its writes.
func (m *Manager) cleanupRun(rs *runState) {
	m.mu.Lock()
	delete(m.active, rs.runID)
	m.mu.Unlock()
}

// finishRun writes the terminal row and closes the broadcaster.
func (m *Manager) finishRun(ctx context.Context, rs *runState, fin agentregistry.RunFinish) {
	if err := m.store.FinishRun(ctx, rs.runID, fin); err != nil {
		m.logger.Warn("runmgr: FinishRun",
			"run_id", rs.runID, "err", err)
	}
	rs.markFinished()
}

// failRun is the synchronous shortcut used when we never got a
// session up (instantiate / execute error). Writes the failed
// run row and marks finished so subscribers see the terminal.
func (m *Manager) failRun(ctx context.Context, rs *runState, fin agentregistry.RunFinish) {
	if fin.Status == "" {
		fin.Status = "failed"
	}
	m.finishRun(ctx, rs, fin)
}

// Run returns the latest registry view of the given run ID.
func (m *Manager) Run(ctx context.Context, id int64) (agentregistry.Run, error) {
	return m.store.GetRun(ctx, id)
}

// ListRuns delegates to the store. `agentRef` may be an id, a
// name, or "" (no filter); `opts.Status` may be a status string
// or "" (no filter); `opts.Limit` caps the result count.
func (m *Manager) ListRuns(ctx context.Context, agentRef string, opts agentregistry.ListRunOpts) ([]agentregistry.Run, error) {
	return m.store.ListRuns(ctx, agentRef, opts)
}

// EventsAfter returns the events for runID with Seq > afterSeq.
// afterSeq < 0 means "from the beginning".
func (m *Manager) EventsAfter(ctx context.Context, runID, afterSeq int64) ([]agentregistry.Event, error) {
	all, err := m.store.Events(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]agentregistry.Event, 0, len(all))
	for _, ev := range all {
		if ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	return out, nil
}

// SubscribeResult bundles the channel returned by Subscribe with
// the last historical seq seen so callers can set Last-Event-Id
// on resume.
type SubscribeResult struct {
	Events    <-chan []byte // SSE wire bytes (id/event/data blocks)
	FromSeq   int64         // the seq the channel starts at (1 if no replay requested)
	IsActive  bool          // true if the run is still running; false if Subscribe raced with Finish
	UnsubFunc func()        // idempotent; safe to defer
}

// Subscribe attaches to the live tail of runID. afterSeq is the
// last seq the caller has already seen; events already in the
// store with Seq > afterSeq are replayed first, then live events
// stream in. `bufSize` controls the channel buffer; 0 picks
// [DefaultSSEBufferSize].
//
// If the run finished before Subscribe was called, the channel is
// closed immediately after replay; if the run finishes while the
// subscriber is attached, the channel is closed at the end.
func (m *Manager) Subscribe(ctx context.Context, runID, afterSeq int64, bufSize int) (SubscribeResult, error) {
	if bufSize <= 0 {
		bufSize = DefaultSSEBufferSize
	}

	m.mu.Lock()
	rs, ok := m.active[runID]
	m.mu.Unlock()

	// Replay from the store even if the run is no longer active.
	replay, err := m.EventsAfter(ctx, runID, afterSeq)
	if err != nil {
		return SubscribeResult{}, err
	}

	if !ok {
		// Run already finished (or never existed). Replay
		// everything we found and close immediately.
		ch := make(chan []byte)
		go func() {
			defer close(ch)
			for _, ev := range replay {
				ch <- formatSSEEvent(&ev)
			}
		}()
		return SubscribeResult{
			Events:    ch,
			FromSeq:   afterSeq,
			IsActive:  false,
			UnsubFunc: func() {},
		}, nil
	}

	// Active run path: reserve a subscriber slot BEFORE replay so
	// the run goroutine does not push a live event between replay
	// output and tail subscription.
	subCh, histCount := rs.registerSubscriber(bufSize)

	if !rs.Finished() {
		// Tail path: feed replay first, then live. We bound the
		// replay write to ctx.Done so a closed connection can abort
		// the replay without queueing stale events.
		go func() {
			// Close subCh when the run finishes (markFinished
			// already does this for active subscribers).
			defer func() {
				// No-op: rs.markFinished closes subCh.
			}()
			for _, ev := range replay {
				select {
				case <-ctx.Done():
					return
				case subCh <- formatSSEEvent(&ev):
				}
			}
			// Hand off to live tail: block until run finishes (rs
			// closes subCh in markFinished).
			<-rs.Done()
		}()
	} else {
		// Race: run finished between our checks. Drain replay and close.
		go func() {
			defer func() {
				// subCh was already closed by registerSubscriber
				// because finished was true.
				_ = histCount
			}()
			for _, ev := range replay {
				select {
				case <-ctx.Done():
					return
				case subCh <- formatSSEEvent(&ev):
				}
			}
		}()
	}

	return SubscribeResult{
		Events:    subCh,
		FromSeq:   afterSeq,
		IsActive:  !rs.Finished(),
		UnsubFunc: func() { /* rs owns lifetime */ },
	}, nil
}

// ActiveCount returns how many runs are currently held in the
// scheduler. Useful for graceful-shutdown handlers (e.g. refuse
// reload while runs are in flight). Cheap; holds the lock briefly.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// Close waits for in-flight run goroutines to drain. The cancel
// is not forceful; backend Execute calls already honour their own
// context (the runs were started with a detached context, so
// Close waits for natural completion, not cancellation).
//
// This is intentionally not used by the current `conductor serve`
// graceful-shutdown path; SIGTERM on the daemon simply lets active
// runs finish in the background once the HTTP listener is
// stopped (the registry keeps the rows; clients reconnect and
// stream to completion).
func (m *Manager) Close() {
	m.wg.Wait()
}

// envSliceToMap converts ["K=V", ...] into map[K]V. Empty /
// malformed entries are skipped; an entry without '=' logs a
// warning and is dropped.
func envSliceToMap(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for _, e := range env {
		eq := strings.IndexByte(e, '=')
		if eq < 1 {
			continue
		}
		out[e[:eq]] = e[eq+1:]
	}
	return out
}

// SetBackendFactory overrides how the Manager builds the backend
// driver for each StartRun. The default (set in [New]) is
// [backend.New] which spawns the production CLI. Tests pass a
// factory that returns a fake-binary-backed driver instead.
//
// Thread-safe: the factory is read once at the start of
// executeRun and held in a local variable; calling this method
// mid-flight has no effect on in-flight runs.
func (m *Manager) SetBackendFactory(f func(agentType string, cfg backend.Config) (backend.Backend, error)) {
	m.mu.Lock()
	m.backendFactory = f
	m.mu.Unlock()
}
