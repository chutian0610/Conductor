package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"conductor/server/internal/protocol"
)

// RunStatus is the lifecycle state of one invocation. Persisted
// in RunState.Status; the runner transitions through these as
// the turn progresses.
type RunStatus string

const (
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCancelled RunStatus = "cancelled"
)

// RunState is the persisted snapshot of one invocation. Keyed by
// RunID. Mutations go through Storage.UpdateRun so the per-runID
// mutex serializes writers.
type RunState struct {
	RunID      string                    `json:"runId"`
	SpecID     string                    `json:"specId"`
	Prompt     string                    `json:"prompt"`
	Status     RunStatus                 `json:"status"`
	StartedAt  time.Time                 `json:"startedAt"`
	FinishedAt *time.Time                `json:"finishedAt,omitempty"`
	SessionID  string                    `json:"sessionId,omitempty"` // codex threadId (for --resume)
	Result     *protocol.AgentTurnResult `json:"result,omitempty"`
	Error      string                    `json:"error,omitempty"` // populated on Status=failed

	// PID is the host process id of the conductor run that owns
	// this run. Used by `conductor cancel <runId>` to send SIGTERM.
	// 0 for runs created before this field existed, or for runs
	// driven by a daemon (Phase 2+).
	PID int `json:"pid,omitempty"`
}

// TimelineItem is one NDJSON line on disk: a timestamp plus the
// raw AgentStreamEvent. The agent already carries a Kind so we
// don't duplicate it.
type TimelineItem struct {
	TS    time.Time                  `json:"ts"`
	Event protocol.AgentStreamEvent `json:"event"`
}

// RunFilter narrows ListRuns. Zero values mean "no filter".
type RunFilter struct {
	SpecID string      // empty = any spec
	Status []RunStatus // empty = any status
	Limit  int         // 0 = no limit (caller caps if they want)
	// Since time.Time filtering (Phase 2+ — UI date pickers).
}

// ErrRunNotFound is returned by GetRun/ListRuns when a runId has
// no state.json. The JsonFileStorage swallows this on ListRuns
// (directories without state.json are treated as orphans and
// skipped, see §6.2.5 migration story).
var ErrRunNotFound = errors.New("storage: run not found")

// ErrSessionIDMissing is returned by LookupSessionID when the
// run exists but has no sessionId recorded. Typical when the
// run is still in flight, or failed before turn/completed.
var ErrSessionIDMissing = errors.New("storage: run has no sessionId yet")

// NewRunID returns a fresh 16-hex-char run id backed by
// crypto/rand. Two consecutive calls produce different ids with
// negligible collision probability (~2^-64).
func NewRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand only fails if the OS RNG is broken; not
		// something to handle gracefully in user code.
		panic("storage: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}


// Storage is the persistence interface. Two implementations
// (JsonFileStorage, future SqliteStorage) sit behind it; callers
// depend on this so a CONDUCTOR_STORAGE=json|sqlite runtime
// switch is a one-liner when SQLite lands.
//
// Implementations MUST:
//   - be goroutine-safe (the runner pumps events from one
//     goroutine while updating state from another);
//   - serialize UpdateRun calls per runId (state.json tear
//     resistance);
//   - persist append-timeline atomically per line (NDJSON
//     append of one record at a time).
type Storage interface {
	// RunCreate writes a fresh RunState with Status=running and
	// StartedAt=now. Returns ErrRunNotFound-adjacent error if
	// runId is already in use.
	CreateRun(ctx context.Context, runID, specID, prompt string) (*RunState, error)

	// GetRun reads state.json for runID. Returns ErrRunNotFound
	// if the run doesn't exist.
	GetRun(ctx context.Context, runID string) (*RunState, error)

	// LookupSessionID is a thin convenience that returns the
	// Codex thread id stored on a completed run. Used by
	// `conductor run --resume-run <runId>` to translate a run
	// reference into the sessionId codex needs for thread/resume.
	// Returns ErrRunNotFound if runID is unknown, or
	// ErrSessionIDMissing if the run exists but didn't record a
	// sessionId (still running, failed before turn/completed, or
	// the provider didn't populate one).
	LookupSessionID(ctx context.Context, runID string) (string, error)

	// ListRuns returns every RunState matching filter, sorted
	// newest-first. Orphan runs (no state.json) are skipped.
	ListRuns(ctx context.Context, filter RunFilter) ([]RunState, error)

	// UpdateRun reads state.json, applies fn under the per-runID
	// lock, and writes back atomically. fn runs while the lock
	// is held — keep it pure (no I/O, no calls back into
	// Storage).
	UpdateRun(ctx context.Context, runID string, fn func(*RunState)) error

	// AppendTimeline appends one NDJSON line for runID. Safe to
	// call concurrently with other AppendTimeline calls for
	// different runIDs (and the same runID — the line is the
	// atomic unit).
	AppendTimeline(ctx context.Context, runID string, item TimelineItem) error

	// ReadTimeline streams TimelineItems for runID, oldest
	// first. Returns an empty reader (immediate io.EOF) if
	// the run has no timeline yet. Callers MUST Close the
	// returned reader.
	ReadTimeline(ctx context.Context, runID string) (TimelineReader, error)
}

// TimelineReader streams TimelineItems. Mirrors the database/sql
// Rows shape so callers can iterate with a familiar Next/Close
// pattern.
type TimelineReader interface {
	Next() (TimelineItem, error) // io.EOF when exhausted
	Close() error
}

// NoopStorage is a Storage that keeps everything in memory
// transiently — CreateRun returns a fresh RunState (not
// persisted), UpdateRun/AppendTimeline are silently no-ops, and
// GetRun always returns ErrRunNotFound. Useful for tests and
// for the (Phase 2) --no-persist flag.
type NoopStorage struct{}

func (NoopStorage) CreateRun(ctx context.Context, runID, specID, prompt string) (*RunState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &RunState{
		RunID:     runID,
		SpecID:    specID,
		Prompt:    prompt,
		Status:    RunStatusRunning,
		StartedAt: time.Now().UTC(),
	}, nil
}
func (NoopStorage) GetRun(context.Context, string) (*RunState, error) {
	return nil, ErrRunNotFound
}
func (NoopStorage) LookupSessionID(context.Context, string) (string, error) {
	return "", ErrRunNotFound
}
func (NoopStorage) ListRuns(context.Context, RunFilter) ([]RunState, error) {
	return nil, nil
}
func (NoopStorage) UpdateRun(context.Context, string, func(*RunState)) error {
	return nil
}
func (NoopStorage) AppendTimeline(context.Context, string, TimelineItem) error {
	return nil
}
func (NoopStorage) ReadTimeline(context.Context, string) (TimelineReader, error) {
	return noopTimeline{}, nil
}

type noopTimeline struct{}

func (noopTimeline) Next() (TimelineItem, error) { return TimelineItem{}, errEOF }
func (noopTimeline) Close() error              { return nil }

var errEOF = errors.New("EOF")
