package agentregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Agent is a registered entity in the registry. It is the
// operator-facing abstraction; one Agent has many Runs.
type Agent struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Backend     string     `json:"backend"`
	ParentID    int64      `json:"parent_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
}

// AgentPatch captures optional updates to mutate an existing agent.
// Only non-nil pointer fields are written; "" / 0 means "no change".
type AgentPatch struct {
	Description *string
	Backend     *string
	ParentID    *int64
	ClearParent bool
}

// ListAgentOpts filters ListAgents.
type ListAgentOpts struct {
	Backend         string // empty = any
	IncludeArchived bool
}

// RegisterAgent inserts a new agent row. Returns the assigned id.
//
// Validation: Name must be unique within the registry; Backend must be
// one of the values package agent declares (enforced here as a string
// check so callers don't have to import the backend package).
func (s *Store) RegisterAgent(ctx context.Context, a Agent) (int64, error) {
	if strings.TrimSpace(a.Name) == "" {
		return 0, errors.New("agentregistry: agent name is required")
	}
	if !isKnownBackend(a.Backend) {
		return 0, fmt.Errorf("agentregistry: unknown backend %q", a.Backend)
	}
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `
        INSERT INTO agents(name, description, backend, parent_id, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(a.Name),
		a.Description,
		a.Backend,
		nullInt(a.ParentID),
		now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("agentregistry: insert agent: %w", err)
	}
	return res.LastInsertId()
}

// EnsureAgent returns the existing agent matching a.Name, or
// registers a fresh one when none exists. The struct's Name +
// Backend are the identity keys; Description and ParentID are
// ignored on lookup and applied only on first register.
//
// This is the helper `conductor run` uses to give the V1 "load
// YAML, run backend" flow a registry home without forcing the
// operator to issue a separate `register` call first.
func (s *Store) EnsureAgent(ctx context.Context, a Agent) (Agent, error) {
	if strings.TrimSpace(a.Name) == "" {
		return Agent{}, errors.New("agentregistry: EnsureAgent: name required")
	}
	if existing, err := s.GetAgent(ctx, a.Name); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Agent{}, err
	}
	id, err := s.RegisterAgent(ctx, a)
	if err != nil {
		return Agent{}, err
	}
	return s.GetAgent(ctx, "@"+strconv.FormatInt(id, 10))
}

// UpdateAgent applies a patch to an existing agent. `archived_at` is
// owned by ArchiveAgent and is not mutated here.
func (s *Store) UpdateAgent(ctx context.Context, id int64, patch AgentPatch) error {
	if patch.Backend != nil && !isKnownBackend(*patch.Backend) {
		return fmt.Errorf("agentregistry: unknown backend %q", *patch.Backend)
	}
	now := time.Now().UnixMilli()
	q := strings.Builder{}
	q.WriteString("UPDATE agents SET updated_at = ?")
	args := []any{now}
	if patch.Description != nil {
		q.WriteString(", description = ?")
		args = append(args, *patch.Description)
	}
	if patch.Backend != nil {
		q.WriteString(", backend = ?")
		args = append(args, *patch.Backend)
	}
	if patch.ClearParent {
		q.WriteString(", parent_id = NULL")
	} else if patch.ParentID != nil {
		q.WriteString(", parent_id = ?")
		args = append(args, *patch.ParentID)
	}
	q.WriteString(" WHERE id = ?")
	args = append(args, id)

	res, err := s.db.ExecContext(ctx, q.String(), args...)
	if err != nil {
		return fmt.Errorf("agentregistry: update agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ArchiveAgent soft-deletes an agent. Use ListAgents with
// IncludeArchived=true to retrieve archived rows; the catalog itself
// never hard-deletes an agent so historical Runs keep their FK.
func (s *Store) ArchiveAgent(ctx context.Context, id int64) error {
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `
        UPDATE agents SET archived_at = ?, updated_at = ? WHERE id = ?`,
		now, now, id)
	if err != nil {
		return fmt.Errorf("agentregistry: archive agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetAgent resolves ref to a single agent. ref can be either:
//
//	"<id>"     — numeric, looked up by id
//	"@<id>"    — explicit id syntax (prefer this in scripts)
//	"<name>"   — looked up by name
//
// Returns ErrNotFound when no row matches.
func (s *Store) GetAgent(ctx context.Context, ref string) (Agent, error) {
	id, isID, err := parseAgentRef(ref)
	if err != nil {
		return Agent{}, err
	}
	q := `SELECT id, name, description, backend, parent_id, created_at, updated_at, archived_at
          FROM agents WHERE `
	if isID {
		q += "id = ?"
	} else {
		q += "name = ?"
	}
	row := s.db.QueryRowContext(ctx, q, lookupArg(ref, id, isID))
	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, fmt.Errorf("agentregistry: get agent: %w", err)
	}
	return a, nil
}

// ListAgents returns agents ordered by id. Backend and archived filters
// stack. An empty Backend string means "any backend".
func (s *Store) ListAgents(ctx context.Context, opts ListAgentOpts) ([]Agent, error) {
	q := strings.Builder{}
	q.WriteString(`SELECT id, name, description, backend, parent_id,
                          created_at, updated_at, archived_at
                   FROM agents WHERE 1=1`)
	args := []any{}
	if opts.Backend != "" {
		q.WriteString(" AND backend = ?")
		args = append(args, opts.Backend)
	}
	if !opts.IncludeArchived {
		q.WriteString(" AND archived_at IS NULL")
	}
	q.WriteString(" ORDER BY id ASC")

	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("agentregistry: list agents: %w", err)
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// scanAgent reads one row from either a Row or Rows.
type scanner interface{ Scan(dest ...any) error }

func scanAgent(s scanner) (Agent, error) {
	var (
		a         Agent
		parentID  sql.NullInt64
		archived  sql.NullInt64
		createdAt int64
		updatedAt int64
	)
	if err := s.Scan(
		&a.ID, &a.Name, &a.Description, &a.Backend, &parentID,
		&createdAt, &updatedAt, &archived,
	); err != nil {
		return a, err
	}
	if parentID.Valid {
		a.ParentID = parentID.Int64
	}
	a.CreatedAt = unixMilli(createdAt)
	a.UpdatedAt = unixMilli(updatedAt)
	if archived.Valid {
		t := unixMilli(archived.Int64)
		a.ArchivedAt = &t
	}
	return a, nil
}

func parseAgentRef(ref string) (id int64, isID bool, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, false, errors.New("agentregistry: empty agent reference")
	}
	if strings.HasPrefix(ref, "@") {
		n, perr := strconv.ParseInt(ref[1:], 10, 64)
		if perr != nil {
			return 0, false, fmt.Errorf("agentregistry: bad id ref %q: %w", ref, perr)
		}
		return n, true, nil
	}
	// Numeric ref is treated as id; otherwise name lookup.
	if n, perr := strconv.ParseInt(ref, 10, 64); perr == nil {
		return n, true, nil
	}
	return 0, false, nil
}

// lookupArg returns the right scalar to bind to "?" for the chosen
// lookup column. When looking up by name, the original ref string is
// needed; when by id, the parsed int is.
func lookupArg(ref string, id int64, isID bool) any {
	if isID {
		return id
	}
	return ref
}

// isKnownBackend mirrors backend.IsSupportedType without importing the
// backend package (the registry must compile even if a new backend is
// added later and we want one source of truth).
func isKnownBackend(b string) bool {
	switch b {
	case "claude", "codex":
		return true
	}
	return false
}

func nullInt(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func unixMilli(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}
