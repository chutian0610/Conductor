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
| 1 | `cmd/conductor` renderer has 0% coverage — `renderMessage` / `renderResult` / `emitUsage` / `emitJSON` / `truncate` are pure stdout formatting | `docs/testing.md` "Known gaps" #4 | ~1.5h (table-driven golden tests) |
| 2 | Async-launch tool result handling — `claudeToolResultHasAsyncLaunch` + array/map variants all 0% | `docs/testing.md` "Known gaps" #1; `server/internal/agent/claude.go:724-770` | ~1h (one integration scenario + one scenario asserting the runtime branch) |
| 3 | `handleClaudeControlRequest` JSON-unmarshal-fail branch unreached (73.7% today) | `server/internal/agent/claude.go:561` | ~10m (malformed `request` event in a scenario) |
| 4 | `writeMcpConfigToTemp` error branches (mkdir / writefile) unreached | `server/internal/agent/claude.go:677` | ~20m (write-protected `TMPDIR` test) |
| 5 | `shouldFallbackToFreshSession` "permanent loss" is now covered end-to-end; the 14.3% gap is the empty-`ResumeContinuityNotice` arm that the integration layer can't drive (it short-circuits the retry before the second attempt). A unit test for it is in `coverage_test.go` already; nothing else to do. | `server/internal/agent/resume_fallback.go:36` | — (closed) |
| 6 | `binrunner.Run` + `drainStdin` are 0% (covered indirectly through the integration scenarios' polls) | `server/internal/agent/testbinaries/binrunner/binrunner.go:61,187` | ~30m (re-exec the test binary, like the existing `TestRun_RecordsArgvAndExits` does for the helpers) |
| 7 | `releaseProcessGroup` unreachable on macOS without `setpgid`; needs a Linux CI matrix to cover | `server/internal/agent/proc_other.go:35`; `docs/testing.md` "Known gaps" #3 | blocked on CI matrix |
| 8 | `providerNeedsInlineSystemPrompt` "default false" is asserted in `coverage_test.go`; new providers will need a one-line case + test | `server/internal/agent/runtime_config.go:31` | ~5m per provider |

## V1.x — feature gaps already in the design

Items the V1 code carries forward but does not surface to the
caller yet. Not bugs, just unfilled knobs.

| # | Item | Source |
|---|---|---|
| 9 | `MessageThinking` for `reasoning` content (V1.2 candidate) | `docs/protocol.md`, `docs/backends/codex.md:127` |
| 10 | Codex `--sandbox` / `--profile` / `--oss` are listed in the blocklist as "V1.2 candidates" — the code is wired up, the user-facing config is not | `docs/backends/codex.md:76` |
| 11 | The blocklist silently drops user args that conflict with conductor-owned flags. A future improvement: warn-and-error rather than silently dropping (noted in ADR-0006 consequences) | `docs/adr/0006-per-backend-blocked-args.md` |

## V2 — deferred by ADR

These have explicit ADRs saying "not in V1." Do not re-litigate;
the ADR owns the rationale.

| # | Item | ADR |
|---|---|---|
| 12 | HTTP transport layer (`internal/http/`, streaming event endpoint, lifecycle hooks) | [ADR-0001](adr/0001-v1-cli-only-no-http.md) |
| 13 | DAG scheduler that triggers runs through the HTTP layer | [ADR-0001](adr/0001-v1-cli-only-no-http.md) |
| 14 | `TaskPrompt` → `SystemPrompt` + per-turn task split (today the brief rides both disk and prompt) | [ADR-0005](adr/0005-brief-duplicated-disk-and-prompt-v1.md) "V2 plan" |
| 15 | Re-evaluate `codex app-server` (websocket) if it becomes the only Codex mode | [ADR-0002](adr/0002-codex-exec-json-not-app-server.md) |
| 16 | Windows process-group / Job Object support | [ADR-0003](adr/0003-refuse-windows-run-time.md) |
| 17 | New top-level YAML sections (`plan:`, `dag:`) | [ADR-0004](adr/0004-strict-yaml-schema.md) |
| 18 | CI workflow file that runs the integration layer (today the `Makefile` covers it; a GitHub Actions / similar config would make the harness explicit) | implicit — no ADR |

## Closing a row

When a row is closed:

1. Land the code/docs change.
2. Remove the row from this file in the same commit, or amend the
   table to point at the new state.
3. If the row is a V2 ADR item, mark the ADR `Superseded` rather
   than deleting it — the rationale is still useful.
