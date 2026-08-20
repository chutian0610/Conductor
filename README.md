# Conductor

Local multi-agent orchestrator. Wraps Codex (and other agent CLIs) into
a workflow-aware runtime with per-spec HOME isolation, multi-host Hub
dispatch, and a soft-workflow engine (PDCA/GSD/custom phases).

## Status

**Phase 1 in progress** (see `docs/design.md` for v0.13).

## Quick start

```bash
# Build
go build -o ./conductor ./server/cmd/conductor

# Run
./conductor --version
./conductor --help
./conductor daemon           # Phase 1 stub
```

## Provider configuration

`conductor spec create --provider <id>` reads the user's Codex
config (`~/.codex/config.toml`, or `$CODEX_HOME/config.toml` if
set) to resolve a `[model_providers.<id>]` block. The `--provider`
flag maps 1-to-1 to the section suffix.

### Schema

```toml
[model_providers.<id>]
name                 = "Display Name"   # optional, future use
base_url             = "https://..."     # required (Phase 1)
env_key              = "ENV_VAR_NAME"    # optional; env var holding the API key
wire_api             = "responses"       # optional; "responses" (default) or "chat"
requires_openai_auth = false             # optional; default = true
```

Fields Conductor reads:

- `name` — display label (Phase 1 unused; reserved for future
  spec-list pretty-print).
- `base_url` — required, the API endpoint.
- `env_key` — name of the env var that holds the API key
  (`OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `MINIMAX_API_KEY`,
  …). Empty for local providers with no auth.
- `wire_api` — `"responses"` (default) or `"chat"`. Set
  explicitly for custom providers that target a specific protocol.
- `requires_openai_auth` — auth gate. `false` opts out of
  ChatGPT OAuth entirely; codex app-server uses the provider's
  own API key (from `env_key`) directly. **Required for
  non-OpenAI providers** (MiniMax, local proxies, etc.).

Unknown fields are silently ignored — Codex adds fields over time
and we want forward compatibility.

### Common configurations

**OpenAI direct** — built-in fallback, no config needed:

```bash
export OPENAI_API_KEY=sk-...
conductor spec create --provider codex --model gpt-5
# defaults to https://api.openai.com/v1, env_key=OPENAI_API_KEY
```

**OpenRouter / OpenAI-compatible proxy** — declare `base_url` +
`env_key`, then `export` the key and create the spec:

```toml
# ~/.codex/config.toml
[model_providers.openrouter]
base_url = "https://openrouter.ai/api/v1"
env_key  = "OPENROUTER_API_KEY"
```

```bash
export OPENROUTER_API_KEY=sk-or-...
conductor spec create --provider openrouter --model anthropic/claude-opus-4-6
```

**Custom provider without an OpenAI account** (MiniMax, internal
proxy, LiteLLM, etc.) — must opt out of ChatGPT OAuth and declare
the wire protocol:

```toml
# ~/.codex/config.toml
[model_providers.minimax]
name                 = "MiniMax"
base_url             = "http://127.0.0.1:8000/v1"   # or your upstream URL
env_key              = "MINIMAX_API_KEY"
wire_api             = "responses"        #   or "chat", depending on what the upstream speaks
requires_openai_auth = false             #   ← this is the critical line
```

```bash
export MINIMAX_API_KEY=...
conductor spec create --provider minimax --model MiniMax-Text-01
conductor run --spec minimax-<hash> "..."
```

Without `requires_openai_auth = false`, codex app-server would
attempt the ChatGPT OAuth flow and refuse to issue API calls,
because the provider doesn't have an OpenAI account.

**Local provider with no auth** (Ollama, embedded):

```toml
[model_providers.ollama]
base_url = "http://localhost:11434/v1"
# env_key left empty
```

### Where the API key env var is read

Conductor does **not** inject env vars itself. The API key must be
in the environment when `conductor run` spawns the codex app-server
subprocess — typically:

```bash
export MINIMAX_API_KEY=sk-...
conductor run --spec minimax-abc "..."
```

For one-shot use, prefix the command:

```bash
MINIMAX_API_KEY=sk-... conductor run --spec minimax-abc "..."
```

### Troubleshooting

| Symptom | Likely cause |
|---|---|
| `error: provider not found: "openrouter"` (path: ...) | Section header is missing or misspelled. Run `cat ~/.codex/config.toml` and check `[model_providers.openrouter]` is spelled correctly (no trailing spaces, no `[[` double bracket). |
| `error: provider "X" has no base_url` | The `[model_providers.X]` block exists but doesn't declare `base_url =`. Add it. |
| Spec creates OK but codex fails with auth error | Either `env_key` is missing (provider can't find your key), the env var isn't exported when you `conductor run`, or `requires_openai_auth` is true on a non-OpenAI provider. |
| Spec creates OK but codex fails with protocol error | Wrong `wire_api` for the upstream. If the upstream only speaks Chat Completions, set `wire_api = "chat"` (note: Phase 1 doesn't include a Responses→Chat Completions adapter yet, so this path is partially supported). |

## Architecture

5-layer design (see `docs/design.md` for full detail):

```
Agent Gateway  ←  HTTP/WS + CLI  (this binary)
   ↓
Agent Workflow  ←  Soft workflow engine (PDCA / GSD / custom)
   ↓
Agent Worker    ←  DAG nodes: single / seq / parallel / switch / loop
   ↓
Agent Runner    ←  Lifecycle / event stream / persistence / context
   ↓
Agent Provider  ←  Codex app-server JSON-RPC (Phase 1)
```

Plus `Player Daemon + Registry` and per-spec HOME isolation under
`$CONDUCTOR_HOME/specs/<specId>/home/`.

## Repository layout

```
conductor/
├── server/                 # Go single binary
│   ├── cmd/conductor/      # Subcommand dispatch (main.go + stubs)
│   └── internal/
│       ├── protocol/        # Wire types (Phase 1 TODO)
│       ├── provider/codex/  # Codex app-server client (Phase 1 TODO)
│       ├── spec/            # Spec CRUD + per-spec HOME (Phase 1 TODO)
│       ├── runner/          # Lifecycle (Phase 1 TODO)
│       ├── home/            # Per-spec HOME isolation (Phase 1 TODO)
│       ├── storage/          # JSON file + SQLite (Phase 1 TODO)
│       ├── worker/           # DAG nodes (Phase 2)
│       ├── workflow/         # Soft workflow engine (Phase 2)
│       ├── registry/         # PlayerRegistry + Hub (Phase 2)
│       ├── gateway/          # HTTP + WS (Phase 2)
│       ├── cancellation/     # context.Context helpers (Phase 1 TODO)
│       └── cli/              # CLI helpers (Phase 1 partial)
├── webui/                  # JS workspace (Phase 3)
├── examples/               # Workflow samples (Phase 2)
├── docs/                   # Design documents
└── README.md
```

## Design

See [`docs/design.md`](docs/design.md) for the full v0.13 design — 22 commits
of iteration over multi-agent orchestration patterns, provider abstraction,
isolation strategies, and cancellation protocols.
