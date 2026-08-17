// Package agentregistry is the Conductor "agent layer" — the persistent
// entity registry that sits above the backend driver (package
// [conductor/server/internal/agent]).
//
// The two packages address different questions:
//
//   - package agent answers "how do I drive Claude Code or the Codex CLI
//     as a subprocess?" (subprocess plumbing + streaming-JSON protocol).
//   - package agentregistry answers "who are my agents, what runs have
//     they done, what came out of each run, and which agent spawned
//     which?" (persistent catalog + run history + audit trail).
//
// The vocabulary mirrors multica's `goal_manager.py`, which Conductor
// refactored into a general-purpose agent layer: a SQLite store with WAL
// + busy_timeout, identity env vars for parent/child distinction, and a
// small CRUD surface that the CLI exposes as `conductor agent ...`.
//
// The registry lives at `<cwd>/.conductor/registry.db`. Pass an empty
// cwd to Open to get a transient in-memory database (tests).
package agentregistry
