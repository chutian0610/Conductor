# Testing & coverage

> **Source of truth:** all `*_test.go` under `server/`.
> Generated against the post-scenario-fill tree (this commit).

## How to run

```bash
cd server

make test          # unit + integration + auto-skipping live tests (~55s on macOS arm64)
make test-race     # same with -race (slow)
make test-cover    # emit coverage.out + coverage.html, print summary
make bench         # benchmarks (V1 has none yet)
```

The `Makefile` is at `server/Makefile`. `make help` lists every target
with a one-liner.

## Test layout

The tree splits into three layers that match the shape of the code
under test.

### 1. Pure-Go tests (always run)

Argv construction, MCP rendering, schema validation, and the
helpers that are too small / pure to deserve integration coverage.
Never need a CLI on PATH, so they run everywhere (CI, dev laptop
without Claude/Codex installed, Docker image).

| Suite | File | What it covers |
|---|---|---|
| `TestClaude_BlocklistFiltersCustomArgs` | `claude_test.go` | `buildClaudeArgs` against `claudeBlockedArgs`, using a `/bin/sh` wrapper to capture argv. |
| `TestCodex_BlocklistFiltersCustomArgs`  | `codex_test.go`  | `buildCodexArgs` against `codexBlockedArgs`, including the resume-vs-fresh flag asymmetry. |
| `TestCodex_McpConfig_*`                | `codex_mcp_test.go` | YAML → JSON → TOML rendering round-trip for the managed-MCP flow. |
| `TestSchema_*`                         | `configschema/schema_test.go` | `Schema.Parse` / `Validate` / `ToExecOptions` plus validation error shapes. |
| `TestProviderNeedsInlineSystemPrompt_*` `TestShouldFallbackToFreshSession_*` `TestResumeWithContinuityNotice_*` `TestRunContext_*` | `coverage_test.go` | Small pure helpers in `runtime_config.go` and `resume_fallback.go`. |
| `TestBinrunner_*`                      | `testbinaries/binrunner/binrunner_test.go` | The shared fake-CLI runner: flag parsing, argv writing, JSONL scenario emission. |

When the live CLIs are missing AND the fake-binary build is skipped,
these are the entire test surface that runs.

### 2. Integration tests (fake-binary driven; always run)

These exercise the full `Backend.Execute` lifecycle — preflight →
spawn → scanner → `finalizeStreamResult` → `Messages` + `Result` —
end-to-end, without a real Claude/Codex install. See
[ADR-0007](../adr/0007-fake-binary-not-pure-mock.md) for the
"why a fake subprocess instead of a Go mock" rationale.

- Harness: `server/internal/agent/testbinaries/`
  - `binrunner/` — the shared runner; reads JSONL scenarios, emits
    events, records argv, drains stdin, blocks on `--block`.
  - `fake-claude/main.go` and `fake-codex/main.go` — ~25-line
    wrappers that pick the flag names per backend.
- `testhelpers_test.go` — `MustBuildFakeBinary(t, name)` shells
  out to `go build -tags testbinaries` on demand (~1 s cold, ~0.2 s
  cached), `WriteScript` / `ReadArgv` for the test side.
- Per-backend suites:
  - `claude_integration_test.go` — 7 scenarios.
  - `codex_integration_test.go` — 3 scenarios + 3 pure-Go argv
    tests (co-located because they share the `containsArg` /
    `hasFlag` helpers).

Scenario matrix (the integration layer, by what each scenario
proves):

| Scenario | Backend | Proves |
|---|---|---|
| `_HappyPath` | both | Full lifecycle end-to-end: system init → assistant → result. Verifies `MessageStatus` pins `session_id` and `MessageText` carries the assistant content. |
| `_Cancel` | both | `context.Cancel()` propagates through the process group; subprocess dies within ~8 s; result status is `aborted`. |
| `_Timeout` | both | `opts.Timeout` produces a `timeout` status (or a fast return, both tolerated). |
| `_WritesStdin` (Claude only) | Claude | `writeClaudeInput` reaches the subprocess: fake's recorded stdin contains the prompt. |
| `_ControlRequest_AllowsAndForcesForeground` | Claude | `handleClaudeControlRequest` writes a `control_response` to stdin that flips `run_in_background:true → false` and replies `behavior:allow`. |
| `_ManagedMcpConfig_Lifecycle` | Claude | `writeMcpConfigToTemp` writes the JSON; `--mcp-config <path>` is on argv; `cleanupMcpConfigTemp` (via `pathSepParent`) removes both the file and its parent dir after `Execute` returns. |
| `_ResumeFallback_OnPermanentSessionLoss` | Claude | Fake emits a `result` with `is_error:true` and the substring "session not found" → `isResumeSessionGone` returns true → the retry loop clears `ResumeSessionID` and re-spawns; second attempt's argv does not carry `--resume thr-prev` anymore. |

These tests run under the normal `go test ./...` flow; no special
build tag is required to *run* them (the `testbinaries` build tag
only affects which `main.go` files the Go toolchain sees — the
integration test files themselves are unconditioned).

### 3. Live tests (auto-skip without the CLI)

Each suite has `TestClaude_Live_*` / `TestCodex_Live_*` tests
prefixed with `Live_`. They use:

```go
func requireClaude(t *testing.T) string {
    path, err := exec.LookPath("claude")
    if err != nil { t.Skip("claude CLI not installed; skipping live test") }
    return path
}
```

…and are skipped automatically when the binary is missing. They
remain the second source of truth for protocol shape — if the
fake ever drifts from the real CLI, the live tests catch it.

## Coverage baseline

Generated by `make test-cover` on the source tree at this commit.
Run again after changes to compare.

| Package | Coverage |
|---|---|
| `conductor/server/cmd/conductor`       | 0.0% — no tests (CLI entry only; behaviour is covered indirectly by the agent suites through `Backend.Execute`) |
| `conductor/server/internal/agent`      | 77.0% |
| `conductor/server/internal/agent/testbinaries/binrunner` | 46.4% — the harness's blocking-on-signal and stderr paths are not exercised in-process |
| `conductor/server/internal/configschema` | 76.9% |
| **Total statements**                   | **69.1%** |

Per-function highlights (low-coverage functions worth watching):

| Function | File | Coverage | Why it's low |
|---|---|---|---|
| `releaseProcessGroup`           | `proc_other.go`            | 0.0% | Non-Windows-only helper; integration tests exercise the surrounding flow but the helper's no-process-group path is unreachable on macOS |
| `claudeTerminalReasonFailure`   | `claude.go`                | 0.0% | Terminal-reason classification helper; needs a `result` event with `terminal_reason: "max_turns"` or similar |
| `claudeToolResultHasAsyncLaunch` + array + map variants | `claude.go` | 0.0% each | Async-launch detection; needs a `user` event with `tool_use_result.is_async_launch: true` |
| `Run`                          | `binrunner.go`             | 0.0% | The runner's `Run` calls `os.Exit` and is only covered by re-exec via the integration tests; in-process tests cover its helpers but not the top-level entrypoint |
| `drainStdin`                   | `binrunner.go`             | 0.0% | Background drain goroutine; covered indirectly by `_WritesStdin` and `_ControlRequest_*` (which poll the drain file), but `drainStdin` itself is never asserted on directly |
| `hideAgentWindow`              | `proc_other.go`            | 0.0% | macOS-only; non-Windows build does nothing here |
| `Load`                         | `configschema/schema.go`   | 0.0% | File-loader helper; tests exercise `Parse` directly |
| `handleClaudeControlRequest`   | `claude.go`                | 73.7% | Integration test covers the happy path; the JSON-unmarshal-fail branch is unreached |
| `writeMcpConfigToTemp`         | `claude.go`                | 62.5% | Success path covered; the two `os.WriteFile` / `os.MkdirTemp` error branches need a write-protected `/tmp` to drive |

## Known gaps

1. **Async-launch tool result handling.** `claudeToolResultHasAsyncLaunch`
   and its two array/map variants are at 0%. They detect when a
   Claude `user` event carries an async tool launch and route the
   result. Adding a `user`-event scenario in `claude_integration_test.go`
   would close this — but the runtime branch that consumes the
   detection is also untested, so this needs both a scenario and a
   review of what conductor does with the result.
2. **MCP temp-file error branches.** `writeMcpConfigToTemp` has two
   uncovered error paths (mkdir / writefile failure). Closing them
   needs a write-protected `TMPDIR` or a fault-injection hook on
   the test's `McpConfig` payload.
3. **`releaseProcessGroup` is unreachable on macOS without
   `setpgid`.** A platform-specific test that spawns under a
   custom process group and asserts the helper's signal would close
   the gap; deferred until we have a CI matrix covering Linux.
4. **cmd/conductor renderer is at 0%.** `renderMessage` /
   `renderResult` / `emitUsage` / `emitJSON` / `truncate` handle
   the user-facing CLI output. Pure stdlib output, mostly table-
   driven golden tests. Worth landing as a V1.x follow-up.

## Regenerating the report

```bash
make test-cover
open coverage.html
```

Both `coverage.out` and `coverage.html` are gitignored under
`coverage.*`; regenerate freely.
