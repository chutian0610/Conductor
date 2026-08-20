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
// The package has two layers:
//
//   - Client (client.go) is the low-level JSON-RPC 2.0 transport:
//     it owns the subprocess, the stdin/stdout pipes, the id →
//     response correlation, and a channel of raw notification
//     envelopes. Tests and ad-hoc tooling can use it directly.
//
//   - Session (session.go) is the high-level wrapper Conductor's
//     runner consumes: it issues the thread/start (or thread/resume)
//     handshake, calls turn/start on Send, maps notifications into
//     protocol.AgentStreamEvent, and routes turn/completed back to
//     the active Send call as an *protocol.AgentTurnResult. Cancel
//     issues turn/interrupt before closing.
//
// Phase 1 RPC methods we issue:
//
//   - thread/start      — start a new conversation thread
//   - thread/resume     — resume a previously-stored thread (when
//                         SessionConfig.SessionId is non-empty)
//   - turn/start        — submit a prompt for a thread
//   - turn/interrupt    — §14 cancellation entry point (graceful)
//
// Notifications we map:
//
//   - item/agentMessage/delta            → protocol.EventText
//   - item/toolCall                      → protocol.EventToolCall
//   - item/toolResult                    → protocol.EventToolResult
//   - item/commandExecution/requestApproval
//     item/fileChange/requestApproval    → protocol.EventPermission
//   - turn/completed                     → Send() return value
//                                         (NOT pushed to Events())
//
// Anything else is logged and skipped in Phase 2; for now dropped
// silently. See protocol.AgentStreamEvent for the union shape.
package codex
