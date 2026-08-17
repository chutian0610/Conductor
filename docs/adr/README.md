# Architecture Decision Records

ADRs capture the **why** behind Conductor's V1 design choices that are
otherwise only mentioned in source-code comments. The full source of
truth is still the code — these records just make the rationale
discoverable and stable across refactors.

Convention: each ADR is a small Markdown file under this directory,
named `NNNN-slug.md`. The next number is `0008`.

## Index

| # | Title | Status |
|---|---|---|
| [0001](0001-v1-cli-only-no-http.md) | V1 is CLI-only; no HTTP server until V2 | Accepted |
| [0002](0002-codex-exec-json-not-app-server.md) | Codex backend uses `codex exec --json`, not the app-server websocket | Accepted |
| [0003](0003-refuse-windows-run-time.md) | Refuse to spawn on Windows; macOS + Linux only | Accepted |
| [0004](0004-strict-yaml-schema.md) | `conductor.yaml` is parsed with strict `KnownFields(true)` decoding | Accepted |
| [0005](0005-brief-duplicated-disk-and-prompt-v1.md) | System brief is delivered through both per-workdir context file *and* prompt (V1 only) | Accepted (V1), superseded by V2 follow-up |
| [0006](0006-per-backend-blocked-args.md) | Per-backend blocklist prevents user `args` from overriding protocol-owned flags | Accepted |
| [0007](0007-fake-binary-not-pure-mock.md) | Integration tests use a fake subprocess binary, not a pure-Go mock | Accepted |
