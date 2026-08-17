# 4. `conductor.yaml` is parsed with strict `KnownFields(true)` decoding

Date: 2026-08-17
Status: Accepted

## Context

`server/internal/configschema/schema.go` decodes `conductor.yaml`
with `yaml.NewDecoder(bytes.NewReader(raw)); dec.KnownFields(true)`.
That flag makes the decoder error on any key that isn't part of the
`Schema` struct.

The alternative is to silently ignore unknown keys, which is the
`encoding/json` (and yaml-by-default) behaviour.

## Decision

Strict decode. `Schema.Validate()` runs after decoding and surfaces a
clear error: `schema: agent.backend is required` / `agent.skills[0]
is empty` / `unknown field "..."`. The error message includes the
field path so the user can find the typo in their YAML.

## Consequences

Positive:
- Typos like `thiking:` instead of `thinking:` fail loudly at load
  time, not at runtime when the model silently uses its default.
- Adding a new field to `Schema` doesn't break old configs but
  *does* surface unknown keys from those configs.
- The validator runs before path resolution, so error messages
  report the original YAML-relative path.

Negative:
- A user with a hand-written YAML that has stray comments or extra
  whitespace will get errors that look noisy. (In practice
  `Unknown field` errors are rare unless someone hand-edits.)
- A future schema migration needs to either keep the old field as a
  no-op alias or announce the breaking change in the field's deprecation
  note.

## Follow-ups

- When V2 grows new top-level sections (`plan:`, `dag:`), they need
  to be added to `Schema` AND meaningfully validated; the
  `KnownFields(true)` flag means a typo in `agent.dag.ndoes:` instead
  of `nodes:` will surface immediately, which is the desired behaviour.
