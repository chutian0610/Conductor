# 2. Codex backend uses `codex exec --json`, not the app-server websocket

Date: 2026-08-17
Status: Accepted

## Context

Codex CLI ships in two modes relevant to Conductor:

1. **`codex exec --json`** — one-shot non-interactive mode that prints
   line-delimited JSON events on stdout and reads the prompt as a
   positional argv argument. Plain OpenAI API key auth.
2. **`codex app-server`** — long-lived websocket that lets a remote
   controller drive the agent. Requires ChatGPT OAuth for the
   remote-control websocket.

The wire shape of mode (1) is structurally identical to Claude's
`stream-json`: both are line-delimited JSON on stdout, both surface
tool use / tool result / assistant turns / terminal result events.
That commonality is what makes Conductor's single uniform
`agent.Message` taxonomy possible.

## Decision

`codexBackend` (in `server/internal/agent/codex.go`) shells out to
`codex exec --json`. The websocket mode is **not** used and there is
no abstraction layer that could be repurposed for it later; it would
require a fundamentally different transport.

## Consequences

Positive:
- Works with a plain `OPENAI_API_KEY`; no ChatGPT OAuth negotiation
  inside Conductor.
- Shares the same `Session` / `Message` shape as Claude, which is
  what justifies the unified `Backend` interface. (`claude.go` and
  `codex.go` mirror each other line-for-line for that reason.)
- Resume behaves predictably: `codex exec resume --json <id>` plus
  per-turn `-m` and `-c` overrides — see [ADR-0006](0006-per-backend-blocked-args.md).

Negative:
- No streaming subcommand continuation mid-execution; runs are
  one-shot. (Claude has the same property; this isn't a step backwards.)
- If OpenAI deprecates `codex exec` in favor of the app-server,
  Conductor's codex backend needs a rewrite. That risk is judged low
  for V1; V2 should re-evaluate if the websocket becomes the only
  viable long-lived mode.

## See also

[`docs/backends/codex.md`](../backends/codex.md) for the argv /
blocked-args wire details, and
[`docs/protocol.md`](../protocol.md#codex-cli--exec-json) for the
event taxonomy.
