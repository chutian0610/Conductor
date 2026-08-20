// Package storage persists run state and the streamed event
// timeline. Two implementations live behind the same interface:
//
//   - JsonFileStorage (Phase 1, this package): one directory per
//     run under $CONDUCTOR_HOME/runs/<runId>/ with a state.json
//     snapshot and an append-only timeline.ndjson log. Easy to
//     eyeball, easy to back up, no daemon required.
//
//   - SqliteStorage (Phase 2+): single SQLite file under
//     $CONDUCTOR_HOME/conductor.db; same wire schema, single
//     timeline table, ideal once you cross ~10k runs.
//
// §11.1 of docs/design.md describes the swap. Phase 1 ships
// only JsonFileStorage; the Storage interface is shaped so the
// SQLite impl is a drop-in.
//
// Layout under $CONDUCTOR_HOME:
//
//	runs/<runId>/
//	├── state.json       # RunState snapshot (atomic tmp+rename)
//	└── timeline.ndjson  # append-only NDJSON of TimelineItem
//
// All methods are goroutine-safe per runId (per-run sync.Mutex
// on state.json). Concurrent invocations of different runIds do
// not contend. Cross-process safety is a Phase 2 concern; the
// SQLite impl will provide it via WAL mode.
package storage
