package agentregistry

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver; registers as "sqlite"
)

// Driver name passed to sql.Open. Pure-Go, no CGo — see ADR-0003 for
// why this matters for the windows compile-time goal.
const driverName = "sqlite"

// registryDirName is the per-cwd directory holding registry.db.
const registryDirName = ".conductor"

// DefaultWALBusyTimeout mirrors multica's `busy_timeout = 10000`. Long
// enough that a concurrent reader does not stall a writer, short enough
// to surface lock contention to the caller.
const DefaultWALBusyTimeout = 10 * time.Second

// ErrNotFound is returned by Get* helpers when no row matches the ref.
// Callers map it to a friendly CLI message ("unknown agent 'foo'").
var ErrNotFound = errors.New("agentregistry: not found")

// ErrAmbiguousRef is returned when a ref string could be parsed as both
// a numeric id and a name; callers should prefer id-style references
// (e.g. "@42") when disambiguation matters.
var ErrAmbiguousRef = errors.New("agentregistry: ambiguous reference")

// Store is a SQLite-backed persistent catalog of agents, runs, and
// events. Safe for concurrent use by multiple OS processes sharing the
// same db file (WAL + busy_timeout).
//
// A nil *Store is never usable; always go through Open.
type Store struct {
	db        *sql.DB
	closeOnce bool
	file      string // "":memory:"  when in-memory, otherwise the on-disk path
}

// Open returns a Store rooted at cwd. The DB lives at
// <cwd>/.conductor/registry.db. An empty cwd selects an in-memory
// database (useful for tests).
//
// Open initialises the schema on first use and is idempotent.
func Open(cwd string) (*Store, error) {
	dsn, file, err := buildDSN(cwd)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("agentregistry: open: %w", err)
	}
	if file != "" {
		// Disk DB: bound the pool so we don't fan out unbounded.
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
	} else {
		// In-memory: single conn so pragmas + tables stay visible.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}
	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, file: file}, nil
}

// DBPath returns the on-disk path of the registry db. Empty when the
// store is in-memory.
func (s *Store) DBPath() string { return s.file }

// Close releases the underlying connection pool. Safe to call multiple
// times.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}


// buildDSN composes the modernc.org/sqlite DSN. For on-disk registries
// we use file:<path>?_pragma=... so each connection sees the same
// journal mode, busy timeout, and foreign-key settings.
func buildDSN(cwd string) (dsn string, file string, err error) {
	if cwd == "" {
		return ":memory:", "", nil
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", "", fmt.Errorf("agentregistry: resolve cwd: %w", err)
	}
	dir := filepath.Join(abs, registryDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("agentregistry: mkdir %s: %w", dir, err)
	}
	dbPath := filepath.Join(dir, "registry.db")
	dsn = fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		dbPath,
	)
	return dsn, dbPath, nil
}

// initSchema applies the DDL once per database. The first Open to a
// fresh db file sees user_version == 0 and runs the v1 schema; future
// migrations check user_version and branch from there.
func initSchema(db *sql.DB) error {
	if _, err := db.Exec(agentsSchema); err != nil {
		return fmt.Errorf("agentregistry: agents schema: %w", err)
	}
	if _, err := db.Exec(runsSchema); err != nil {
		return fmt.Errorf("agentregistry: runs schema: %w", err)
	}
	if _, err := db.Exec(eventsSchema); err != nil {
		return fmt.Errorf("agentregistry: events schema: %w", err)
	}
	// user_version carries our schema number; the pragmas above and
	// the three CREATE TABLE statements together form v1.
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		return fmt.Errorf("agentregistry: user_version: %w", err)
	}
	return nil
}
