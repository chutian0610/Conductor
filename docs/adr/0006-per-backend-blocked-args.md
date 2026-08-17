# 6. Per-backend blocklist prevents user `args` from overriding protocol-owned flags

Date: 2026-08-17
Status: Accepted

## Context

`conductor.yaml` lets the user pass extra CLI flags via
`agent.args: [...]`. Without guardrails, a user could put
`--output-format text` or `--model` into `args`, and Conductor's
internal protocol contract (stream-json, model picker, permission
mode) would silently break — Conductor's scanner would receive
mismatched output and the run would emit nonsense.

## Decision

Each backend maintains a `*BlockedArgs` map of flags it owns:

```go
// internal/agent/claude.go
var claudeBlockedArgs = map[string]struct{}{
    "-p": {}, "--output-format": {}, "--input-format": {},
    "--permission-mode": {}, "--strict-mcp-config": {},
    "--model": {}, "--max-turns": {}, "--mcp-config": {},
    "--session-name": {}, "--resume": {}, "--continue": {},
    "--fork-session": {},
}

// internal/agent/codex.go
var codexBlockedArgs = map[string]struct{}{
    "exec": {}, "resume": {}, "--json": {},
    "--approve-for-me": {}, "-C": {}, "--cwd": {},
    "-m": {}, "--model": {}, "-c": {},
    "--sandbox": {}, "--profile": {}, "--oss": {},
}
```

`*FilterCustomArgs(opts.CustomArgs)` walks the user list, skipping
any entry in the blocklist (and, for value-taking flags, the value
that follows it). The filter is silent — there is no warning to the
user. Conductor still emits the conductor-owned flag from
`buildClaudeArgs` / `buildCodexArgs` so the protocol contract holds.

## Consequences

Positive:
- The streaming protocol cannot be silently broken by an agent.yaml.
- Operators get a stable interface: Conductor always knows what argv
  shape to expect from the subprocess.

Negative:
- A user who *wants* to override `--model` (e.g. for an evaluation run)
  cannot do so via `agent.args`. They must use `agent.model:` instead.
  `agent.model:` is the supported path; the blocklist is just the
  last-line-of-defence against typos.
- Silent drops mean a malformed agent.yaml "works" but quietly
  ignores half its `args`. (A future improvement: warn-and-error on
  blocked entries rather than silently dropping them.)

## See also

[`docs/backends/claude.md`](../backends/claude.md) and
[`docs/backends/codex.md`](../backends/codex.md) for the full
per-backend blocklist tables.
