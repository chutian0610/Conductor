package agentregistry

// Schema DDL for the agent registry. Versioned via SQLite's
// user_version pragma so future migrations can branch without
// re-running init.
//
// The shape mirrors multica's `goal_manager.py`:
//   - agents   ≈ goals     (a registered entity the operator can launch)
//   - runs     ≈ tasks     (one execution of an agent)
//   - events   ≈ audit_log (one observation per agent event)
//
// We add `parent_id` / `parent_run_id` to express sub-agent spawn
// structure that multica captures only via env vars.
const (
	schemaVersion = 1

	schemaVersionPragma = `PRAGMA user_version = 1;`
)

const agentsSchema = `
CREATE TABLE IF NOT EXISTS agents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    description TEXT    NOT NULL DEFAULT '',
    backend     TEXT    NOT NULL,
    parent_id   INTEGER REFERENCES agents(id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    archived_at INTEGER
);
CREATE INDEX IF NOT EXISTS agents_backend ON agents(backend);
CREATE INDEX IF NOT EXISTS agents_parent  ON agents(parent_id);
CREATE INDEX IF NOT EXISTS agents_active  ON agents(archived_at) WHERE archived_at IS NULL;
`

const runsSchema = `
CREATE TABLE IF NOT EXISTS runs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id      INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    parent_run_id INTEGER REFERENCES runs(id) ON DELETE SET NULL,
    started_at    INTEGER NOT NULL,
    ended_at      INTEGER,
    status        TEXT    NOT NULL DEFAULT 'running',
    prompt_sha    TEXT    NOT NULL DEFAULT '',
    session_id    TEXT    NOT NULL DEFAULT '',
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    error         TEXT    NOT NULL DEFAULT '',
    usage_json    TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS runs_agent   ON runs(agent_id);
CREATE INDEX IF NOT EXISTS runs_session ON runs(session_id);
CREATE INDEX IF NOT EXISTS runs_status  ON runs(status);
`

const eventsSchema = `
CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,
    ts          INTEGER NOT NULL,
    kind        TEXT    NOT NULL,
    payload_json TEXT   NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_run ON events(run_id, seq);
`

// runAuditsSchema (v2) adds the audit-traversal table. Each row is one
// audit invocation — re-audits produce additional rows under the same
// run_id. The schema is independent of `runs` so we can mutate it
// (e.g. add an `attempt` column for retry numbering) without an
// agents/runs migration.
const runAuditsSchema = `
CREATE TABLE IF NOT EXISTS run_audits (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    verdict       TEXT    NOT NULL,
    evidence      TEXT    NOT NULL,
    auditor_model TEXT    NOT NULL,
    audited_at    INTEGER NOT NULL,
    input_sha     TEXT    NOT NULL,
    prompt_sha    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS run_audits_run ON run_audits(run_id);
`
