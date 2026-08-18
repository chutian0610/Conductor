# 1. V1 is CLI-only; no HTTP server until V2

Date: 2026-08-17
Status: Superseded by [ADR-0010](0010-v2-http-transport.md) for the V2 HTTP surface. The V1 CLI mode (single `run` subcommand, no HTTP listener) remains valid; this ADR's lifting applies to V2 daemon mode (the new `serve` subcommand + REST/SSE surface).
## Context

Conductor's job is to drive an LLM CLI as a subprocess and surface
the wire protocol as a uniform event stream. V1 was scoped to
validate the **subprocess boundary + event stream + config schema**
end-to-end.

An HTTP layer would let external systems (UI, scheduler, IDE plugin)
trigger and observe runs. A DAG scheduler would let runs chain into
plans. Both are real value, but they were explicitly deferred to V2.

## Decision

`cmd/conductor` exposes a single cobra command (`run`) with no HTTP
listener. The binary's only job is:

```
conductor run --config <agent.yaml> [--prompt "..."] [--resume <id>]
               [--json] [--quiet]
```

UI / scheduler / remote trigger land in V2, when the DAG scheduler
needs an external entry point.

## Consequences

Positive:
- V1 scope stays small enough to ship and verify; no transport-layer
  code paths to maintain alongside the protocol work.
- `conductor run` is a scriptable unit — CI, cron, git hooks all run
  it without an HTTP stack under them.

Negative:
- V2 will need to thread `Backend` calls through an HTTP boundary;
  until then, "remote triggers" can only be shell-out.
- The current CLI shares an output convention (stderr for events,
  stdout for result) that the HTTP layer must mirror — see
  [the renderer in `cmd/conductor/main.go`](../../server/cmd/conductor/main.go).

## Follow-ups

- V2 HTTP: `internal/http/`, streaming event endpoint, lifecycle hooks.
- DAG scheduler: a trigger that *uses* the HTTP layer to spawn runs.
