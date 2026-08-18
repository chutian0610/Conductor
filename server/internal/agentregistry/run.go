package agentregistry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Run captures a single execution of an Agent. One Agent has many
// Runs; a Run owns its Events.
//
// Status values mirror backend.Result.Status: "running", "completed",
// "failed", "timeout", "cancelled". "running" is the only non-terminal
// status and is set by StartRun.
type Run struct {
	ID          int64           `json:"id"`
	AgentID     int64           `json:"agent_id"`
	ParentRunID *int64          `json:"parent_run_id,omitempty"`
	StartedAt   time.Time       `json:"started_at"`
	EndedAt     *time.Time      `json:"ended_at,omitempty"`
	Status      string          `json:"status"`
	PromptSHA   string          `json:"prompt_sha"`
	SessionID   string          `json:"session_id"`
	DurationMs  int64           `json:"duration_ms"`
	Error       string          `json:"error,omitempty"`
	Usage       json.RawMessage `json:"usage,omitempty"`
	EventCount  int             `json:"event_count,omitempty"`
}

// Event is one observation from the streamed Agent.Message channel.
// seq is monotonic per-run; payload is the JSON-serialised Message.
type Event struct {
	ID      int64           `json:"id"`
	RunID   int64           `json:"run_id"`
	Seq     int64           `json:"seq"`
	TS      time.Time       `json:"ts"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// RunFinish closes a Run. It is set by FinishRun; callers must populate
// at least Status.
type RunFinish struct {
	Status     string
	DurationMs int64
	Error      string
	SessionID  string
	Usage      json.RawMessage
}

// ListRunOpts filters ListRuns.
type ListRunOpts struct {
	Status string // empty = any
	Limit  int    // 0 = no limit
}

// StartRun begins a new run for the given agent. The ParentRunID is
// propagated from CONDUCTOR_PARENT_RUN_ID by the caller (typically the
// CLI's run flow). Returns the new run id.
func (s *Store) StartRun(ctx context.Context, r Run) (int64, error) {
	if r.AgentID <= 0 {
		return 0, errors.New("agentregistry: StartRun: agent_id required")
	}
	// FK check so callers get a clear error instead of an opaque
	// constraint violation if the agent was archived/deleted.
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE id = ?`, r.AgentID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("agentregistry: agent %d not found", r.AgentID)
		}
		return 0, err
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `
        INSERT INTO runs(agent_id, parent_run_id, started_at, status, prompt_sha, session_id)
        VALUES (?, ?, ?, 'running', ?, ?)`,
		r.AgentID,
		nullInt64Ptr(r.ParentRunID),
		r.StartedAt.UnixMilli(),
		r.PromptSHA,
		r.SessionID,
	)
	if err != nil {
		return 0, fmt.Errorf("agentregistry: insert run: %w", err)
	}
	return res.LastInsertId()
}

// FinishRun closes a Run opened by StartRun. ended_at is set to now if
// the caller did not specify a duration.
//
// Idempotent: reapplying a Finish to an already-terminal run is a
// no-op so late events delivered after the run "completed" don't crash.
func (s *Store) FinishRun(ctx context.Context, id int64, f RunFinish) error {
	if f.Status == "" {
		return errors.New("agentregistry: FinishRun: status required")
	}
	now := time.Now().UnixMilli()
	usage := f.Usage
	if len(usage) == 0 {
		usage = json.RawMessage("{}")
	}
	res, err := s.db.ExecContext(ctx, `
        UPDATE runs
           SET status      = ?,
               ended_at    = ?,
               duration_ms = ?,
               error       = ?,
               session_id  = COALESCE(NULLIF(?, ''), session_id),
               usage_json  = ?
         WHERE id = ?
           AND status = 'running'`,
		f.Status, now, f.DurationMs, f.Error,
		f.SessionID, string(usage), id,
	)
	if err != nil {
		return fmt.Errorf("agentregistry: finish run: %w", err)
	}
	_, err = res.RowsAffected()
	return err
}

// AppendEvent writes one event for the given run. seq is assigned by
// the store as (max(seq)+1) within a single SQL statement so concurrent
// callers cannot collide (mirrors the serialised-write pattern multica's
// `goal_manager.py` documents). The seq is implicit; callers that need
// the per-event position can read Events(runID) after the run ends.
func (s *Store) AppendEvent(ctx context.Context, runID int64, kind string, payload []byte) error {
	if payload == nil {
		payload = []byte("{}")
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO events(run_id, seq, ts, kind, payload_json)
        VALUES (
            ?,
            COALESCE((SELECT MAX(seq) FROM events WHERE run_id = ?), 0) + 1,
            ?, ?, ?
        )`,
		runID, runID, now, kind, string(payload),
	)
	if err != nil {
		return fmt.Errorf("agentregistry: append event: %w", err)
	}
	return nil
}

// GetRun returns one run, optionally with an event count.
func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT r.id, r.agent_id, r.parent_run_id,
               r.started_at, r.ended_at, r.status, r.prompt_sha,
               r.session_id, r.duration_ms, r.error, r.usage_json,
               (SELECT COUNT(*) FROM events e WHERE e.run_id = r.id)
          FROM runs r WHERE r.id = ?`, id)
	r, err := scanRun(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// ListRuns returns runs for an agent (or all agents if ref == ""),
// newest first.
func (s *Store) ListRuns(ctx context.Context, agentRef string, opts ListRunOpts) ([]Run, error) {
	var (
		args  []any
		where []string
	)
	if agentRef != "" {
		id, isID, err := parseAgentRef(agentRef)
		if err != nil {
			return nil, err
		}
		if isID {
			where = append(where, "r.agent_id = ?")
		} else {
			where = append(where, "a.name = ?")
		}
		args = append(args, lookupArg(agentRef, id, isID))
	}
	if opts.Status != "" {
		where = append(where, "r.status = ?")
		args = append(args, opts.Status)
	}
	q := strings.Builder{}
	q.WriteString(`
        SELECT r.id, r.agent_id, r.parent_run_id,
               r.started_at, r.ended_at, r.status, r.prompt_sha,
               r.session_id, r.duration_ms, r.error, r.usage_json,
               (SELECT COUNT(*) FROM events e WHERE e.run_id = r.id)
          FROM runs r
          JOIN agents a ON a.id = r.agent_id
         WHERE 1=1`)
	for _, w := range where {
		q.WriteString(" AND " + w)
	}
	q.WriteString(" ORDER BY r.id DESC")
	if opts.Limit > 0 {
		q.WriteString(" LIMIT ?")
		args = append(args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("agentregistry: list runs: %w", err)
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Events returns all events for a run in seq order.
func (s *Store) Events(ctx context.Context, runID int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, run_id, seq, ts, kind, payload_json
          FROM events WHERE run_id = ? ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("agentregistry: events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var (
			e      Event
			ts     int64
			payload sql.RawBytes
		)
		if err := rows.Scan(&e.ID, &e.RunID, &e.Seq, &ts, &e.Kind, &payload); err != nil {
			return nil, err
		}
		e.TS = unixMilli(ts)
		e.Payload = append([]byte(nil), payload...)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ParsePrompt returns the canonical SHA-256 (hex) for a run prompt.
// Run callers should compute this once and store it via Run.PromptSHA
// so the audit trail can correlate runs of the same prompt.
func ParsePrompt(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

type runScanner interface{ Scan(dest ...any) error }

func scanRun(s runScanner, withCount bool) (Run, error) {
	var (
		r          Run
		parent     sql.NullInt64
		endedAt    sql.NullInt64
		errStr     sql.NullString
		promptSHA  sql.NullString
		sessionID  sql.NullString
		usage      []byte
		eventCount sql.NullInt64
		startedAt  int64
	)
	dests := []any{
		&r.ID, &r.AgentID, &parent,
		&startedAt, &endedAt, &r.Status, &promptSHA,
		&sessionID, &r.DurationMs, &errStr, &usage,
	}
	if withCount {
		dests = append(dests, &eventCount)
	}
	if err := s.Scan(dests...); err != nil {
		return r, err
	}
	r.StartedAt = unixMilli(startedAt)
	if parent.Valid {
		v := parent.Int64
		r.ParentRunID = &v
	}
	if endedAt.Valid {
		t := unixMilli(endedAt.Int64)
		r.EndedAt = &t
	}
	if errStr.Valid {
		r.Error = errStr.String
	}
	r.PromptSHA = promptSHA.String
	r.SessionID = sessionID.String
	if len(usage) > 0 {
		r.Usage = json.RawMessage(append([]byte(nil), usage...))
	}
	if eventCount.Valid {
		r.EventCount = int(eventCount.Int64)
	}
	return r, nil
}

func nullInt64Ptr(p *int64) any {
	if p == nil || *p == 0 {
		return nil
	}
	return *p
}

// joinIDs is a tiny helper for CLI formatting only; not exported to
// avoid coupling the store to a presentation layer.
func joinIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}
