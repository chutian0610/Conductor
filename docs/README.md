# Conductor Backend Docs

This directory documents **V1 of the `conductor` backend** — the Go
service that drives an LLM CLI (Claude Code today, Codex CLI in V1.1)
as a subprocess and exposes a uniform event stream + terminal result.

> **Status:** V1 only validates the subprocess boundary + event stream +
> config schema. No HTTP server, no DAG scheduler, no self-audit, no
> memory. Those land in V2. See `server/README.md` for the user-facing
> quick-start (build, run, conductor.yaml shape, verifications).

## Layout

```
docs/
├── README.md           — this file
├── backend.md          — Backend interface, lifecycle, Message/Result
├── protocol.md         — line-delimited JSON wire protocols (Claude +
│                          Codex)
├── configuration.md    — conductor.yaml schema reference
├── process-model.md    — subprocess, process groups, cancellation,
│                          Windows refuse-to-run
└── backends/
    ├── claude.md       — Claude Code backend details (argv, blocked
    │                      args, MCP, result fields)
    └── codex.md        — Codex CLI backend details (argv, blocked
                          args, resume subcommand, MCP persistence,
                          fallback on permanent session loss)
```

## Reading order

1. **[backend.md](backend.md)** — start here. Defines the seam
   everything else hangs off.
2. **[protocol.md](protocol.md)** — what a backend actually receives
   from the CLI on stdout. Both backends are line-delimited JSON.
3. **[backends/](backends/)** — per-backend details: argv construction,
   blocked flags, MCP materialisation, terminal result mapping.
4. **[configuration.md](configuration.md)** — full `conductor.yaml`
   reference with validation rules.
5. **[process-model.md](process-model.md)** — how the subprocess is
   spawned, how SIGINT becomes a graceful SIGTERM→SIGKILL on the whole
   process tree, and why Windows refuses to run.

## Adding a third backend

To plug in a new LLM CLI (e.g. a hypothetical `grok` or `kimi`):

1. Implement `agent.Backend` (`internal/agent/agent.go`) — one file in
   `internal/agent/`, exported as a private struct + a `case` in `New()`.
2. Add the type name to `SupportedTypes`.
3. If the CLI reads no per-workdir context file, set
   `providerNeedsInlineSystemPrompt("<type>")` to `true` so
   `SystemPrompt` is prepended to the prompt; otherwise write the brief
   to the appropriate per-workdir file from
   `InjectRuntimeConfig(workDir, "<type>", brief)` and add a
   `runtimeConfigPath` case for the new provider.
4. Document the wire protocol in `protocol.md`, the argv / blocked-flags
   contract in `backends/<name>.md`.
5. Tests: emulate the CLI in a small Go program (mirroring how the
   `claude_test.go` / `codex_test.go` suites use a fake binary) and
   exercise argv construction + scanner dispatch + terminal-result
   classification.

That is the entire contract.
