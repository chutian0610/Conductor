# 3. Refuse to spawn on Windows; macOS + Linux only

Date: 2026-08-17
Status: Accepted

## Context

Cancellation correctness in Conductor relies on Unix process groups:

```go
// server/internal/agent/proc_other.go
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
...
kill(-pgid, sig)        // SIGTERM to whole tree
...
kill(-pgid, sig)        // SIGKILL to whole tree
```

This makes sure that when the user presses Ctrl-C, the LLM CLI *and
every subprocess it spawned* (npm downloads, MCP servers like the
filesystem MCP, child shells) all receive SIGTERM in one shot, and
if they don't exit in 5 s, all receive SIGKILL.

There is no portable Windows equivalent. Job Objects offer some of
the same properties (process-tree cleanup, kill-the-tree semantics),
but the API surface and the lack of an immediate signal story mean a
real port is materially different code, not a build-tag flip.

## Decision

`server/internal/agent/proc_windows.go` exists but only returns an
explicit error at execute time:

> "Windows is not supported by conductor's process-group machinery;
> macOS and Linux only."

The binary still compiles on `windows/*` (so the build matrix is not
broken), it just refuses to spawn agents at runtime.

Build matrix excludes `windows/*` from CI runners.

## Consequences

Positive:
- V1 stays small; no special-casing of signals, Job Objects, console
  allocation, etc.
- Runners can use macOS + Linux image runners trivially.
- The README's "Platform support" table is honest.

Negative:
- A Windows developer who clones the repo can `go build` successfully
  but their first `conductor run` will print the refusal and exit
  non-zero. Mitigated by the error message being explicit.
- V2 should consider Job Objects — the syscall reach is real and the
  signal story can be made to work — but only if there's a real demand
  signal.

## See also

[`docs/process-model.md`](../process-model.md) for the layered
cancellation story and the process-tree diagram.
