# Codex CLI backend

> **Source of truth:** `server/internal/backend/codex.go`,
> `server/internal/backend/codex_mcp.go`,
> `server/internal/backend/resume_fallback.go`.

## Summary

`codexBackend` drives the Codex CLI in one-shot non-interactive mode:

```
codex exec --json [--approve-for-me] [-C <cwd>] [-m <model>]
                [-c model_reasoning_effort=<level>] ...
                [-- <prompt-positional>]
```

`codex exec resume --json [--m <model>] [-c model_reasoning_effort=X]
                       [-- <id> <prompt-positional>]`
…when `opts.ResumeSessionID` is set.

Why `exec --json` and not the "app-server" websocket mode: app-server
requires ChatGPT OAuth for the remote-control websocket, whereas
`exec --json` works with a plain `OPENAI_API_KEY` and the wire shape
is structurally identical to Claude's `stream-json` (which is why
`codex.go` mirrors `claude.go`).

## argv construction

`buildCodexArgs(opts)` assembles the argv in this order:

```
exec
[resume <ResumeSessionID>]                    # only when resuming
--json
--approve-for-me                              # fresh exec only
[-C <opts.Cwd>]                               # fresh exec only
[-m <opts.Model>]                             # both modes
[-c model_reasoning_effort=<opts.ThinkingLevel>]   # both modes
<codexFilterCustomArgs(opts.CustomArgs)>
```

Then the prompt is passed *positionally* — `codex exec --json ... "do
the thing"` — not on stdin.

### Why some flags don't survive into `codex exec resume`

Verified against `codex-cli 0.147.0`:

| Flag | Fresh exec | Resume | Note |
|---|---|---|---|
| `--json`            | ✓ | ✓ |
| `--approve-for-me`  | ✓ | ✗ — rejected as "unexpected argument"; the resume subcommand uses the original session's approval policy. |
| `-C <dir>`          | ✓ | ✗ — rejected; the resume subcommand uses the original session's cwd. |
| `-m <model>`        | ✓ | ✓ — overrides the session's model for this turn only. |
| `-c model_reasoning_effort=<level>` | ✓ | ✓ — per-turn config override. |

The asymmetry is hard-coded in `buildCodexArgs`: when `ResumeSessionID`
is set, only `-m` and `-c` are appended after `resume`. Everything else
in `CustomArgs` is then re-filtered through `codexBlockedArgs`.

## Blocked user flags

See [ADR-0006](../adr/0006-per-backend-blocked-args.md) for the design rationale.

`codexBlockedArgs` blocks anything that would re-shape the protocol
or shadow the running mode:

| Flag | Notes |
|---|---|
| `exec` / `resume`                | subcommands conductor already selected |
| `--json`                         | (with value for variants) |
| `--approve-for-me`               | both standalone and `-c approve_for_me=...` form |
| `-C`, `--cwd`                    | cwd override |
| `-m`, `--model`                  | model override |
| `-c`                             | takes a value, dropped together |
| `--sandbox`, `--profile`, `--oss`| V1.2 candidates (currently rejected — fresh-only knobs) |

`codexFilterCustomArgs` walks the user's `agent.args`, skipping both
the flag and its value for `-c` / `-m`. The model / thinking entries
conductor already injected via `buildCodexArgs` are kept; duplicates
from `CustomArgs` are removed so the user cannot fight conductor.

## MCP

Codex persists MCP config in `$CODEX_HOME/config.toml`. Conductor
intentionally goes through the file rather than argv because:

- argv would echo env-bearing secrets through `ps` / process listings
  on a multi-user box.
- Codex reads `config.toml` at startup; updating it between calls is
  the documented pattern.

`codex_mcp.go`:

1. Resolves `codexHome` from `opts.Env["CODEX_HOME"]`; falls back to
   `~/.codex`; errors if neither is available.
2. Renders the YAML `mcp.servers` list to TOML.
3. Writes `0o600` to `$CODEX_HOME/config.toml` (atomic write —
   `ensureCodexMcpConfig`).
4. Stashes the original file contents (under a sidecar, *not* via git)
   so we can restore on `cleanupMcpConfigTemp`.

`hasManagedCodexMcpConfig` returns true only when the user supplied
servers; an empty list leaves Codex's regular config untouched.

## Runtime config injection

`InjectRuntimeConfig(opts.Cwd, "codex", opts.SystemPrompt)` writes
`<cwd>/AGENTS.md` (mode `0o600`). Codex reads this natively. The CLI
gets a *positional* prompt argv argument that **does not** include the
brief (the file takes care of that); `providerNeedsInlineSystemPrompt`
returns `false`.

## Wire protocol

See [`../protocol.md`](../protocol.md#codex-cli--exec---json).

Dispatch happens in `handleCodexItem`, which fires for both
`item.started` and `item.completed`. The interesting corner: for
`command_execution`, Codex emits the `command` field only on
`item.started`. We cache it in a per-`item.id` map and replay it as
`MessageToolUse.Input` when the matching `item.completed` arrives.

`codexItem.Type` values we handle:

- `agent_message` → `MessageText` (the visible assistant turn)
- `command_execution` → `MessageToolUse` (on started) +
  `MessageToolResult` (on completed, with `aggregated_output` and
  `exit_code`)

Anything else is recorded as a `MessageStatus` ("item type=X") so it
shows up in the stream without dropping information.

## Resume and fallback

`codexBackend` is the only backend with a non-trivial resume story
(claude resumes via a single `--resume` flag; codex has a subcommand
plus config-asymmetry rules).

### What `codex exec resume` accepts

Per the validation table above, only model + thinking override.
Anything the user supplied in `agent.args` is filtered through
`codexBlockedArgs` *after* the `buildCodexArgs` prefix, so a stray
`-C` from a stale config cannot break a resume.

### Permanent-loss fallback

`resume_fallback.go` (`resumeWithContinuityNotice`) only fires when:

- The caller passed `ResumeExpected = true` (operator considers the
  session a soft hint, not a hard requirement).
- The first attempt fails with a *permanent* failure: session gone,
  schema drift, image too large, etc.

Fallback sequence:

1. Surface the original failure on `Message.Messages` (a
   `MessageError` event) so the UI can render what happened.
2. Re-spawn the backend in fresh-exec mode.
3. Prepend `ResumeContinuityNotice` to the spawned CLI's prompt
   (typically `"treat this as a fresh conversation; do not assume any
   prior context"`).
4. Resume the normal stream / result cycle from the new session.

When `ResumeExpected = false`, resume failures bubble up as the run's
terminal error and conductor exits non-zero.

## Termination grace

`codexTerminateGrace = 5 * time.Second` — same gap as Claude, applied
on cancellation via SIGTERM→SIGKILL across the process group.

## Test surface

`codex_test.go` and `codex_mcp_test.go` exercise:

- `buildCodexArgs` against the resume / fresh-exec flag matrices.
- `codexBlockedArgs` per-flag behaviour.
- `handleCodexItem` dispatch against the four item types.
- `codexMcpConfig` round-trip (YAML → TOML → Codex-friendly shape).

Tests are split into two layers: pure-Go tests
(argv construction, scanner dispatch, MCP rendering) run
unconditionally; `Test*_Live_*` tests run only when the real `claude`
(or `codex`) CLI is on `$PATH` — they `t.Skip` otherwise. **There is
no fake-binary harness today**; the `testhelpers_test.go` file only
provides a logger helper and a temp-file helper. Adding a
`testdata/fake-claude/main.go` + `testdata/fake-codex/main.go` paired
with a `*_integration_test.go` per backend is the recommended next
step to lift `internal/backend` coverage and close the gap between the
argv unit tests and the actual `Execute()` lifecycle.
