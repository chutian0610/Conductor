# 10. V2 HTTP transport — daemon mode + REST/SSE surface

Date: 2026-08-18
Status: Accepted (V2; supersedes ADR-0001's deferral of HTTP)

## Context

ADR-0001 deferred "V2 HTTP: `internal/http/`, streaming event
endpoint, lifecycle hooks" on the grounds that the V1 surface
needed a CLI only — `conductor run` was the lone command, and the
binary's only job was to drive an LLM CLI as a subprocess.

V1 + V1.x have landed:

  - The agent registry (ADR-0008) gives every run a durable record.
  - The adversarial audit loop (ADR-0009) gives that record a verdict.
  - The brief routing split (ADR-0005 V2 plan) cut duplicate brief
    delivery.
  - `--no-record` / auto-register / identity env vars round out the
    CLI surface.

What's missing for V2: external systems — IDE plugins, webhooks,
CI coordinators, custom dashboards — can only interact with
conductor via shell-out to the binary. The DAG scheduler (followup
#13) has nowhere to live. Long-lived sessions can't be observed
mid-flight from a browser.

This ADR lifts ADR-0001's gate and captures the V2 HTTP design
**before** code is written. Endpoints, streaming format,
auth, concurrency, and failure semantics are pinned now so the
implementation has boundaries to anchor against.

## Decision

### 1. Same binary, two modes

The `conductor` binary gains a top-level `serve` subcommand. The
existing `run` / `agent` / `audit` subcommands continue to work
exactly as they do today (V1 surface preserved). Two modes:

  - CLI mode (`conductor run`, `conductor agent …`, `conductor
    audit …`) — one-shot, stdin/stdout, exits when done. This is
    the V1 contract.
  - Daemon mode (`conductor serve`) — long-lived process listening
    on a bind address, surfaces the same operations over HTTP. CLI
    subcommands keep working alongside it; they're aliases for the
    most common cases.

No separate `conductord` binary. Operators who want a daemon run
`conductor serve`; operators who want a one-shot run the same
binary the way V1 did. The codebase stays a single Go module.

### 2. Bind address — localhost only

Default bind address: `127.0.0.1:7411`. Override via `--bind
<addr>` flag on `conductor serve`. Unix socket supported via
`--bind unix:///var/run/conductor.sock`. Listen on `0.0.0.0`
**requires** `--allow-public-bind` and emits a one-line
audit-stderr warning; otherwise the daemon exits with non-zero
before binding.

**Exposure beyond localhost is the operator's problem, not ours.**
The code path that binds `0.0.0.0` plus carries no auth tokens is
the failure mode we explicitly want operators to opt into. Default
is safe; dangerous defaults are what get exploited.

### 3. Auth — single shared token for V2

When `conductor serve` starts:

  - If `CONDUCTOR_TOKEN` env var is set, that string is the bearer
    token. The daemon accepts `Authorization: Bearer <token>`
    on every request and rejects everything else with 401.
  - If `CONDUCTOR_TOKEN` is unset, the daemon generates a random
    32-byte token, writes it to `--token-out <path>` (default
    `~/.config/conductor/serve.token`), and prints it once at startup.

The CLI subcommands in CLI mode do not need the token (they go
through the binary directly). The CLI does need the token when
running against the daemon (a future ADR or followup row covers
`conductor run --remote-url <addr>` if demand emerges).

**Auth model assumptions baked in:**

  - One token, full permissions. No RBAC, no per-agent scopes.
  - Token is owner-only by virtue of file mode on the token file
    (0600) and Unix socket permissions (default 0600).
  - The token grants the bearer to do anything the daemon can do:
    start runs, read runs, register agents, force-audit. There is
    no way to lock down to a subset in V2.

### 4. Endpoints — REST/JSON under `/v1/`

| Path | Method | Body | Response | Notes |
|---|---|---|---|---|
| `/v1/healthz` | GET | — | `{"ok":true}` | Liveness; no auth. |
| `/v1/version` | GET | — | `{"version":"..."}` | No auth. |
| `/v1/runs` | GET | — | `[Run]` | `?agent_id=&status=&limit=&after=` filters. |
| `/v1/runs` | POST | `{agent_id, prompt?, resume?, opts}` | `Run` (202 Created) | Starts a run; returns the row immediately. |
| `/v1/runs/{id}` | GET | — | `Run` | Detail row. |
| `/v1/runs/{id}/events` | GET | — | `[Event]` | Replay recorded events (read-only). |
| `/v1/runs/{id}/stream` | GET | — | SSE | Live event stream (see §5). |
| `/v1/runs/{id}/result` | GET | — | `Result` | The terminal Result.Wait short-circuits if already done. |
| `/v1/runs/{id}/audit` | GET | — | `RunAudit?` | Latest audit verdict (null if never audited). |
| `/v1/runs/{id}/audit:run` | POST | `{force?, model?}` | `RunAudit` (sync) | Trigger a fresh audit; sync because they're cheap-ish. |
| `/v1/agents` | GET | — | `[Agent]` | |
| `/v1/agents` | POST | `{name, backend, description?, parent?, config_yaml}` | `Agent` | YAML body is parsed and validated at registration time. |
| `/v1/agents/{id}` | GET | — | `Agent` | |
| `/v1/agents/{id}` | PATCH | partial Agent | `Agent` | Update fields. |
| `/v1/agents/{id}` | DELETE | — | `204 No Content` | Soft archive. |
| `/v1/agents/{id}/runs` | GET | — | `[Run]` | Runs for one agent. |
| `/v1/audits/pending` | GET | — | `[run_id]` | List runs lacking a completed audit. |

JSON shapes reuse the existing `Run` / `Agent` / `Event` / `RunAudit`
structs from `internal/agentregistry` and `internal/audit` via Go's
JSON tags. **No new wire types** — the same `Message`, `Result`,
etc. that the CLI emits now become HTTP responses. CLI and HTTP
share the contract by sharing the type.

Path versioning: the `/v1/` prefix is fixed for V2.0. Breaking
changes (schema fields reordered, paths renamed) require a new
major version `/v2/`. Additive changes (new optional fields, new
endpoints) stay under `/v1/`.

### 5. Streaming — SSE over `GET /v1/runs/{id}/stream`

The live event channel is `Server-Sent Events` (HTTP/1.1, text/event-stream).
SSE is preferred over WebSocket because:

  - the wire is unidirectional (server → client); duplex isn't needed.
  - it works through plain HTTP/1.1 proxies (with `X-Accel-Buffering: no`
    for nginx).
  - it's implemented by `net/http` without a separate library.

**Per-event SSE shape** (one event per `backend.Message`):

```
event: started
id: 1
data: {"run_id":42,"started_at":1700000000000}

event: stdout
id: 2
data: {"Type":"text","Content":"hi","Tool":"","CallID":"","Input":null,"Output":"","Status":"","Level":"","SessionID":""}

event: result
id: 99
data: {"Status":"completed","DurationMs":1234,"SessionID":"...","Output":"final answer","Error":"","Usage":{"claude-sonnet-4-5":{...}}}
```

The `id:` is the events.id row id (monotonic per run); clients
reconnecting with `Last-Event-Id` resume from that point.

Heartbeats: every 15s the daemon emits an empty `event: ping` so
proxies with read timeouts keep the connection alive.

**Terminal events:**
  - `event: result` — the run completed; payload mirrors `Result`.
  - `event: error` — start failed (invalid config, agent archived, …);
    the connection closes with 200 + this event then closes cleanly.
  - `event: cancelled` — operator called `DELETE /v1/runs/{id}`.

Each run's stream is a single SSE connection; the daemon handles
many concurrent streams (one per active run × active subscriber).

### 6. Persistence — single daemon owns the registry

Conductor is local-first: one operator, one machine, one daemon.
A second `conductor serve` against the same registry is a usage
bug, not a feature. The product assumption is that multiple
daemon instances do not arise; we do not paper over them.

Concretely:

  - On startup the daemon takes an exclusive, non-blocking
    `flock(2)` on `conductor.lock` in the registry directory
    (user-global: `~/.conductor/conductor.lock`; project-local:
    `<cwd>/.conductor/conductor.lock`). The lock file is created
    mode 0600 if absent. The parent directory's mode is whatever
    `mkdir -p` left it (typically 0755), which is fine because
    `flock(2)` is owner-only and the file itself is 0600.
  - If the lock is already held, the daemon logs to stderr the
    existing holder's PID / host / uptime (read from a sidecar
    `conductor.lock.json` written by the holder), and exits with
    code 2. There is no `--takeover` flag and there will not be.
  - `SIGTERM` and `SIGINT` release the lock and remove the sidecar
    JSON before exiting.
  - Network filesystems (NFS / SMB / FUSE) are out of scope. The
    lock is plain POSIX advisory locking; the storage layer makes
    no claims beyond one host, one daemon.

CLI invocations (`conductor run`, `conductor agent …`,
`conductor audit …`) do not take the lock. They are short-lived
and coexist with the daemon through SQLite's normal WAL
concurrency, identical to V1.x behaviour. The lock is a
daemon-to-daemon exclusion, not a process-wide one.

WAL-mode SQLite remains in use. Its old job — "coordinate
multiple writers" — is gone. It is now a single-writer (the
daemon), multiple-reader contract for the lifetime of one daemon;
CLI writes piggyback through SQLite's regular commit semantics.

The default registry path remains V1 behaviour: `~/.conductor/registry.db`
or `<cwd>/.conductor/registry.db`. The token file (§3) lives at
`~/.config/conductor/serve.token`; the two are intentionally
separate — the token belongs to a daemon instance, the registry
belongs to a working directory / operator.

Consequences:

  - A duplicate-daemon startup fails fast with a clear error;
    silent corruption from "two daemons fighting the same
    SQLite" is now structurally impossible.
  - The on-disk surface of the registry directory becomes:
    `registry.db`, `conductor.lock`, `conductor.lock.json`. A
    single `rm -rf` on that directory resets both lock state and
    data.
  - CLI invocations behave exactly as V1.x. Auto-register on
    first sight keeps working; the daemon picks up new agents on
    its next read.
  - Supervisors (systemd, launchd, runit, tmux) that already do
    "spawn one, kill on shutdown, retry on exit" work without
    change; a fast restart before the lock release fails and the
    supervisor's retry loop should back off.
  - Operators running `conductor serve` against a network
    filesystem get an immediate hard error on daemon start
    instead of mysterious lock-timeout corruption later.

### 7. Failure semantics — explicit

  - **Daemon crashes mid-run**: the run's events are still in
    SQLite. There's no in-memory pending state to lose. A new
    daemon (or any subscriber) reads the events via
    `GET /v1/runs/{id}/events` and sees everything that happened
    up to the crash. SSE subscribers see only the streamed portion;
    they reconcile via `GET /events` if their stream drops.
  - **Backend subprocess survives daemon death**: daemon death
    drops the daemon's read of the backend's stdout. The next
    time the operator looks, the run's terminal `Result` is
    missing — but the events up to that point are recorded. This
    is a known asymmetry and called out: the registry is the
    authoritative record of "events seen", not "run completed".
  - **Concurrent run starts**: each `POST /runs` allocates a new
    goroutine that drives the existing `Backend.Execute`. The
    daemon's Mux routes each request independently; there's no
    request-level serialisation. SQLite's WAL handles the
    concurrent writers.
  - **SSE reconnect**: client loses connection → reconnects →
    sends `Last-Event-Id` → daemon replays from that event id
    forward. Missing events (the daemon crashed between SSE ack and
    SQLite commit) are caught by `GET /events` reconciliation.
    **No rewind**: events are append-only, never edited.

### 8. V1 surface — preserved verbatim

`conductor run`, `conductor agent …`, `conductor audit …` all
continue to work. They're implemented via the same `Backend.Execute`
+ recorder + audit package that's now also available over HTTP. The
**operator never has to migrate** from V1.x to V2 to keep working.

There is one runtime change worth flagging: when `conductor serve`
is running on the same machine, a CLI invocation opens a fresh
SQLite connection (not an HTTP round-trip). Concurrent edits
remain fine because SQLite serialises writes. But operators
shouldn't run `conductor run` and `conductor serve` against the
*same* `cwd` unless they know what they're doing — the CLI default
and the daemon default both write to `<cwd>/.conductor/registry.db`,
and two writers racing on the same file can confuse `OPEN`.

This is documented in `conductor serve --help`; no code-level
guard.

## Endpoint examples

**Start a run** (cURL):

```
POST /v1/runs
Authorization: Bearer <token>
Content-Type: application/json

{
  "agent_id": 7,
  "prompt": "review the staged diff",
  "opts": {"MaxTurns": 4, "ThinkingLevel": "medium"}
}
```

→ `202 Created`, body `Run` with id=42, status=running.

**Stream events**:

```
GET /v1/runs/42/stream
Authorization: Bearer <token>
Accept: text/event-stream

→ 200 OK, Content-Type: text/event-stream, body SSE
```

**Audit**:

```
GET /v1/runs/42/audit
→ 200 OK, body {"verdict":"pass","evidence":"…","audit_id":7,…}
     OR 404 Not Found if never audited.
```

## Non-goals (V2 boundaries — explicit)

  - **mTLS / OAuth / per-token scopes** — see §3. One shared
    bearer for V2. Anything richer needs a different ADR.
  - **Multi-user RBAC** — out. Operators who need per-user scoping
    can layer a reverse proxy. Conductor's contract is "the bearer
    owns the daemon."
  - **WebSocket / gRPC / Protobuf** — SSE + JSON covers V2 needs.
    WebSocket is the obvious choice if duplex control surfaces
    later (e.g. interactive cancel from browser); that's a future
    followup, not V2.
  - **Remote YAML parse** — `POST /agents` accepts the YAML body
    because it's a one-time upload, not a per-request eval. We
    never evaluate operator YAML on the data path (e.g. from a
    `POST /runs` body). Locks the attack surface on the operator's
    local file.
  - **OpenAPI / generated client SDKs** — not in V2. Operators
    hand-write scripts against the JSON contract. Stability of the
    `/v1/` prefix is the whole guarantee.
  - **Persistent SSE multi-host replication** — V2 supports many
    clients connecting to one daemon; it does not support one run
    streaming from many daemons. Operators who want HA run two
    daemons and route at the load-balancer.
  - **Audit-polling automation** — operators can cron
    `conductor audit --pending` from CLI; V2 exposes
    `GET /v1/audits/pending` for the same shape. Auto-audit-on-run
    is a separate decision (followup row to add if needed).

## Consequences

Positive:

  - IDE plugins, schedulers, and ad-hoc automation get a real
    surface — `POST /v1/runs` and `GET /v1/runs/{id}/stream`
    are the two endpoints 80% of use-cases need.
  - DAG scheduler (followup #13) has a real home — its triggers
    live as HTTP requests to the daemon's `/v1/runs` endpoint.
    Without this ADR, #13 was an architectural riddle.
  - Observability improves without any CLI changes — operators
    can `curl /v1/runs/{id}/stream` to attach a browser to a run
    that's already in flight.
  - The HTTP surface reuses Go structs from `internal/agentregistry`
    + `internal/audit`; no new wire types.

Negative:

  - The HTTP surface is the integration boundary for V2. Once
    shipped, `/v1/` schema changes are a contract. Operators
    will write scripts against this; large renames cost migration.
    Mitigation: the path prefix `/v1/` pins the contract; major
    changes land at `/v2/`.
  - One-bearer-auth is a sharp tool. We document the assumption and
    lean on Unix socket permissions + file mode 0600 for the
    generated-token file. Operators who expose beyond localhost
    get what they ask for.
  - SSE has known issues with proxy buffering. We document this in
    `conductor serve --help` and the `Running behind a proxy`
    section; nginx + `X-Accel-Buffering: no` is the standard
    workaround.
  - SSE vs WebSocket split: clients that want duplex control (e.g.
    interactive cancel from a UI) need a separate future ADR. V2
    accepts this limit.

## V2 implementation order (suggested; not part of this ADR)

Once this ADR is Accepted:

  1. **Minimum daemon** (steps 1-3 of the previous discussion):
     - `internal/http/server.go` with the endpoint table above.
     - All `GET`s; CLI subcommands kept; one `POST /runs` to start;
     - SSE for `GET /runs/{id}/stream`.
  2. **Audit endpoints** (`GET /runs/{id}/audit`, `POST /runs/{id}/audit:run`,
     `GET /audits/pending`).
  3. **Agent CRUD via HTTP** (already exists as `conductor agent …`).
  4. **Daemon file watcher**: each `conductor serve` reloads the
     registry file when it changes on disk (so a parallel CLI invocation
     shows up). This is a "free" win on top of WAL-mode SQLite and
     only worth doing if operators actually run mixed CLI+daemon.
  5. **Web UI** is a separate project, not part of this ADR.

Each step is one PR; each step is independently revertable. None
of them depend on each other.

## See also

  - ADR-0001 — V1 CLI-only (this ADR supersedes its deferral).
  - ADR-0005 — brief routing split (relevant because the daemon
    uses the same `InjectRuntimeConfig` path the CLI does).
  - ADR-0008 — agent registry persistence (this ADR's data layer).
  - ADR-0009 — adversarial audit loop (exposed via HTTP at the
    audit endpoints).
  - `docs/agent-layer.md` — operator guide for the CLI surface
    that V2's HTTP mirrors.
  - followups row #22 — removed by this ADR.

## Update log

### 2026-08-18 — `POST /v1/agents` body shape shipped in PR #4 differs from §4 (step 3)

The §4 endpoint table lists the body as a single field `config_yaml`
(parsed + validated). The actual implementation shipped in PR #4
(adopted as commit 7f898e9 on 2026-08-18) carries a multica-aligned
**multi-field JSON body** instead:

  - Required: `name`, `backend`
  - Optional: `description`, `parent` (name or `@id`),
    `instructions`, `model`, `thinking_level`,
    `runtime_config` (freeform JSON), `custom_args` (JSON array),
    `custom_env` (JSON map), `mcp_config` (JSON)

Rationale: multica's CLI/API uses this multi-field shape;
aligning V2 Conductor's wire shape to multica (and to V2 Conductor's
storage layer, which now carries the matching columns) made it
worthwhile to deviate from the literal §4 prose on this one row.
A follow-up PR may rename the §4 row so this paragraph stays
internal only.

## Update log

### 2026-08-18 — Operator decisions: token path + single-owner persistence

Two deviations from the original decision, both owner decisions on
2026-08-18 (chat transcript); rationale recorded for posterity.

#### (a) §3 default token path → `~/.config/conductor/serve.token`

The original default was `~/.conductor/token`. The product wants
the token file at `~/.config/conductor/serve.token` **literally**
(not via `os.UserConfigDir()` resolution). Cross-platform behaviour
is the operator's job; on Linux the default is the bytes above,
and on macOS / Windows the operator is expected to override
`--token-out` (Windows is refused outright by ADR-0003 anyway).

#### (b) §6 inverted: single daemon owns the registry

The original §6 said V2 supports multiple daemons against the same
SQLite, citing WAL coordination and leaving NFS / multi-host as an
operator concern. That is the wrong default for a local-first
daemon — the question "why would multiple instances appear?"
answered itself: they wouldn't, by construction. The new §6
flips the default to single-owner with `flock(2)` exclusivity,
no `--takeover` escape hatch, and an explicit out-of-scope note on
network filesystems. CLI invocations keep V1.x behaviour
(auto-register on first sight still writes) — the lock is a
daemon-to-daemon exclusion, not a process-wide one.

The original §6's "V2 supports running `conductor serve` on
multiple machines pointing at an NFS-mounted registry file"
sentence is therefore deleted; running two daemons against the
same registry is no longer a use case this ADR recognises.
