# Conductor Server

V1 of the **Conductor AI worker** — a Go service that drives an LLM CLI
(Claude Code today; Codex CLI in V1.1) as a subprocess, parses the
streaming JSON protocol, and exposes a uniform event stream + terminal
result.

This repo deliberately contains **no HTTP server and no UI** for V1. The
only consumer is the `conductor` CLI, which is enough to validate the
backend+agent abstraction end-to-end. The HTTP layer and DAG scheduler
land in V2.

## Platform support

| OS      | Status   |
|---------|----------|
| macOS   | Supported (arm64 + amd64) |
| Linux   | Supported (amd64 + arm64) |
| Windows | **Not supported.** The binary compiles but refuses to spawn agents at runtime with a clear error. Windows support would require Job Objects and is out of scope — see `internal/backend/proc_windows.go` for the rationale. |

The subprocess machinery relies on Unix process groups (`Setpgid` +
`kill(-PGID, sig)`) for graceful whole-tree cancellation; that has no
portable Windows equivalent.

## Layout

```
server/
├── cmd/conductor/        — CLI entry (cobra)
├── internal/backend/       — Backend interface + claude + codex backends
├── internal/configschema/— conductor.yaml schema + loader
├── examples/             — example agent.yaml + skills
├── go.mod
└── README.md
```

The Backend interface in `internal/backend/agent.go` is the seam everything
else hangs off:

```go
type Backend interface {
    Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error)
}

type Session struct {
    Messages <-chan Message   // live events, closed when finished
    Result   <-chan Result     // exactly one terminal outcome
}
```

Adopting a new LLM CLI means implementing `Backend` (one file in
`internal/backend/`) and adding the type name to `SupportedTypes` + the
`switch` in `New()`.

## Quick start

```bash
# 1. Build
go build -o bin/conductor ./cmd/conductor

# 2. Edit an agent
$EDITOR examples/code-review-agent.yaml

# 3. Run it
./bin/conductor run --config examples/code-review-agent.yaml \
                    --prompt "Review the staged diff"
```

The CLI streams human-readable events to stderr and writes the agent's
final answer to stdout. Use `--json` to get one JSON object per event for
machine consumption; use `--quiet` to silence the per-event rendering and
emit only the final result.

## conductor.yaml (v0)

See `examples/code-review-agent.yaml` and the schema docs in
`internal/configschema/schema.go`. Top-level shape:

```yaml
version: 1
agent:
  name: ...
  description: ...
  backend: claude              # claude or codex (exec --json)
  model: claude-sonnet-4-5
  thinking: medium             # low|medium|high|xhigh|max
  cwd: .                       # relative paths resolve against the YAML's dir
  max_turns: 30                # 0 = unlimited
  timeout: 15m                 # "30m" / "1h" / "0s" / ""
  prompt: |                    # system brief
    You are a ...
  skills:                      # plain-text briefs appended after `prompt:`
    - ./skills/style.md
  args: []                     # extra CLI flags (blocklist filtered)
  env: {}                      # extra env vars for the spawned CLI
  mcp:
    servers:                   # passed via --mcp-config
      - name: filesystem
        command: npx
        args: ["-y", "@modelcontextprotocol/server-filesystem", "."]
```

## V1 limitations

- **Two backends, both production-usable** — `claude` (Claude Code) and
  `codex` (Codex CLI in `exec --json` mode). Codex's app-server mode is
  not used because it requires ChatGPT OAuth for the remote-control
  websocket; `exec --json` works with a plain API key and the protocol
  is the same shape as Claude's `stream-json`. See `internal/backend/`
  for the wire details.
- **No DAG / plan / self-audit / memory** — those are V2. V1 only
  validates the subprocess boundary + event stream + config schema.
- **No HTTP server** — `conductor run` is the only entry point. Add one
  when the V2 DAG scheduler needs an external trigger.

## Verification

```bash
go vet ./...
go test ./...
go build -o bin/conductor ./cmd/conductor
```

Cross-platform: the build matrix is `darwin/amd64`, `darwin/arm64`,
`linux/amd64`, `linux/arm64`. `windows/*` is excluded.
