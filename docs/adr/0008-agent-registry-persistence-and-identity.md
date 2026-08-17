# 8. Agent registry persistence and identity propagation

Date: 2026-08-17
Status: Superseded by the V1.x refresh — see "Update log" below.

The original "Accepted" decision captured the storage shape +
identity precedence that are still in force. The follow-on V1.x
refresh changed the **default behaviour** of `conductor run` and
added an auto-register policy; those edits live in the Update log
so future readers see the original ADR + the operative defaults
in one place.

## Context

V1 of Conductor defines a `Backend` seam in
`server/internal/agent/agent.go` that drives an LLM CLI
(Claude Code / Codex CLI) as a subprocess, parses the streaming JSON
protocol, and surfaces a uniform event stream + terminal result. That
package is correctly named *the backend driver* — it answers "how do I
drive Claude Code / Codex CLI as a subprocess?"

But Conductor doesn't yet answer the *agent layer* question: "who are
my agents, what runs have they done, what came out of each run, which
agent spawned which, and what audit trail exists for each run?" This
is the gap the user flagged after reading V1.

The reference design for this layer is multica's
`goal_manager.py` — a headless /goal state machine implemented as a
Python CLI over a SQLite store with WAL + busy_timeout, identity env
vars for parent/child distinction, and a CRUD surface around the
`goals / tasks / audit_log / audit_archive` tables.

Conductor needs the same shape, but the entities are different: an
**agent** is a registered, named entity (with a fixed backend and
optional parent); a **run** is one execution; and an **event** is one
emission on the streamed `agent.Message` channel.

## Decision

Add `server/internal/agentregistry` as the Conductor agent layer. It
is independent of `internal/agent` (the backend driver) — the two
packages answer different questions and both must stay small. The
naming explicitly preserves the user's terminology: **backend 层**
refers to the driver, **agent 层** refers to the registry.

### Schema

Three tables, versioned via SQLite's `user_version` pragma:

- `agents` (`id, name UNIQUE, description, backend, parent_id REFERENCES agents(id), created_at, updated_at, archived_at`) — registered entities with optional parent (sub-agent relationship).
- `runs` (`id, agent_id REFERENCES agents(id), parent_run_id REFERENCES runs(id), started_at, ended_at, status, prompt_sha, session_id, duration_ms, error, usage_json`) — one execution of an agent; `parent_run_id` is set on child invocations (future: sub-agent fork).
- `events` (`id, run_id REFERENCES runs(id), seq, ts, kind, payload_json`) — one observation per `agent.Message`. `seq` is monotonic per-run via `(MAX(seq) + 1)` so callers do not need to lockstep.

The store lives at `<cwd>/.conductor/registry.db`. An empty cwd selects
`:memory:` (tests only). Foreign keys are enabled; WAL + busy_timeout
mirror multica's setting (10 000 ms).

### Identity

Sub-agent identity is propagated by environment variables, matching
multica's precedence:

```
agent_id    : CONDUCTOR_AGENT_ID  > CONDUCTOR_PARENT_AGENT_ID  > ""
session_id  : CLAUDE_CODE_SESSION_ID > CODEX_THREAD_ID >
              CONDUCTOR_SESSION_ID > CONDUCTOR_PARENT_SESSION_ID > ""
```

The first two (backends' own session ids) come from the LLM CLI's
output — Conductor surfaces them via `agent.Result.SessionID`. The
"`CONDUCTOR_`" prefix is the new namespace; multica used
`CLAUDE_GOAL_*` because multica is Claude-Code-specific while Conductor
multi-backend. Conductor's CLI injects the parent identity into the
spawn via `agentregistry.IdentityEnv(...)` so the LLM can read its
parent without operator prop drilling.

### CLI surface

`conductor agent <subcommand>` is the user-facing surface; it is one
of `conductor`'s top-level commands alongside the existing `run`:

```
conductor agent list      [--backend <type>] [--all]
conductor agent show      <ref>
conductor agent register  <name> --backend <type> [--parent <ref>] [--description <text>]
conductor agent update    <ref> [--backend <type>] [--description <text>] [--parent <ref>|--clear-parent]
conductor agent archive   <ref>
conductor agent runs      [ref] [--status <status>] [--limit N]
conductor agent events    <run-id>
conductor agent run       <ref> --config <agent.yaml> [--prompt "..."] [--resume <sid>]
```

The top-level `conductor run` is unchanged so V1 callers see the same
behaviour. The new `conductor agent run <ref>` is a thin convenience
that wires the registry, the YAML config, and the recorder together.

### Recorder stance (best-effort)

The registry is *secondary* — the backend's terminal `agent.Result`
remains the source of truth (multica takes the same position:
`goal_manager.py` documents that the underlying Claude session is
truth and the SQLite store is audit). So `runRecorder.recordEvent`
and `runRecorder.finish` swallow write errors with a stderr log line;
they never abort the agent run. If SQLite is corrupt, the LLM still
finishes its work and the operator still sees the result.

### SQLite library

`modernc.org/sqlite@1.56.0` — pure Go, no CGo. This is compatible with
ADR-0003 (Windows refuses to run but compiles cleanly); CGo would force
a Windows build dep just to compile the registry, which ADR-0003
deliberately does not require.

### Migration plan

`user_version = 1` is the only version we ship today. Future schema
changes will branch on `PRAGMA user_version` and run an additive
migration inside `initSchema`. No DROP/CREATE rewrites; `ON DELETE
SET NULL` keeps historical Runs intact when an agent is archived.

## Consequences

Positive:

- Closes the "V1 is only the backend driver" gap (the user's
  reportable concern from review). The CLI gains a first-class
  persistent catalog without any HTTP transport (which ADR-0001
  defers to V2).
- Mirrors multica's vocabulary one-for-one so future "Conductor +
  multica" integrations have a shared conceptual model.
- Pure-Go SQLite means the registry compiles on Windows
  (ADR-0003-still-refuses-to-run) without extra CGo deps.

Negative:

- Naming is asymmetric: `internal/agent` (Backend driver) and
  `internal/agentregistry` (Agent layer). Tracked as followup #19
  in `docs/followups.md`; the rename would touch ~1500 LOC of tests
  and so is deferred.
- The recorder is best-effort, which means a flaky filesystem will
  silently drop audit rows. We surface this as a stderr log line;
  operators who need stronger guarantees should run `conductor agent
  list` + `conductor agent runs <ref>` to compare against the LLM's
  own session state.


## Update log

### 2026-08-17 — V1.x refresh: defaults + auto-register + ANSI cleanup

Subsequent work landed the following changes; the original "Accepted"
text above is preserved for the storage / identity mechanics it
documents, but the *operational defaults* below supersede anything
that the original decision implied about V1 caller behaviour.

#### (a) `--no-record` opt-out (V1.x)

The original ADR said "the top-level `conductor run` is unchanged so
V1 callers see the same behaviour". That promise was broken in the
V1.x refresh: `conductor run` now records by default. Pass
`--no-record` to keep the original V1 behaviour (no DB write, no
auto-register). See `docs/agent-layer.md` → "Default behaviour
(V1.x+)" for the rationale.

#### (b) Auto-register on first sight

`conductor run` now resolves the YAML's `agent.name` against the
registry. On miss, the agent is auto-registered with `Name +
Backend + Description` from the YAML (the YAML's `description`
lands only on first register; subsequent runs do not overwrite).
Operators who want to revise metadata use `conductor agent
update <ref>`.

This is implemented via `agentregistry.EnsureAgent(ctx, Agent)` —
an idempotent Get-or-Register helper. On hit the existing row is
returned unchanged; on miss a fresh row is inserted.

#### (c) Shared `executeBackend(...)` helper

`conductor run` and `conductor agent run <ref>` now share the same
wire-format path via a top-level `executeBackend(...)` helper. The
only difference is recorder lifecycle ownership:

- `doRun` (V1.x default) opens the registry, owns its Close()
  lifecycle, and constructs the recorder internally.
- `runWithRecorder` (was `doRunRecorded`) is a thin wrapper that
  injects identity env into the spawned CLI and delegates to
  `executeBackend`.

#### (d) Identity env re-confirmed

`CONDUCTOR_AGENT_ID > CONDUCTOR_PARENT_AGENT_ID` and
`CLAUDE_CODE_SESSION_ID > CODEX_THREAD_ID > CONDUCTOR_SESSION_ID >
CONDUCTOR_PARENT_SESSION_ID` precedence chains remain in force.
New helper `agentregistry.IdentityEnv(agentID, runID, sessionID)`
returns the env fragment the CLI injects on spawn; sub-agent runs
still automatically pick up the parent run id via
`CONDUCTOR_PARENT_RUN_ID`.

#### (e) ANSI cleanup at the renderer boundary (narrow scope)

A new `internal/ansiclean` package ships a single helper:

- `Strip(s)` — removes ANSI / CSI / OSC escape sequences
  (`<ESC>[...m`, `<ESC>]...<BEL>`, single-byte escapes).

`renderMessage` and `emitUsage` in `cmd/conductor/main.go` apply
this so the operator's terminal shows clean output. The underlying
`Result.Usage` / `Message` payloads stay raw — replay from the
events table preserves the original bytes.

What Strip does NOT do: it does not strip bracket-y content, even
when the bracket looks SGR-shaped (`[<digits>m`). Claude uses
bracketed suffixes to distinguish same-named model variants
(`MiniMax-M3[1m]`), and an early draft of `internal/ansiclean`
that stripped these orphans was rolled back. The renderer
conservatively leaves brackets alone; only ESC-prefixed terminal
control codes are removed.

#### Failure-semantics clarification

The registry remains *secondary* — write failures (SQLite locked,
read-only fs, etc.) are logged to stderr and the agent run
completes regardless. The original ADR's "registry is audit" stance
is preserved; `doRun` simply moved the open to a point where the
auto-register side effect can be controlled via `--no-record`.

## See also

- [`docs/backend.md`](../backend.md) — the Backend driver seam.
- [`docs/agent-layer.md`](../agent-layer.md) — the user-facing guide
  to the agent layer (CLI reference + recipes).
- ADR-0001 (V1 is CLI-only; no HTTP/transport).
- ADR-0003 (Windows refuse-to-run).
- ADR-0004 (strict YAML schema; the registry re-uses the same
  schema-friendly directive style via clear-error-returning helpers).
- ADR-0006 (per-backend blocked args; `isKnownBackend` in the
  registry mirrors the same allowlist).
