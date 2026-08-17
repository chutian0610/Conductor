# 7. Integration tests use a fake subprocess binary, not a pure-Go mock

Date: 2026-08-17
Status: Accepted

## Context

Conductor's V1 test surface was missing the most important layer: the
`os/exec` boundary itself. Pure-Go tests covered argv construction,
MCP rendering, and schema validation, and `Test*_Live_*` covered
the full lifecycle — but only when the real `claude` / `codex` CLIs
were on PATH. CI runs without those CLIs, so the cancellation,
timeout, pause, and resume-fallback paths were dark.

Two ways to fill the gap:

1. **Pure-Go mock.** Extract a `subprocessRunner` interface, fake
   it in tests, return canned JSONL.
2. **Fake binary.** Compile a tiny `fake-claude` / `fake-codex` that
   reads a JSONL scenario file and emits the events. Build it on
   demand with `-tags testbinaries`; tests point
   `Config.ExecutablePath` at the resulting executable.

The "obvious" choice is option 1 — it is the standard Go pattern,
faster (no process spawn), and easier to set up. Option 2 looks
like more machinery for the same coverage.

## Decision

Option 2 — a real fake subprocess — and **no** new abstraction in
the production path. The harness is:

- `internal/agent/testbinaries/binrunner` — the shared runner. Reads
  a JSONL script of `{delay_ms, event}` steps, emits each event as
  one JSONL line, records argv, drains stdin to a file, blocks on
  `--block`, exits with `--exit=N`.
- `internal/agent/testbinaries/fake-claude/main.go` — wraps the
  runner with the Claude flag names (`--script`, `--argv`, etc.).
- `internal/agent/testbinaries/fake-codex/main.go` — same, with
  codex flag names.
- `MustBuildFakeBinary(t, name)` in `testhelpers_test.go` — calls
  `go build -tags testbinaries -o <tmpdir>/<name>` on demand. Build
  is ~1 s on a warm cache.
- `*_integration_test.go` per backend — the end-to-end suites that
  point `Config.ExecutablePath` at the fake and run 5–6 scenarios
  per backend (happy path, stdin cancellation, timeout, pause,
  resume-fallback, resume-success).

The production code is unchanged. There is no
`type subprocessRunner interface` in `claude.go` / `codex.go`; the
fakes speak the real `os/exec` boundary.

## Consequences

Positive:

- **The thing that is being tested is the thing that matters.** The
  `os/exec` boundary — process group setup, `SIGTERM` delivery,
  stdin pipe closing, exit-code propagation, line-buffered scanner
  — is the *whole point* of Conductor's subprocess architecture. A
  pure-Go mock would prove that `Backend.Execute` calls the runner
  it was given, not that it actually drives a CLI. The fake
  exercises the real production code path end-to-end.
- **Wire format is shared with the real CLI.** A real Claude
  `stream-json` line and a fake-Claude `stream-json` line are
  byte-identical; both flow through the same `bufio.Scanner` and
  the same `handleClaudeAssistant` / `handleCodexItem` dispatch.
  Regressions in the event handlers fire from the integration
  tests, not just from hand-crafted unit tests.
- **Tests assert on what the binary *received*.** `--argv` writes
  the real argv to a file the test then reads. This catches the
  class of bug the original `claude_test.go` flag-construction
  tests already caught, but through the full pipeline (build →
  spawn → record) rather than just `buildClaudeArgs` in isolation.
- **No production-code abstraction tax.** Option 1 would have added
  a `subprocessRunner` interface and a default implementation that
  shells out — the interface would be the only thing the production
  code calls, the implementation would be the only thing the tests
  bypass. Code that exists solely to be mocked is a maintenance
  burden with no production value.
- **One harness, two backends, six scenarios each.** Both
  `fake-claude` and `fake-codex` are ~25 lines of wrapper; the
  shared `binrunner` is the only place scenario parsing lives.

Negative:

- **Build cost.** Each `MustBuildFakeBinary` call shells out to
  `go build -tags testbinaries`. A fresh build is ~1 s, cached
  builds are ~0.2 s. `claude_integration_test.go` and
  `codex_integration_test.go` each call it 5–6 times — total cost
  is ~3 s, paid in parallel. Within budget for `make test` (~50 s
  total today, ~53 s with the integration tests). If the cost
  becomes painful, a `TestMain` cache the path; the harness is
  already shaped for that.
- **`go build` must be on PATH inside tests.** `go test` already
  requires this; no new constraint.
- **The fake is a separate binary that can drift from the real
  CLI.** Mitigated by reusing the real JSONL wire format and by
  keeping the live `Test*_Live_*` tests as the second source of
  truth for protocol shape.

## Alternatives considered

- **Pure-Go interface-based mock.** Faster, but the test stops at
  the seam. Cancellation, SIGTERM-to-process-group, and stdin-pipe
  behaviour are exactly the bugs the harness is meant to catch, and
  they live below the seam.
- **`httptest`-style fake HTTP server.** Rejected because neither
  real CLI speaks HTTP. Conductor is a subprocess wrapper, not a
  client.
- **Recorded golden transcripts of the real CLI.** Tempting, but
  large, brittle, and bakes in third-party behaviour we don't
  control. The fake lets each test author the exact scenario it
  cares about.
- **Refactor `Backend.Execute` to take an `io.Reader`/callback for
  the subprocess output.** This is a real refactor with a real
  cost (it changes the production type signature to suit tests)
  for marginal benefit (the fake already covers the seam).

## See also

- `server/internal/agent/testbinaries/binrunner/` — the runner.
- `server/internal/agent/testhelpers_test.go` —
  `MustBuildFakeBinary`, `WriteScript`, `ReadArgv`.
- `server/internal/agent/claude_integration_test.go` and
  `codex_integration_test.go` — the suites this ADR enabled.
- `docs/testing.md` — current coverage baseline (post-fake-binary).
- [ADR-0006](0006-per-backend-blocked-args.md) — the blocklist
  that the integration tests now verify by reading the fake's
  argv file.
