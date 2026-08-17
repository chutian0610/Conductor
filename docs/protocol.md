# Streaming JSON wire protocol

Both V1 backends communicate with their CLI over **line-delimited JSON
on stdout**. The contract is small enough that the two implementations
mirror each other almost line-for-line; the table below shows the
structural alignment.

## Claude Code — `stream-json`

Claude is invoked with `--output-format stream-json --input-format
stream-json --verbose`. Each line on stdout is one of:

| Event | Type discriminator | Translated to |
|---|---|---|
| `{"type":"system",...}`                       | `system` (legacy) / present in stream-json as init | `MessageStatus` ("running") with `SessionID` pinned |
| `{"type":"assistant","message":{...}}`        | assistant turn | iterate `message.content[]`, mapping each block: `text`→`MessageText`, `thinking`→`MessageThinking`, `tool_use`→`MessageToolUse` (with `Input`) |
| `{"type":"user","message":{...}}`             | tool result echoed by claude | `MessageToolResult` |
| `{"type":"result","result":"...","is_error":...,"session_id":"..."}` | terminal | classifies `Result.Status`; populates `Output`, `Error`, `SessionID`, `Usage` |
| `{"type":"control_request",...}`              | permission prompt | `handleClaudeControlRequest` writes a synthetic deny on stdin (we never accept interactive permissions) |
| `{"type":"log",...}`                          | CLI-side log line | `MessageLog` |

`claudeUsage` and `claudeResultModelUsage` populate `Result.Usage`
keyed by model name. Cache tokens use Anthropic's prompt-cache shape
(input / cache_creation / cache_read / output).

## Codex CLI — `exec --json`

Codex is invoked with `codex exec --json [--approve-for-me] [-C <cwd>]
[-m <model>] [-c model_reasoning_effort=<level>] ... -- "<prompt>"`.
Each line on stdout is one of:

| Event | Type discriminator | Translated to |
|---|---|---|
| `{"type":"thread.started","thread_id":"..."}` | session banner  | `MessageStatus` ("running") with `SessionID` (= `thread_id`) |
| `{"type":"turn.started"}`                     | turn begins     | `MessageStatus` |
| `{"type":"item.started","item":{...}}`        | item starts     | `MessageToolUse` for `command_execution` (capture command on started; echo is not repeated on completed); `lastAgentText`/`lastReasoning` capture for downstream items |
| `{"type":"item.completed","item":{...}}`      | item completes  | `MessageToolResult` for command_execution (aggregated_output + exit_code); `MessageText` for `agent_message`; `MessageThinking` for `reasoning` (V1.2) |
| `{"type":"turn.completed","usage":{...}}`     | terminal        | closes the turn; usage is folded into `Result.Usage` |
| `{"type":"error","message":"..."}`            | CLI error       | `MessageError` and an immediate terminal `failed` |

`codexItem` covers `agent_message`, `reasoning`, and
`command_execution` subtypes. The `command` field is captured on
`item.started` because Codex does not repeat it on `item.completed`
— we have to remember it for the matching `MessageToolResult`.

`codexUsage` keys: `input_tokens`, `cached_input_tokens`,
`cache_write_input_tokens`, `output_tokens`, `reasoning_output_tokens`.

## Shared scanner

Both backends consume stdout through `stream.go` which exposes:

- `newAgentStreamScanner(stdout io.Reader)` — a `*bufio.Scanner` with
  an enlarged buffer (1 MiB max line) so JSON events with embedded
  tool output do not split lines.
- `finalizeStreamResult(provider, timeout, runCtxErr, writeErr, exitErr,
  sessionID, state)` — see [backend.md](backend.md#termination-precedence)
  for the precedence rules.

The scanner does not own message dispatch; that lives per backend in
`handleClaude...` / `handleCodex...` so each side stays free to evolve
its event taxonomy without re-touching the other.

## What is *not* on the wire

- The model name (typically inferred from a `system` event on Claude
  and absent on Codex; surfaces in `Result.Usage` keys instead).
- Tool `id` (we map to `CallID` but only Claude uses it for
  correlation today; Codex `item.id` is per-item).
- Token usage for backends that don't report it (zero values in
  `TokenUsage`).

## Why line-delimited JSON, not app-server

Codex also ships an "app server" / remote-control websocket mode. We
don't use it: it requires ChatGPT OAuth for the websocket, whereas
`exec --json` works with a plain `OPENAI_API_KEY` and the wire shape
is structurally identical to Claude's `stream-json` — which is why
`codex.go` mirrors `claude.go`.
