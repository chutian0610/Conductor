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
	// schemaVersion 2: agent rows carry the runtime-config columns
	// (instructions / runtime_config / custom_args / custom_env /
	// mcp_config / model / thinking_level) so the V2 HTTP surface
	// (POST /v1/agents etc.) can store a full multica-aligned agent
	// definition. v1 databases are ALTER TABLE'd up on first open
	// after this commit; see initSchema in store.go.
	schemaVersion = 2

	schemaVersionPragma = `PRAGMA user_version = 2;`
)

// agentsSchema describes the V1 baseline. The new runtime-config
// columns live in agentsColumnsV2Up below and are added by the
// gated migration in initSchema (store.go). Keeping the CREATE
// shape at V1 means fresh DBs and existing V1 DBs follow the same
// ALTER TABLE path on first open after this commit.
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

// agentsColumnsV2Up lists the ALTER TABLE statements that move a v1
// agents table to the v2 shape. ALTER TABLE ADD COLUMN is not
// idempotent in SQLite (errors if the column already exists), so
// initSchema runs these only when PRAGMA user_version < 2.
var agentsColumnsV2Up = []string{
	`ALTER TABLE agents ADD COLUMN instructions    TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN runtime_config  TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE agents ADD COLUMN custom_args     TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE agents ADD COLUMN custom_env      TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE agents ADD COLUMN mcp_config      TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN model           TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN thinking_level  TEXT NOT NULL DEFAULT ''`,
}

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
