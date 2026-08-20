package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"conductor/server/internal/home"
)

// JsonFileStorage is the Phase 1 Storage implementation: one
// directory per run under <root>/runs/<runId>/. See doc.go for
// the on-disk layout.
type JsonFileStorage struct {
	root      string  // CONDUCTOR_HOME or test-supplied
	muByRunID sync.Map // runID -> *sync.Mutex (process-scoped; cross-process locking is Phase 2)
}

// NewJsonFileStorage returns a Storage backed by JSON files
// under $CONDUCTOR_HOME/runs/. Ensures the runs/ directory exists
// with 0700 perms.
func NewJsonFileStorage() (*JsonFileStorage, error) {
	return NewJsonFileStorageAt(home.ConductorHome())
}

// NewJsonFileStorageForHome is the test-facing alias. Same as
// NewJsonFileStorageAt; the name is more descriptive in tests
// ("for this home dir") than the implementation detail ("at this
// path").
func NewJsonFileStorageForHome(root string) (*JsonFileStorage, error) {
	return NewJsonFileStorageAt(root)
}

// NewJsonFileStorageAt is the explicit-root constructor. Tests
// use it to point at a temp directory without touching
// $CONDUCTOR_HOME.
func NewJsonFileStorageAt(root string) (*JsonFileStorage, error) {
	if err := os.MkdirAll(filepath.Join(root, "runs"), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir runs: %w", err)
	}
	return &JsonFileStorage{root: root}, nil
}

// runsDir returns <s.root>/runs/.
func (s *JsonFileStorage) runsDir() string {
	return filepath.Join(s.root, "runs")
}

func (s *JsonFileStorage) runDir(runID string) string {
	return filepath.Join(s.runsDir(), runID)
}

func (s *JsonFileStorage) statePath(runID string) string {
	return filepath.Join(s.runDir(runID), "state.json")
}

func (s *JsonFileStorage) timelinePath(runID string) string {
	return filepath.Join(s.runDir(runID), "timeline.ndjson")
}

// lockFor returns a per-runID mutex (lazy-init).
func (s *JsonFileStorage) lockFor(runID string) *sync.Mutex {
	if v, ok := s.muByRunID.Load(runID); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := s.muByRunID.LoadOrStore(runID, mu)
	return actual.(*sync.Mutex)
}

// CreateRun writes a fresh RunState with Status=running. Errors
// if state.json already exists for runID (callers should generate
// unique IDs via NewRunID()).
func (s *JsonFileStorage) CreateRun(ctx context.Context, runID, specID, prompt string) (*RunState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, errors.New("storage: runID required")
	}

	state := &RunState{
		RunID:     runID,
		SpecID:    specID,
		Prompt:    prompt,
		Status:    RunStatusRunning,
		StartedAt: time.Now().UTC(),
	}
	if err := os.MkdirAll(s.runDir(runID), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir run dir: %w", err)
	}
	// Refuse to clobber an existing run.
	if _, err := os.Stat(s.statePath(runID)); err == nil {
		return nil, fmt.Errorf("storage: run %q already exists", runID)
	}
	if err := s.writeState(state); err != nil {
		return nil, err
	}
	return state, nil
}

// GetRun reads state.json. ErrRunNotFound if the run doesn't exist.
func (s *JsonFileStorage) GetRun(ctx context.Context, runID string) (*RunState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.statePath(runID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("read state.json: %w", err)
	}
	var state RunState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state.json: %w", err)
	}
	return &state, nil
}

// LookupSessionID is a convenience that returns the Codex thread
// id from a prior run's state.json. Used by
// `conductor run --resume-run <runId>` to translate a run
// reference into the sessionId codex needs for thread/resume.
func (s *JsonFileStorage) LookupSessionID(ctx context.Context, runID string) (string, error) {
	state, err := s.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}
	if state.SessionID == "" {
		return "", ErrSessionIDMissing
	}
	return state.SessionID, nil
}

// ListRuns returns every run under runs/, newest first, applying
// filter. Orphan directories (no state.json) are silently skipped
// — they're half-aborted writes that the next prune pass should
// clean up.
func (s *JsonFileStorage) ListRuns(ctx context.Context, filter RunFilter) ([]RunState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.runsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs dir: %w", err)
	}
	out := make([]RunState, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := s.GetRun(ctx, e.Name())
		if err != nil {
			if errors.Is(err, ErrRunNotFound) {
				continue
			}
			return nil, fmt.Errorf("load run %q: %w", e.Name(), err)
		}
		if filter.SpecID != "" && rec.SpecID != filter.SpecID {
			continue
		}
		if len(filter.Status) > 0 {
			matched := false
			for _, st := range filter.Status {
				if rec.Status == st {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// UpdateRun reads-modifies-writes state.json under the per-runID
// mutex. fn is invoked while the lock is held; keep it pure.
func (s *JsonFileStorage) UpdateRun(ctx context.Context, runID string, fn func(*RunState)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("storage: UpdateRun requires non-nil fn")
	}
	mu := s.lockFor(runID)
	mu.Lock()
	defer mu.Unlock()

	state, err := s.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	fn(state)
	return s.writeState(state)
}

// AppendTimeline writes one NDJSON line. Opens, writes, closes
// per call — no persistent fd; cheap because the per-run files
// are small and we don't need cross-write buffering.
func (s *JsonFileStorage) AppendTimeline(ctx context.Context, runID string, item TimelineItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if item.TS.IsZero() {
		item.TS = time.Now().UTC()
	}
	data, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("marshal timeline item: %w", err)
	}
	data = append(data, '\n')
	f, err := os.OpenFile(s.timelinePath(runID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open timeline.ndjson: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("append timeline: %w", err)
	}
	return nil
}

// ReadTimeline opens timeline.ndjson for streaming. Returns an
// emptyTimeline (immediate EOF) when the file doesn't exist —
// which is the normal state for a run that hasn't produced any
// events yet.
func (s *JsonFileStorage) ReadTimeline(ctx context.Context, runID string) (TimelineReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(s.timelinePath(runID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyTimeline{}, nil
		}
		return nil, fmt.Errorf("open timeline: %w", err)
	}
	return &jsonTimeline{scanner: bufio.NewScanner(f)}, nil
}

// writeState atomically replaces state.json: write to state.json.tmp
// then rename. Uses 0600 so the file is owner-only (matches the
// 0700 on the containing dir).
func (s *JsonFileStorage) writeState(state *RunState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state.json: %w", err)
	}
	final := s.statePath(state.RunID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write state.json.tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename state.json.tmp: %w", err)
	}
	return nil
}

// jsonTimeline is the streaming reader for timeline.ndjson.
type jsonTimeline struct {
	scanner *bufio.Scanner
	closed  bool
}

func (t *jsonTimeline) Next() (TimelineItem, error) {
	if t.closed {
		return TimelineItem{}, io.EOF
	}
	if !t.scanner.Scan() {
		err := t.scanner.Err()
		if err == nil {
			err = io.EOF
		}
		return TimelineItem{}, err
	}
	var item TimelineItem
	if err := json.Unmarshal(t.scanner.Bytes(), &item); err != nil {
		return TimelineItem{}, err
	}
	return item, nil
}

func (t *jsonTimeline) Close() error {
	t.closed = true
	return t.scanner.Err()
}

// emptyTimeline is the zero-item reader (no timeline file yet).
type emptyTimeline struct{ done bool }

func (e emptyTimeline) Next() (TimelineItem, error) {
	if e.done {
		return TimelineItem{}, io.EOF
	}
	e.done = true
	return TimelineItem{}, io.EOF
}
// (kept consistent with jsonTimeline, which also returns io.EOF)
func (e emptyTimeline) Close() error { return nil }
