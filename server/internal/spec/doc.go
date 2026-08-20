// Package spec implements Conductor's spec CRUD (§6.2.5 of
// docs/design.md).
//
// A spec is a user-defined, reusable agent configuration. Each
// spec gets its own HOME directory so that the Codex app-server
// config.toml is written ONCE at spec creation and reused across
// invocations (so session JSONL can resume later — see §6.2.5).
//
// On-disk layout under $CONDUCTOR_HOME:
//
//	specs/<specId>/
//	├── spec.json     ← SpecRecord (AgentSpec + HomePath + ConfigToml)
//	└── home/         ← per-spec HOME for codex app-server
//	    ├── .codex/config.toml
//	    ├── .codex/sessions/<session-id>.jsonl
//	    └── .codex.json → $CONDUCTOR_HOME/.auth/<provider>/auth.json
//
// SpecId is content-addressable: "<sanitized-name>-<6-char-hash>"
// when the spec has a user-given Name, or "<16-char-hash>" otherwise.
// The hash is sha256 of the canonical JSON of AgentSpec with
// metadata fields (SpecId, CreatedAt, UpdatedAt) stripped, so the
// id is deterministic for a given spec content.
//
// BaseURL and EnvKey are passed in by the caller (typically the CLI
// layer, which reads ~/.codex/config.toml to resolve the chosen
// [model_providers.<id>] block). This package stays provider-
// agnostic — TOML parsing of the user's codex config is not its job.
package spec
