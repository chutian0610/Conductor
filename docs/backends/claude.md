# Claude Code backend

> **Source of truth:** `server/internal/agent/claude.go`.

## Summary

`claudeBackend` (`internal/agent/claude.go`) implements `agent.Backend`
by spawning the Claude Code CLI in non-interactive mode with streaming
JSON in/out. Conductor drives everything via stdin / stdout so the
process is fully headless.

## argv construction

`buildClaudeArgs(opts)` always opens with these baseline flags:

```
-p
--output-format stream-json
--input-format  stream-json
--verbose
--permission-mode bypassPermissions
--disallowedTools AskUserQuestion
```

The intent:

- `-p` — non-interactive "print mode" (no REPL).
- `--output-format stream-json --input-format stream-json` — line-
  delimited JSON on both pipes.
- `--verbose` — request the `system` init event so we can capture
  `session_id` and `model` for the terminal `Result`.
- `--permission-mode bypassPermissions` — auto-allow tool calls; we
  never accept interactive prompts in a server context.
- `--disallowedTools AskUserQuestion` — refuse Claude's interactive
  question tool. (`handleClaudeUser` also warns and refuses any
  async background tool launch — `claude launched an async tool; not
  allowed in conductor mode`.)

Then *if* managed MCP is configured (`hasManagedMcpConfig` on the
JSON-derived config):

```
--strict-mcp-config
```

…so Claude refuses to silently fall back to user-level MCP settings
that weren't opted into by the conductor run.

After that:

```
[--model <Model>]                              # only when opts.Model != ""
[--effort <ThinkingLevel>]                     # only when opts.ThinkingLevel != ""
[--max-turns <MaxTurns>]                       # only when opts.MaxTurns > 0
[--resume <ResumeSessionID> | --r <id> --fork-session]   # only when resuming
```

…followed by `codexFilterCustomArgs(opts.CustomArgs)`. The resume
flag goes **before** user `CustomArgs` so the user cannot shadow it;
the blocklist below also covers it.

`--approve-for-me` is **not** added — the close-but-not-identical
Claude flag is `bypassPermissions` (added unconditionally above).

## Blocked user flags

See [ADR-0006](../adr/0006-per-backend-blocked-args.md) for the design rationale.

`claudeBlockedArgs` refuses to let `agent.args` override anything
conductor owns. Blocked entries (and the ones whose value is also
dropped):

| Flag | Drops value? |
|---|---|
| `-p` | |
| `--output-format` | ✓ |
| `--input-format` | ✓ |
| `--permission-mode` | ✓ |
| `--strict-mcp-config` | |
| `--model` | ✓ |
| `--max-turns` | ✓ |
| `--mcp-config` | ✓ |
| `--session-name` | ✓ |
| `--resume` / `-r` / `--continue` | ✓ |
| `--fork-session` | |

Anything else from `agent.args` passes through verbatim, including
unknown future flags (Claude's CLI is forward-compatible enough that
this rarely breaks).

## MCP

When `opts.McpConfig` is non-nil, the Claude backend writes it to a
temp file (mode `0o600`) and points the CLI at it via `--mcp-config
<path>`. The temp file is cleaned up on `Session.Result` close.

`hasManagedMcpConfig(opts.McpConfig)` returns true when the schema
produced a non-empty `mcpServers` object. Empty means: *inherit
whatever the user has configured locally* — conductor never writes an
empty config because Claude treats that as "no MCP at all".

## Runtime config injection

`InjectRuntimeConfig(opts.Cwd, "claude", opts.SystemPrompt)` is called
before subprocess start. It writes:

```
# <cwd>/CLAUDE.md
<renderRuntimeConfig("claude", brief)>
```

…with mode `0o600`. This is how the agent's persistent brief reaches
Claude without being duplicated into stdin. `providerNeedsInlineSystemPrompt`
returns `false` for `"claude"`, so the prompt argument passed on stdin
does **not** include the brief.

## Wire protocol

See [`../protocol.md`](../protocol.md#claude-code--stream-json). The
specific dispatch lives in:

| Function | Purpose |
|---|---|
| `handleClaudeAssistant` | Walks `message.content[]` (text / thinking / tool_use) |
| `handleClaudeUser`       | Tool result echo → `MessageToolResult`; refuses async launches |
| `handleClaudeControlRequest` | Writes a synthetic deny on stdin for permission prompts |
| `claudeResultUsage`      | Maps `usage` + `modelUsage` into `Result.Usage` |
| `resolveFallback`        | If the terminal `result` event has empty text, fall back to the last `assistant` text the scanner saw |

## Terminal result

`finalizeStreamResult("claude", timeout, runCtxErr, writeErr, exitErr,
sessionID, streamTerminalState{...})` is the single source of truth
for `Result.Status`. It combines:

- `lastAssistantText` — what the last `assistant` event contained.
- `finalResultText`  — `result.result` from the terminal event.
- `sawResult`        — whether the terminal event was seen.
- `resultIsError`    — `result.is_error` from the terminal event.

into:

| Condition | `Status` | `Output` |
|---|---|---|
| saw terminal result, `is_error=false`, text non-empty | `completed` | the model text |
| saw terminal result, `is_error=false`, text empty | `completed` | `lastAssistantText` (fallback) |
| saw terminal result, `is_error=true` | `failed` | `result.result` if non-empty, else `lastAssistantText`; `Error` = `result.result` |
| infra failure (scanner / write) | `failed` | empty |
| `runCtx.Err() == DeadlineExceeded` and status was completed | `timeout` | preserved |
| `runCtx.Err() == Canceled` and status was completed | `aborted` | preserved |

## Usage reporting

`claudeResultUsage` walks `result.usage` (top-level) and
`result.modelUsage` (per-model) and produces a `map[string]TokenUsage`
keyed by model name. Both shapes report Anthropic prompt-cache fields
(`cache_creation_input_tokens`, `cache_read_input_tokens`) which map
to `TokenUsage.CacheWriteTokens` / `CacheReadTokens`.

## Termination grace

`claudeTerminateGrace = 5 * time.Second` — the gap between SIGTERM and
SIGKILL on the process group during cancellation. Five seconds is
generous enough for the CLI to flush and MCP servers to exit cleanly,
short enough that a stuck handler is recovered quickly.

## Test surface

`claude_test.go` exercises:

- `buildClaudeArgs` against the `claudeBlockedArgs` matrix.
- The wire scanner against fixture event streams.
- Terminal-result classification for each combination of
  `lastAssistantText`, `finalResultText`, `is_error`, and `runCtx.Err()`.

Tests are split into two layers: pure-Go tests
(argv construction, scanner dispatch, MCP rendering) run
unconditionally; `Test*_Live_*` tests run only when the real `claude`
(or `codex`) CLI is on `$PATH` — they `t.Skip` otherwise. **There is
no fake-binary harness today**; the `testhelpers_test.go` file only
provides a logger helper and a temp-file helper. Adding a
`testdata/fake-claude/main.go` + `testdata/fake-codex/main.go` paired
with a `*_integration_test.go` per backend is the recommended next
step to lift `internal/agent` coverage and close the gap between the
argv unit tests and the actual `Execute()` lifecycle.
