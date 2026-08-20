// Package home implements per-spec HOME isolation for the Codex
// provider (v0.13 of docs/design.md §6.2.5).
//
// Why per-spec HOME:
//
//   - Codex's app-server reads ~/.codex/config.toml on startup and
//     locks onto one model_provider for the session. Different specs
//     need different providers, so each spec gets its own HOME.
//   - Session JSONL lives under HOME/.codex/sessions/<id>.jsonl.
//     Same spec across invocations share the same HOME and can
//     therefore --resume previous sessions.
//   - config.toml is written ONCE at spec creation; subsequent
//     invocations are read-only on it (avoids the race where two
//     Codex spawns would stomp each other's config).
//
// Layout under $CONDUCTOR_HOME:
//
//	.auth/                                  ← shared auth (per provider)
//	├── openai/auth.json
//	├── openrouter/auth.json
//	└── ollama/                             ← local, no auth
//	specs/<specId>/
//	├── spec.json                            ← AgentSpec + SpecRecord metadata
//	└── home/                                ← per-spec HOME
//	    ├── .codex/
//	    │   ├── config.toml                  ← written at spec create
//	    │   └── sessions/<session-id>.jsonl
//	    └── .codex.json                      ← symlink → ../../.auth/<provider>/auth.json
package home
