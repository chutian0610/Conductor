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
