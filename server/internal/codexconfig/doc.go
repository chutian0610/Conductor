// Package codexconfig reads the user's ~/.codex/config.toml (or
// wherever $CODEX_HOME points) and resolves [model_providers.<id>]
// blocks. Conductor needs base_url + env_key at spec-create time
// to populate the per-spec HOME config.toml (§6.2.5).
//
// Phase 1 uses a hand-rolled TOML reader for the subset we need:
//
//	# comments OK, blank lines OK
//	[model_providers.<id>]
//	name     = "Display Name"      ; optional
//	base_url = "https://..."        ; required for non-built-in
//	env_key  = "ENV_VAR_NAME"       ; optional (empty for local)
//
// Sections outside [model_providers.*] are ignored (forward compat:
// Codex adds new top-level tables over time). Unknown keys inside
// [model_providers.*] are also ignored (so users can have extra
// fields without breaking us).
//
// Phase 2 should swap this for a real TOML library
// (pelletier/go-toml/v2 or BurntSushi/toml) if more complex
// config.toml shapes are needed. The public API (Resolve / ReadFile
// / Parse) would stay the same.
package codexconfig
