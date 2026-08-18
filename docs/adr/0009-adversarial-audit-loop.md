# 9. Adversarial audit loop for recorded runs

Date: 2026-08-18
Status: Accepted (V2)

## Context

The agent registry (ADR-0008) records every run's events: prompts,
tool uses, tool results, assistant text, the terminal `Result`. But
it does not **judge** those events. The operator can `cat` a JSON dump
of one run but cannot ask "was this run actually correct?" without
re-reading the whole transcript themselves.

Multica's `goal_manager.py run_audit` does this with a fresh LLM
subprocess: read the recorded task, spawn a *different* agent (the
"auditor"), feed the transcript, and parse a structured verdict.
Conductor has the same shape waiting — full events are in SQLite,
the backend driver (`internal/backend`) is reusable, and we already
have a clean separation between data and judgment.

This ADR captures that and the boundary of what the auditor is
allowed to do. It is a new ADR because the audit introduces storage
shape, a CLI surface, and a verdict contract — each worth pinning
down before code is written.

## Decision

### 1. New CLI surface

```
conductor audit <run-id> [--force] [--json] [--model <model>]
conductor audit --pending [--limit N]
```

`<run-id>` is the integer `runs.id` (operators get this from
`conductor agent runs <agent>`). `--force` re-audits a previously
audited run; without it, the command refuses with a non-zero exit
so a CI loop can detect the case. `--pending` is read-only and lists
runs in `runs` that have no row in `run_audits` yet, so operators
can drain the backlog after a batch run.

The default model is the same one the original run used (carried
in `runs.session_id`'s backend session metadata; for now we
re-read from the events). `--model` overrides it. V2 supports
**`claude` only**; `codex` is documented as out-of-scope for this
ADR (see Non-goals).

### 2. New table — `run_audits`

A separate table, not columns on `runs`. Operators can re-audit
(`--force`) and the audit trail grows; multiple rows per run are
the natural shape.

```sql
CREATE TABLE IF NOT EXISTS run_audits (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id         INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    verdict        TEXT    NOT NULL,           -- pass | fail | unverifiable
    evidence       TEXT    NOT NULL,           -- 1–3 sentence reason
    auditor_model  TEXT    NOT NULL,           -- backend's reported model name
    audited_at     INTEGER NOT NULL,           -- unix ms
    input_sha      TEXT    NOT NULL,           -- sha256 of fed events
    prompt_sha     TEXT    NOT NULL            -- sha256 of auditor system prompt
);
CREATE INDEX IF NOT EXISTS run_audits_run ON run_audits(run_id);
```

Schema version bumps from 1 → 2. The migration is the
`CREATE TABLE IF NOT EXISTS` line above — idempotent on first open.

### 3. Auditor prompt contract

The auditor is spawned fresh with two prompts:

- **`SystemPrompt`** (persisted to `<cwd>/CLAUDE.md` via the existing
  `InjectRuntimeConfig` path): a short instruction sheet that tells
  the auditor it is reviewing another agent's work, lists the
  verdict values, and demands output as a single JSON line with
  shape `{"verdict": "<pass|fail|unverifiable>", "evidence": "..."}`.
- **`prompt` argument to `Execute`** (written to stdin as the user
  message): the rendered audit context — a header block (run
  metadata: status, duration, error if any) followed by the full
  stream of `agent.Message` events with their `Type / Content /
  Tool / Input / Output / Error` joined onto stable event IDs.

`--disallowed-tools ''` is set so the auditor never invokes tools;
the verdict is computed from the transcript, not from probing the
filesystem or running further commands.

Output is read from the terminal `Result.Output` once claude exits.
The auditor's first / only line of output is expected to be a JSON
object matching the contract above.

### 4. Verdict parsing

A small parser that extracts the first line beginning with `{` and
ends with `}` and runs `json.Unmarshal` against `{"verdict",
"evidence"}`.

- Parse OK with one of the three allowed verdicts → persist that
  row, return success.
- Parse OK with an unknown verdict (e.g. `"maybe"`) → persist the
  row with `verdict = "unverifiable"` and the original evidence, log
  a warning. Don't silently drop.
- Parse fails → persist `verdict = "unverifiable"` with `evidence =
  "auditor returned malformed JSON: <line>..."`. Same outcome as
  unknown verdict.

Three explicit outcomes, no fourth category. Unknown verdicts fall
back to unverifiable on purpose (the auditor did its job, it's
just a verdict shape we didn't enumerate).

### 5. Trust model — judgment, **never** enforcement

The audit verdict is **information, not a gate**. It does not:

- change the run's terminal `status`,
- block re-issuing the run,
- gate any subsequent `conductor run` against the same agent,
- surface as a CI required-check.

Operators read `conductor audit <run-id>` and decide. The decision
to escalate to a different LLM, retry the run, or discard the agent
is theirs.

This is a deliberate refusal to build a feedback loop that the
operator hasn't asked for. The multica equivalent is the same:
`run_audit` produces a record; it doesn't take action. If we ever
need enforcement, that's a separate ADR — likely V3.

### 6. CLI contract — both human and machine

Default (TTY): the audit row is summarised on stderr:

```
audited run #42  agent=reviewer  model=claude-sonnet-4-5  → fail
  evidence: tool Bash failed once with exit 2, but the agent
            repeated it four more times before giving up.
```

`--json` (machine / CI use): one JSON object per line on stdout:
`{"run_id": 42, "verdict": "fail", "evidence": "...", "audited_at": ...,
"auditor_model": "...", "audit_id": 7}`. Stderr is quiet.

Both modes persist the same row. Exit code: 0 on success
(including `verdict = unverifiable`), non-zero only on infrastructure
failure (registry open, claude binary missing, etc.). The verdict
itself never drives exit code.

## Non-goals

- **codex backend** — the auditor would need a different prompt
  wiring (codex uses `-c reasoning_effort=...` not `--disallowed-tools ''`)
  and a different output-shape contract. Land alongside if there's
  demand; for now claude-only.
- **Streaming verdict** — a one-shot audit is enough; live verdict
  is what `conductor run --verbose` already gives.
- **Auto-audit-on-run** — `conductor run` does not call the auditor
  on completion. Operators trigger audits explicitly. Auto-audit
  is a separate ADR if/when it becomes wanted (likely with `--policy`
  on the agent registry).
- **Cross-run audits** ("did this agent consistently over-reach?") —
  not in this ADR; would need its own data shape.
- **Dispute flow** ("second auditor dissents") — out.
- **Per-run audit policy metadata** on `agents` (e.g. `audit:
  strict`) — out, deferred.

## Consequences

Positive:

- Operator gets a one-shot "did this run go okay" answer without
  re-reading transcript. For agent-layer users this is the
  biggest single upgrade to the registry's value.
- Re-auditable (`--force`) — operator can re-run with a different
  model or a fresh prompt tweak without losing the previous verdict.
- Persistence in SQLite means audits travel with the rest of the
  registry, so backup / replay also capture the audit trail.
- Pure CLI — no ADR-0001 lift required; audit is part of V2's
  CLI-only footprint.

Negative:

- Adds a new CLI surface; `conductor --help` now has 9 subcommand
  groups. YAGNI pressure applies to each — keep the test list in
  `auditcmd.go` narrow.
- Audit prompt + verdict contract are conjoined: changing either
  is an API break for `--json` consumers (also triggers the
  unknown-verdict → unverifiable fallback, so old verifiers don't
  silently lie).
- Storage migration v1 → v2 — registered users on a fresh
  database are unaffected (CREATE IF NOT EXISTS is a no-op). Users
  importing an existing v1 db get the new table on first
  `Open()`; the 0.5 ms cost is well under `openRecorderForRun`'s
  per-run budget.

## See also

- ADR-0008 — registry persistence + identity env (the data being
  audited).
- `docs/agent-layer.md` (added: Audit section) — operator guide for
  the new CLI commands.
- Followups row #21 — the closure note for the row this ADR
  resolves.
