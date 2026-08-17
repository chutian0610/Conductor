# 5. System brief is delivered through both per-workdir context file *and* prompt (V1 only)

Date: 2026-08-17
Status: Accepted (V1), superseded by V2 follow-up

## Context

`InjectRuntimeConfig(workDir, provider, brief)` writes
`<workDir>/CLAUDE.md` for Claude and `<workDir>/AGENTS.md` for Codex.
Both CLIs read those files natively from the working directory.

The brief is the agent's persistent system instructions — it's long-
lived context that should land on disk, not in the per-turn prompt.

But `claude`/`codex` in V1 still also receive the brief as part of
their prompt input — there's no clean separation in either CLI
between "system instructions" and "this turn's task".

## Decision

For V1, deliver the brief through **both**:

1. Write it to the per-workdir context file (Claude / Codex read it
   natively).
2. Concatenate it onto the prompt passed to the CLI.

`providerNeedsInlineSystemPrompt(provider)` exists as the routing
decision: it returns `true` for any future provider that does NOT
read a per-workdir file (so the brief has to ride inline). In V1
both providers read their context files, so the function currently
always returns `false` — it's wired up but unused.

The schema layer keeps the two paths separate:

- `Schema.RuntimeBrief()` — concatenates `agent.prompt` + skill
  files; this is what the backend persists to disk.
- `Schema.TaskPrompt(extraPrompt)` — appends `--prompt` to the runtime
  brief; this is what the backend passes to the CLI.

## Consequences

Positive:
- Works for both backends today with one abstraction.
- Adding a non-disk-delivery provider in V2 requires only adding a
  case to `providerNeedsInlineSystemPrompt` and trusting the
  existing prompt concatenation.

Negative:
- The brief is delivered twice (file + prompt) — wasteful for
  long briefs and a possible source of "I see you said X but the
  agent is acting like it didn't" confusion in logs.

## V2 plan

`TaskPrompt` should split: `SystemPrompt` becomes the persistent
brief (routed by `providerNeedsInlineSystemPrompt` to either disk or
inline), and the prompt argument passed to the CLI carries only the
per-turn task. The duplication goes away.

## See also

[`docs/backend.md`](../backend.md#the-prompt-vs-brief-split) —
the routing decision in prose.
