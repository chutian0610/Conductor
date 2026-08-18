// Package runmgr owns the in-process lifecycle of a `conductor serve`
// run. It is the scheduler between HTTP requests and the underlying
// backend spawn driver, and the broadcaster that fans live backend
// events out to Server-Sent Events subscribers.
//
// One Manager per server; carries one *agentregistry.Store for
// persistence and one slog.Logger for diagnostics. Concurrency
// model: each active run lives in its own goroutine (spawned by
// [Manager.StartRun]) and writes events to the store synchronously;
// SSE subscribers receive events through per-subscriber buffered
// channels fed by the same goroutine. When the run finishes the
// broadcaster closes all subscriber channels and the goroutine
// exits; the run row remains in the store for replay.
//
// Restarts: the Manager keeps no on-disk state of its own. Active
// runs are lost if the daemon crashes mid-execution; runs that
// already called FinishRun before the crash survive in the
// registry and replay cleanly on reconnect. SSE subscribers that
// reconnect after a crash see the registry replay and miss only
// events between the last successful AppendEvent and the crash;
// the same gap any unreplicated streaming system accepts.
package runmgr

// StreamFormat is the event-id header name used on SSE connections.
// Matches ADR-0010 §5; clients send "Last-Event-Id: N" on resume.
const StreamHeaderLastEventID = "Last-Event-Id"
