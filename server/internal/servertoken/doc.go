// Package servertoken materialises the bearer token that
// `conductor serve` requires on every incoming HTTP request.
// Per ADR-0010 §3 (see the 2026-08-18 Update log entry):
//
//   - If the env var (CONDUCTOR_TOKEN by default) is set and
//     non-empty, that string is the token; no file is read or
//     written.
//   - Otherwise, [LoadOrGenerate] reads a token from the configured
//     path; if absent, generates a fresh 32-byte URL-safe token,
//     writes it mode 0600 to that path, and returns it.
//
// The default path on Linux is exactly `~/.config/conductor/serve.token`
// — `os.UserConfigDir()` is intentionally NOT used.
package servertoken
