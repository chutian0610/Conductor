# V1 follow-ups

> **Status:** collected at V1 wrap-up. Each row is a one-line pointer
> to the file/section that owns the rationale — do not duplicate
> context here. Update the source when a row is closed.

V1 ships: a CLI that drives Claude Code and Codex CLI as
subprocesses, with a uniform `Message` / `Result` event stream,
`conductor.yaml` config, MCP integration, resume with permanent-
loss fallback, and an end-to-end integration test harness
(`docs/testing.md` and [ADR-0007](adr/0007-fake-binary-not-pure-mock.md)).

This page is the index for everything that did **not** make V1.

## V1.x — closeable in-tree, no new design

Concrete gaps that the current code knows about but does not yet
exercise or ship. Each row points at the source of truth.

| # | Item | Source of truth | Effort |
|---|---|---|---|
| 5 | `shouldFallbackToFreshSession` "permanent loss" is now covered end-to-end; the 14.3% gap is the empty-`ResumeContinuityNotice` arm that the integration layer can't drive (it short-circuits the retry before the second attempt). A unit test for it is in `coverage_test.go` already; nothing else to do. | `server/internal/backend/resume_fallback.go:36` | — (closed) |
| 7 | `releaseProcessGroup` unreachable on macOS without `setpgid`; needs a Linux CI matrix to cover | `server/internal/backend/proc_other.go:35`; `docs/testing.md` "Known gaps" #3 | blocked on CI matrix |
| 8 | `providerNeedsInlineSystemPrompt` "default false" is asserted in `coverage_test.go`; new providers will need a one-line case + test | `server/internal/backend/runtime_config.go:31` | ~5m per provider |
| 9 | `MessageThinking` for claude `thinking` / codex `reasoning` blocks — DEFERRED. Live probe (Claude Code 2.1.215) confirmed the main-agent thinking blocks are not forwarded to stdout in stream-json output regardless of `--effort medium`. The wire protocol supports them at the API level, but until Claude Code CLI ships a flag that surfaces them to stdout, the parser / renderer / event-store path is dormant. Re-introduce when the CLI exposes them — the wire paths are documented in `docs/backends/claude.md`. | see `internal/backend/claude.go` docstring at top of file (mentions the V1.1 + deferred state); `docs/backends/codex.md` and `docs/protocol.md` no longer reference MessageThinking | — |

## V1.x — feature gaps already in the design

Items the V1 code carries forward but does not surface to the
caller yet. Not bugs, just unfilled knobs.

| # | Item | Source |
|---|---|---|
| 10 | Codex `--sandbox` / `--profile` / `--oss` are listed in the blocklist as "V1.2 candidates" — the code is wired up, the user-facing config is not | `docs/backends/codex.md:76` |

## V2 — deferred by ADR

These have explicit ADRs saying "not in V1." Do not re-litigate;
the ADR owns the rationale.

| # | Item | ADR |
|---|---|---|
| 12 | HTTP transport layer (`internal/http/`, streaming event endpoint, lifecycle hooks) | [ADR-0001](adr/0001-v1-cli-only-no-http.md) |
| 13 | DAG scheduler that triggers runs through the HTTP layer | [ADR-0001](adr/0001-v1-cli-only-no-http.md) |
| 15 | Re-evaluate `codex app-server` (websocket) if it becomes the only Codex mode | [ADR-0002](adr/0002-codex-exec-json-not-app-server.md) |
| 16 | Windows process-group / Job Object support | [ADR-0003](adr/0003-refuse-windows-run-time.md) |
| 17 | New top-level YAML sections (`plan:`, `dag:`) | [ADR-0004](adr/0004-strict-yaml-schema.md) |

| 19 | Agent layer — persistent registry, runs, events, identity env vars (V1.x refresh: defaults + auto-register) | [docs/agent-layer.md](agent-layer.md) + [ADR-0008](adr/0008-agent-registry-persistence-and-identity.md) (superseded by Update log) | shipped |
| 21 | Adversarial audit loop: `conductor audit <run-id>` spawns a fresh LLM subprocess to audit a recorded run (multica analogue: `goal_manager.py run_audit`) | [ADR-0009](adr/0009-adversarial-audit-loop.md) | shipped |
| 22 | HTTP surface for the registry (V2 transport; ADR-0001 today forbids it) | [ADR-0001](adr/0001-v1-cli-only-no-http.md) | — |

## Closing a row

When a row is closed:

1. Land the code/docs change.
2. Remove the row from this file in the same commit, or amend the
   table to point at the new state.
3. If the row is a V2 ADR item, mark the ADR `Superseded` rather
   than deleting it — the rationale is still useful.
