# Backend architecture

> **Source of truth:** `server/internal/agent/agent.go`,
> `server/internal/agent/stream.go`,
> `server/internal/agent/runtime_config.go`,
> `server/internal/agent/stderr_tail.go`,
> `server/internal/agent/resume_fallback.go`,
> `server/internal/agent/proc_other.go`,
> `server/internal/agent/proc_windows.go`,
> `server/cmd/conductor/main.go`.

## The `Backend` seam

Everything hangs off `agent.Backend` (`internal/agent/agent.go`). The
contract is deliberately small so a third LLM CLI plugs in with one new
file plus a case in `New()`.

```go
// internal/agent/agent.go
type Backend interface {
    Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error)
}

type Session struct {
    Messages <-chan Message   // live events, closed when the agent finishes
    Result   <-chan Result     // exactly one terminal outcome
}

type MessageType string
const (
    MessageText       MessageType = "text"
    MessageThinking   MessageType = "thinking"
    MessageToolUse    MessageType = "tool-use"
    MessageToolResult MessageType = "tool-result"
    MessageStatus     MessageType = "status"   // lifecycle events
    MessageError      MessageType = "error"    // model/CLI error frames
    MessageLog        MessageType = "log"      // structured log line
)

type Message struct {
    Type      MessageType
    Content   string
    Tool      string
    CallID    string
    Input     map[string]any
    Output    string
    Status    string
    Level     string
    SessionID string         // populated early so callers can pin a session id
}

type TokenUsage struct {
    InputTokens     int64
    OutputTokens    int64
    CacheReadTokens int64
    CacheWriteTokens int64
}

type Result struct {
    Status     string                 // "completed" | "failed" | "timeout" | "cancelled" | "aborted"
    Output     string                 // final user-facing output
    Error      string                 // populated when Status != "completed"
    DurationMs int64
    SessionID  string                 // backend-native, stable across resumes
    Usage      map[string]TokenUsage  // keyed by model name
}
```

`Execute` is **non-blocking** — it returns a `*Session` immediately. The
caller drains `Session.Messages` for live events and waits on
`Session.Result` for the terminal outcome. The `Messages` channel is
closed when the agent finishes; `Result` carries exactly one value before
closing.

### `ExecOptions`

Zero values are meaningful:

| Field | Zero behaviour |
|---|---|
| `Cwd` | "" runs in the caller's cwd |
| `Model` | "" uses the backend's default |
| `MaxTurns` | `<= 0` means unbounded |
| `Timeout` | `0` disables the wall-clock bound (inactivity watchdog still fires) |
| `SemanticInactivityTimeout` | `0` disables it |
| `ThinkingLevel` | "" uses the runtime default |
| `SystemPrompt` | routed by `providerNeedsInlineSystemPrompt` — either prepended to the CLI's prompt or written to the workdir context file |
| `ResumeSessionID` | "" starts a fresh session |
| `McpConfig` | `nil` means "inherit whatever the user has configured locally" |
| `CustomArgs` | appended after the blocklist filter |
| `Env` | merged into the spawned process environment, overriding inherited values |

Provider-specific knobs (resume session ids, service tiers, plugin
settings) belong in `ExecOptions` fields the relevant backend reads;
absent fields stay absent rather than being pre-declared.

### `Config`

```go
type Config struct {
    ExecutablePath string                              // "" means resolve via $PATH
    Env            map[string]string                   // extra env for the subprocess
    Logger         *slog.Logger                        // nil falls back to slog.Default()
}
```

The CLI (`cmd/conductor/main.go`) currently sets only `Logger`; everything
else comes from `conductor.yaml` and `os.Getenv`.

### `SupportedTypes`

```go
var SupportedTypes = []string{"claude", "codex"}

func IsSupportedType(t string) bool { /* linear scan */ }

func New(agentType string, cfg Config) (Backend, error) {
    if !IsSupportedType(agentType) { /* ... */ }
    switch agentType {
    case "claude": return &claudeBackend{cfg: cfg}, nil
    case "codex":  return &codexBackend{cfg: cfg}, nil
    }
}
```

Adding a new type means implementing `Backend` and editing this
`switch` — nothing else.

## Lifecycle

```
┌──────────────────────────┐
│ main(): cobra parses CLI │
└─────────────┬────────────┘
              │ doRun(...)
              ▼
┌────────────────────────────────────────────────────────────────┐
│ configschema.Load(path) → schema.Validate → ToExecOptions     │
│ Brief = RuntimeBrief() = prompt: + skill files                  │
└─────────────┬──────────────────────────────────────────────────┘
              │ schema.Agent.Backend → agent.New("...", cfg)
              ▼
┌────────────────────────────────────────────────────────────────┐
│ signal.NotifyContext(SIGINT, SIGTERM)                          │
│ backend.Execute(ctx, prompt, opts) → *Session                   │
└─────────────┬──────────────────────────────────────────────────┘
              │
              ▼
┌────────────────────────────────────────────────────────────────┐
│ Per-backend preflight:                                          │
│   claude: InjectRuntimeConfig(cwd, "claude", brief) → CLAUDE.md │
│   codex:  InjectRuntimeConfig(cwd, "codex",  brief) → AGENTS.md  │
│           ensureCodexMcpConfig($CODEX_HOME/config.toml, cfg)    │
└─────────────┬──────────────────────────────────────────────────┘
              │ spawn subprocess (Setpgid + new session)
              ▼
┌────────────────────────────────────────────────────────────────┐
│ Pump stdout line-delimited JSON through wire scanners.         │
│   claude.go:  handleClaudeAssistant / User / Result              │
│   codex.go:   handleCodexItem → Message{Text,Thinking,ToolUse,  │
│                 ToolResult}, capture lastAgentText/thread_id    │
│ Emit Message* events on Messages.                               │
│ stderr → stderrTail (rolling 4 KiB buffer; surfaced on failure). │
└─────────────┬──────────────────────────────────────────────────┘
              │ proc.Wait() returns; procDone closed
              ▼
┌────────────────────────────────────────────────────────────────┐
│ finalizeStreamResult(provider, timeout, runCtxErr, writeErr,    │
│                       exitErr, sessionID, state)                │
│   1. infra failure? → "failed"                                  │
│   2. timeout?       → "timeout"                                 │
│   3. cancellation?  → "aborted"                                 │
│   4. else            → completed/failed by is_error + last text │
└─────────────┬──────────────────────────────────────────────────┘
              │
              ▼
┌────────────────────────────────────────────────────────────────┐
│ Drain Messages. Emit one Message on Session.Result. Close both. │
└────────────────────────────────────────────────────────────────┘
```

### Termination precedence

`finalizeStreamResult` (`internal/agent/stream.go`) classifies the run in
this order — earlier checks win:

1. **Infrastructure failure** — scanner / stdout close / write error →
   `failed`.
2. **`status == "completed"` AND `runCtx.Err() == DeadlineExceeded`** →
   `timeout` (the run finished talking to the model but exceeded the wall
   clock; we surface "timed out" so callers can tell apart from a model
   crash).
3. **`status == "completed"` AND `runCtx.Err() == Canceled`** →
   `aborted` (SIGINT/SIGTERM).
4. **Else** — the CLI's own decision: `is_error: true` → `failed`,
   `is_error: false` → `completed`, regardless of `exitErr`.

Cancellation is non-destructive: the backend keeps draining `Messages`
until both channels close, so the UI can still render any final output
that was already buffered before SIGTERM landed.

### Timeouts

Two clocks run per execution:

- `opts.Timeout` — wall-clock bound for the whole run. Exceeding it
  cancels `runCtx` (`runContext`) and SIGTERMs the spawned CLI.
- `opts.SemanticInactivityTimeout` — fires when *no* event arrives on
  stdout for the configured duration. Catches "model is silently
  thinking and never returns" without giving up on long reasoning
  passes. Zero disables.

Both default to zero; the inactivity watchdog fires only when explicitly
configured.

## The prompt-vs-brief split

The user-facing prompt seen by the CLI is built in
`configschema.Schema.TaskPrompt`:

```
brief + "\n\n---\n\n" + extraPrompt
```

where `brief = RuntimeBrief()` is `agent.prompt` concatenated with each
skill file's contents (each prefixed by `--- skill: <basename> ---`).
`extraPrompt` is whatever the caller passed via `--prompt` on the CLI.

Both halves then travel two paths:

- `brief` is also stored as `ExecOptions.SystemPrompt` and routed by
  `providerNeedsInlineSystemPrompt(provider)`:
  - `claude` → written to `<cwd>/CLAUDE.md` (claude reads this natively)
  - `codex`  → written to `<cwd>/AGENTS.md` (codex reads this natively)
  - any future provider lacking disk-based delivery → prepended to
    the spawned CLI's prompt

- `extraPrompt` is appended to the prompt passed to the spawned CLI for
  both backends. For Claude this goes on stdin (`writeClaudeInput`); for
  Codex it's a positional argv argument.

V1 duplicates the brief in both places (file + prompt). This is
intentional — it preserves the existing "everything via stdin"
semantics for backends that, today, have no per-workdir context hook.
V2 will split cleanly so `prompt` carries only the per-turn task and
`SystemPrompt` carries the persistent brief.

## Resume semantics

`ExecOptions.ResumeSessionID` continues a previous session instead of
starting fresh. Backend-specific translation:

| Backend | Session ID source | Resumption mechanism |
|---|---|---|
| Claude | `session_id` from the `system` event | `--resume <id>` *must appear before* any user `CustomArgs` so `claudeBlockedArgs` cannot drop it; `--r` is an alias and is also blocked. |
| Codex  | `thread_id` from `thread.started` notification | `codex exec resume --json <id> "prompt"`. The resume subcommand **rejects** `-C` and `--approve-for-me`; per `codex-cli 0.147.0`, only `-m` and `-c` overrides are accepted. |

`ResumeExpected` toggles fallback. When `true`, a *permanent* resume
failure (session gone, schema drift, image too large) triggers
`resumeWithContinuityNotice` (`resume_fallback.go`) which:

1. Re-spawns the backend in fresh-exec mode.
2. Prepends `ResumeContinuityNotice` to the prompt (the operator-supplied
   "treat this as a fresh conversation; do not assume any prior context"
   banner).
3. Surfaces the original failure in a `MessageError` event before the
   fresh session starts.

When `false`, resume failures bubble up as the run's terminal error.

## Stderr handling

`stderr_tail.go` always runs alongside the subprocess: it pipes stderr
into a rolling ~4 KiB buffer (`stderrTail`) and, on a non-completed
exit, surfaces the last lines so the user can see why the CLI died.

The tail is **diagnostic only** — model output travels on stdout as
line-delimited JSON (see `protocol.md`).

## Per-backend detail

See:

- [`backends/claude.md`](backends/claude.md)
- [`backends/codex.md`](backends/codex.md)

## Design notes (ADR pointers)

- [ADR-0001](adr/0001-v1-cli-only-no-http.md) — V1 ships a CLI only.
- [ADR-0002](adr/0002-codex-exec-json-not-app-server.md) — `codex exec --json`; no app-server websocket.
- [ADR-0003](adr/0003-refuse-windows-run-time.md) — process-group machinery is Unix-only.
- [ADR-0004](adr/0004-strict-yaml-schema.md) — `KnownFields(true)`.
- [ADR-0005](adr/0005-brief-duplicated-disk-and-prompt-v1.md) — brief travels disk + prompt in V1; V2 splits.
- [ADR-0006](adr/0006-per-backend-blocked-args.md) — `claudeBlockedArgs` / `codexBlockedArgs`.
