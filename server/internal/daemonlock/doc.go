// Package daemonlock enforces the "single daemon owns the registry"
// invariant pinned by ADR-0010 §6. One `conductor serve` process can
// hold the lock at a time; a second process against the same
// directory exits with [ErrAlreadyHeld] so the operator gets a clear
// error instead of silent SQLite corruption.
//
// The lock itself is plain POSIX flock(2) on
// `<db-dir>/conductor.lock`; a sidecar `conductor.lock.json`
// records the holder's PID, hostname, and start time so the
// second-startup error message can name the conflict.
//
// POSIX advisory locking only works on a single host with a local
// filesystem. Network filesystems (NFS / SMB / FUSE) are explicitly
// out of scope — the caller should not point two daemons at them,
// and the lock file does not pretend to coordinate across hosts.
package daemonlock

// DefaultLockName and DefaultHolderJSONName are the two sidecar
// paths rooted under the directory passed to [Acquire]. They are
// kept in lock-step: the lockfile carries the flock, the JSON
// carries the holder metadata.
const (
	DefaultLockName       = "conductor.lock"
	DefaultHolderJSONName = "conductor.lock.json"
)
