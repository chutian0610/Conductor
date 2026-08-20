// Package codex implements the AgentClient / AgentSession
// interfaces (server/internal/runner) by talking to OpenAI's
// `codex app-server` over JSON-RPC 2.0 over stdio.
//
// The Codex CLI ships an "app-server" subcommand that exposes a
// long-running JSON-RPC interface on its stdin/stdout. Conductor
// spawns it as a subprocess, points HOME at the per-spec HOME
// (server/internal/home.IsolatedHome), and pipes:
//
//   - writes: JSON-RPC requests ("method" + "params" + "id")
//   - reads:  JSON-RPC responses (matching "id") and notifications
//             (no "id", pushed by the server)
//
// We deliberately don't implement every Codex RPC method. Phase 1
// needs:
//
//   - model/list         — enumerate available models for a provider
//   - thread/start       — start a new conversation thread
//   - thread/resume      — resume an existing thread by id
//   - turn/start         — send a prompt for a thread
//   - turn/interrupt     — §14 cancellation entry point
//
// Notifications we care about:
//
//   - item/agentMessage/delta    streaming text
//   - item/toolCall              tool invocation
//   - item/toolResult            tool result
//   - turn/completed             end of turn (with usage)
//
// Anything we don't recognise is logged and skipped — the AgentStreamEvent
// model only carries the kinds we need (see protocol package).
package codex
