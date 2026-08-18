| [0007](0007-fake-binary-not-pure-mock.md) | Integration tests use a fake subprocess binary, not a pure-Go mock | Accepted |
| [0008](0008-agent-registry-persistence-and-identity.md) | Agent registry persistence + identity env propagation (CONDUCTOR_AGENT_ID / CONDUCTOR_PARENT_AGENT_ID) | Superseded by V1.x refresh (Update log) |
| [0009](0009-adversarial-audit-loop.md) | Adversarial audit loop — a fresh LLM subprocess judges a recorded run | Accepted |
| [0010](0010-v2-http-transport.md) | V2 HTTP transport (bearer token, `/v1/` routes, single-daemon registry lock). Supersedes ADR-0001's deferral. | Accepted (V2; supersedes ADR-0001) |

## Index

| # | Title | Status |
|---|---|---|
| [0001](0001-v1-cli-only-no-http.md) | V1 is CLI-only; no HTTP server until V2 | Superseded (see [ADR-0010](0010-v2-http-transport.md)) |
| [0002](0002-codex-exec-json-not-app-server.md) | Codex backend uses `codex exec --json`, not the app-server websocket | Accepted |
| [0003](0003-refuse-windows-run-time.md) | Refuse to spawn on Windows; macOS + Linux only | Accepted |
| [0004](0004-strict-yaml-schema.md) | `conductor.yaml` is parsed with strict `KnownFields(true)` decoding | Accepted |
| [0005](0005-brief-duplicated-disk-and-prompt-v1.md) | System brief is delivered through both per-workdir context file *and* prompt (V1 only) | Accepted (V1), superseded by V2 follow-up |
| [0006](0006-per-backend-blocked-args.md) | Per-backend blocklist prevents user `args` from overriding protocol-owned flags | Accepted |
| [0007](0007-fake-binary-not-pure-mock.md) | Integration tests use a fake subprocess binary, not a pure-Go mock | Accepted |
| [0008](0008-agent-registry-persistence-and-identity.md) | Agent registry persistence + identity env propagation | Superseded by V1.x refresh (Update log) |
| [0009](0009-adversarial-audit-loop.md) | Adversarial audit loop — a fresh LLM subprocess judges a recorded run | Accepted |
| [0010](0010-v2-http-transport.md) | V2 HTTP transport — single bearer token, `/v1/` routes, single-daemon registry lock. Supersedes ADR-0001. | Accepted (V2) |

## Reading order

1. **[0007](0007-fake-binary-not-pure-mock.md)** — the integration
   test strategy; everything below assumes you read this first
   because the wire contracts were decided with "test by running
   a fake binary" in mind.
2. **[0009](0009-adversarial-audit-loop.md)** — the audit surface
   the CLI (`conductor audit ...`) and the V2 HTTP layer
   (`/v1/runs/{id}/audit`, `/v1/runs/{id}/audit:run`,
   `/v1/audits/pending`) both wrap.
3. **[0010](0010-v2-http-transport.md)** — the V2 HTTP layer.
   Read this if you are wiring up an external client (CI hook,
   IDE plugin, Web UI) to the daemon.