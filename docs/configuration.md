# `conductor.yaml` configuration

> **Source of truth:** `server/internal/configschema/schema.go`,
> loaded by `server/cmd/conductor/main.go`. Validation is strict
> (`yaml.Decoder.KnownFields(true)`) so unknown keys fail loudly.

## Schema (version 1)

```yaml
version: 1                  # required; only `1` is supported

agent:
  name: string              # required, human-readable label
  description: string       # optional, free-form
  backend: claude           # required, one of agent.SupportedTypes
  model: claude-sonnet-4-5  # optional, backend default otherwise
  thinking: medium          # optional, see "Thinking effort"
  cwd: .                    # optional, relative paths resolve against the YAML's dir
  max_turns: 30             # optional, 0 = unlimited
  timeout: 15m              # optional, Go duration: "30m" / "1h" / "0s"; "" = no bound
  prompt: |                 # optional, the agent brief (system)
    ...

  skills:                   # optional, plain-text briefs appended after `prompt:`
    - ./skills/style.md

  args: []                  # optional, extra CLI flags, blocklist-filtered

  env: {}                   # optional, extra env vars for the spawned CLI

  mcp:                      # optional, see "MCP servers"
    servers:
      - name: filesystem
        command: npx
        args: ["-y", "@modelcontextprotocol/server-filesystem", "."]
```

## Validation rules

`Schema.Validate()` (in `schema.go`) enforces:

1. `version` defaults to `1` if absent; anything else errors out.
2. `agent.backend` is required and must be in `agent.SupportedTypes`
   (`claude`, `codex` today).
3. `agent.max_turns >= 0`.
4. `agent.skills[i]` must not be empty/whitespace.
5. `agent.mcp.servers[i]` must have a non-empty `name`; transport is
   `"stdio"` (default) or `"http"`; HTTP entries must have a `url`;
   stdio entries must have a `command`.

The validator runs *before* path resolution, so error messages report
the original YAML-relative path. After validation, `resolvePaths`
rebases `agent.cwd` and every `agent.skills` entry against the YAML's
directory.

## Top-level fields → `agent.ExecOptions`

`Schema.ToExecOptions(extraPrompt, resumeID)` converts the YAML plus
runtime flags into the call to `agent.Backend.Execute`. The mapping:

| YAML field | ExecOptions field |
|---|---|
| `agent.backend`             | determines which `Backend` is constructed by `agent.New` |
| `agent.cwd`                 | `Cwd` |
| `agent.model`               | `Model` |
| `agent.thinking`            | `ThinkingLevel` |
| `agent.max_turns`           | `MaxTurns` |
| `agent.timeout`             | `Timeout` (parsed via `time.ParseDuration`) |
| `agent.prompt + skills`     | `SystemPrompt` (via `RuntimeBrief`) **and** appended to user prompt |
| `agent.args`                | `CustomArgs` (filtered per-backend by `*BlockedArgs`) |
| `agent.env`                 | `Env` |
| `agent.mcp.servers`         | `McpConfig` (via `McpConfigJSON`) |
| (cli flag) `--prompt`       | appended to the spawned CLI's prompt by `TaskPrompt` |
| (cli flag) `--resume`       | `ResumeSessionID` |

`SystemPrompt` routing is then handled by
`providerNeedsInlineSystemPrompt(backend)` — see
[backend.md](backend.md#the-prompt-vs-brief-split). For `claude` and
`codex` the brief is written to the workdir's `CLAUDE.md` / `AGENTS.md`
rather than duplicated into the CLI prompt.

## Thinking effort

`agent.thinking` is a backend-native effort value:

| Backend | Valid values |
|---|---|
| `claude` | `low` / `medium` / `high` / `xhigh` / `max` (maps to `--effort`) |
| `codex`  | `minimal` / `low` / `medium` / `high` (maps to `-c model_reasoning_effort=...`) |

The schema does not enforce per-backend vocabulary — the CLI passes
the string through and the backend filters at the constructor.

## MCP servers

`agent.mcp.servers` is a list. Each entry has:

```yaml
- name: filesystem             # required
  transport: stdio             # "stdio" (default) or "http"
  command: npx                 # required when transport=stdio
  args: ["-y", "@mcp/...", "."]
  env: {KEY: VAL}              # optional, merged into the spawned env
  url: https://example/mcp     # required when transport=http
  headers: {Authorization: ...} # optional, http-only
```

For `claude`, the YAML is rendered to a JSON object matching Claude
Code's `--mcp-config` schema. With an empty `servers` list the CLI
inherits whatever the user has configured locally — conductor does not
write an empty config.

For `codex`, the JSON is rewritten into TOML and atomically written
to `$CODEX_HOME/config.toml` (`codex_mcp.go`); `CODEX_HOME` falls back
to `~/.codex` when unset. The file is mode `0o600` because argv would
echo env-bearing secrets through `ps` / process listings, so conductor
keeps them off the command line.

## Output channels

`conductor run` writes two streams:

- **stderr** — per-event rendering (default), `--quiet` silent, or
  `--json` one JSON object per line with `{"kind":"event","payload":{...}}`
- **stdout** — the agent's final answer (only). Use the conventional
  `conductor run ... > out.md` to capture.

`Result.Status != "completed"` causes the CLI to exit non-zero with
`"conductor: <status>: <error>"` on stderr.

## Minimal example

```yaml
version: 1
agent:
  name: hello
  backend: claude
  prompt: "Say hi in one line."
```

```bash
conductor run --config agent.yaml
```

See `server/examples/code-review-agent.yaml` for a fuller example that
uses `model`, `thinking`, `max_turns`, `timeout`, `skills`, and `mcp`.
