# Agent layer

The Conductor **agent layer** is the persistent catalog of *agents*
(registered entities) and the *runs* each one has produced. It sits
above the backend driver:

| | backend driver | agent layer |
|--|--|--|
| package | `server/internal/agent` | `server/internal/agentregistry` |
| question | how do I drive Claude Code / Codex CLI as a subprocess? | who are my agents, what runs did each one do, what events came out, and who spawned whom? |
| surface  | `Backend` interface used by `conductor run` | persistent SQLite store exposed as `conductor agent ...` |
| multica analogue | (none — multica is just the goal manager) | `goal_manager.py` (`goals` ↔ `agents`, `tasks` ↔ `runs`, `audit_log` ↔ `events`) |

See [ADR-0008](adr/0008-agent-registry-persistence-and-identity.md)
for the design rationale.

## Quick start

```bash
# Either path is fine — explicit register OR auto-register on first run.

# (a) Explicit register, then `conductor agent run <ref>`.
conductor agent register reviewer --backend claude --description "…"
conductor agent run reviewer --config examples/code-review-agent.yaml --prompt "…"

# (b) Just run with the regular V1 command — the registry records the
#     run by default, auto-registering the agent on first sight. Pass
#     --no-record to opt out of both the registry write and the
#     auto-register.
conductor run --config examples/code-review-agent.yaml --prompt "…"

# Inspect what happened (works for either path).
conductor agent runs reviewer
conductor agent events 1            # run id from the previous line
```

Each `conductor agent run` is one row in `runs`. The full event stream
from the `agent.Message` channel is persisted as JSON in `events`,
ordered by `seq`, one row per message. The terminal `agent.Result`
(Status / DurationMs / SessionID / Usage) lands in `runs.ended_at`,
`runs.duration_ms`, `runs.status`, etc. when the LLM finishes.


## Default behaviour (V1.x+)

As of `conductor run`'s V1.x refresh, the V1 subcommand records runs to
the registry by default. The semantics:

- **First `conductor run` with a new `agent.name`** → auto-registers an
  agent using the YAML's `name`, `backend`, and `description`.
- **Subsequent `conductor runs`** with the same name → reuse the
  existing row (no overwrite of description etc.). Operators who want
  to revise metadata use `conductor agent update <ref>`.
- **`--no-record`** disables both the registry write and the
  auto-register step. Use it for one-off ad-hoc invocations.
- **Registry failure is non-fatal** — if SQLite is locked or the
  filesystem is read-only, the run still completes and the failure
  is logged to stderr. The registry is audit; the LLM's own session
  is truth.

This mirrors multica's stance: the underlying session result remains
authoritative, the SQLite store is the audit.

`conductor run` exposes extended thinking through the same events
pipeline: `MessageThinking` rows land in the `events` table alongside
text/tool events, so an audit later can recover the model's reasoning
alongside its outputs. The renderer renders thinking as
`... <truncated-N-chars>` on stderr (default `thinkingPreviewChars`
in `cmd/conductor/main.go`). Pass `--quiet` to suppress.


## Persistence model

The registry is a SQLite database at `<cwd>/.conductor/registry.db`:

```
.conductor/
└── registry.db   # WAL, foreign_keys=on, busy_timeout=10000ms
```

Schema (three tables, versioned via `PRAGMA user_version = 1`):

- **`agents`** — registered entities. `name` is unique; `parent_id`
  is a self-FK for sub-agent structure; `archived_at` is the soft-
  delete tombstone.
- **`runs`** — one execution of one agent. `parent_run_id` is a
  self-FK for sub-agent runs (future); `session_id` is the backend-
  stable id (Claude `system` → `session_id`, Codex `thread.started`
  → `thread_id`); `usage_json` carries the per-model token usage.
- **`events`** — one observation per streamed message. `seq` is
  monotonic per-run, assigned in SQL via
  `(MAX(seq) + 1)` so concurrent callers do not collide.

Use `:memory:` in tests (the package exposes this by passing `""` to
`Open`).

## Identity propagation (parent/child)

Identity is propagated by environment variables, mirroring multica's
precedence chain:

```
agent_id    : CONDUCTOR_AGENT_ID  > CONDUCTOR_PARENT_AGENT_ID
session_id  : CLAUDE_CODE_SESSION_ID > CODEX_THREAD_ID >
              CONDUCTOR_SESSION_ID   > CONDUCTOR_PARENT_SESSION_ID
```

`conductor agent run` automatically injects the parent identity
(`CONDUCTOR_PARENT_RUN_ID=<run-id>`,
`CONDUCTOR_PARENT_SESSION_ID=<backend-session-id>`) into the spawn so
the LLM can read its own lineage without the operator prop drilling.
See `agentregistry.IdentityEnv(...)` for the helper.

## CLI reference

```
conductor agent list      [--backend claude|codex] [--all] [--json]
conductor agent show      <ref>             [--json]
conductor agent register  <name> --backend <type> [--parent <ref>] [--description <text>]
conductor agent update    <ref> [--backend <type>] [--description <text>] [--parent <ref>|--clear-parent]
conductor agent archive   <ref>
conductor agent runs      [<agent-ref>] [--status <status>] [--limit N] [--json]
conductor agent events    <run-id>                       [--json]
conductor agent run       <ref> --config <agent.yaml> [--prompt "..."] [--resume <sid>]
                          [--json] [--quiet] [--no-record]
```

`<ref>` is either a `name`, a numeric id, or `@<id>`. Names are
preferred for scripts.

`--no-record` runs the agent without writing to the registry. Useful
for ad-hoc invocations that should not pollute the audit trail.

## Best-effort recorder

The recorder is **secondary** — the backend's terminal result is the
source of truth (same stance as multica). Write failures are logged
to stderr and never abort the agent run. If you need stronger
guarantees, reconcile the registry against the LLM's own session
via `session_id`.

## Verification

```bash
go test ./internal/agentregistry/...
go build ./...                            # CLI builds with the
                                          # registry wired into
                                          # `conductor agent ...`
```

End-to-end smoke (already in `internal/agentregistry/agentregistry_test.go`):

```go
s, err := agentregistry.Open("")           // :memory:
id, _  := s.RegisterAgent(ctx, Agent{Name: "reviewer", Backend: "claude"})
runID, _ := s.StartRun(ctx, Run{AgentID: id, PromptSHA: ParsePrompt("...")})
s.AppendEvent(ctx, runID, "text", []byte(`{"content":"hi"}`))
s.FinishRun(ctx, runID, RunFinish{Status: "completed", DurationMs: 100})
```

## Roadmap

The agent layer enables three planned follow-ups; see
[`docs/followups.md`](followups.md) items #19, #20, #21:

- **#20** — adversarial audit loop. Multica spawns a fresh
  `claude -p` subprocess to audit a goal; the equivalent for Conductor
  is a `conductor audit <run-id>` command that runs a separate
  LLM task against the recorded events.
- **#21** — HTTP surface. The CLI surface is enough for V1.x;
  V2 (ADR-0001) will add JSON endpoints over the same registry.
