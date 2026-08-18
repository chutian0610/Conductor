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

// initSchema applies the DDL idempotently and updates user_version
// to schemaVersion. Each block below is CREATE TABLE IF NOT EXISTS,
// so re-running against an already-migrated database is a no-op. The
// version branches (v1 → v2) only matter for skipping the write of
// user_version on databases that predate the constant change.
func initSchema(db *sql.DB) error {
	for _, ddl := range []struct {
		name, ddl string
	}{
		{"agents", agentsSchema},
		{"runs", runsSchema},
		{"events", eventsSchema},
	} {
		if _, err := db.Exec(ddl.ddl); err != nil {
			return fmt.Errorf("agentregistry: %s schema: %w", ddl.name, err)
		}
	}
	// v2 migration: run_audits table. Each fresh-DB open runs this;
	// an existing v1 DB moves to v2 on first open after this commit.
	if _, err := db.Exec(runAuditsSchema); err != nil {
		return fmt.Errorf("agentregistry: run_audits schema: %w", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = ` + fmtInt(schemaVersion)); err != nil {
		return fmt.Errorf("agentregistry: user_version: %w", err)
	}
	return nil
}

// fmtInt avoids a strconv import just to interpolate a constant.
func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + fmtInt(-n)
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
