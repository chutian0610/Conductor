package daemonlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrAlreadyHeld is returned by [Acquire] when the registry lock
// is already held by another `conductor serve` process. The sentinel
// is exposed so the cmd/conductor main path can map it onto exit
// code 2 per ADR-0010 §6 failure semantics.
var ErrAlreadyHeld = errors.New("conductor serve: registry is already held by another daemon")

// ErrUnsupportedPlatform is returned by [Acquire] on platforms for
// which the package has no flock implementation. ADR-0003 already
// refuses Windows at runtime; this error is the lock layer's
// contribution to that refusal.
var ErrUnsupportedPlatform = errors.New("conductor serve: daemon lock not supported on this platform")

// Holder is the metadata written to the sidecar JSON so the
// second-startup error message can name the current owner.
type Holder struct {
	PID       int       `json:"pid"`
	Host      string    `json:"host"`
	StartedAt time.Time `json:"started_at"`
}

// Lock represents a held registry lock. [Lock.Release] must be
// called exactly once; calling it twice is a no-op.
type Lock struct {
	dir  string
	file *os.File
	on   bool // true after Release has been called
}

// Acquire takes an exclusive, non-blocking flock on
// `<dir>/conductor.lock` and writes the sidecar JSON with the
// holder metadata. The directory is created mode 0755 if absent;
// the lock file is created mode 0600. If another process already
// holds the lock, returns [ErrAlreadyHeld] (wrapped with the
// holder's PID / host / start time, read from the existing
// sidecar JSON via [ReadHolder]).
//
// The returned *Lock is the in-process handle; call
// [Lock.Release] to drop it.
func Acquire(ctx context.Context, dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("daemonlock: mkdir %s: %w", dir, err)
	}
	lockPath := filepath.Join(dir, DefaultLockName)
	jsonPath := filepath.Join(dir, DefaultHolderJSONName)

	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemonlock: open %s: %w", lockPath, err)
	}

	if err := acquireFlock(f); err != nil {
		if errors.Is(err, ErrAlreadyHeld) {
			if h, herr := readHolder(jsonPath); herr == nil && h != nil {
				_ = f.Close()
				return nil, fmt.Errorf("%w (pid=%d host=%s started_at=%s)",
					ErrAlreadyHeld, h.PID, h.Host, h.StartedAt.Format(time.RFC3339))
			}
		}
		_ = f.Close()
		return nil, err
	}

	holder := Holder{
		PID:       os.Getpid(),
		Host:      hostname(),
		StartedAt: time.Now().UTC(),
	}
	if err := writeHolder(jsonPath, holder); err != nil {
		_ = releaseFlock(f)
		_ = f.Close()
		_ = os.Remove(jsonPath)
		return nil, fmt.Errorf("daemonlock: write holder json: %w", err)
	}

	return &Lock{dir: dir, file: f}, nil
}

// Release drops the flock, removes the sidecar JSON, and closes the
// lock file fd. Safe to call multiple times. After Release, the
// lock is no longer held by this process.
func (l *Lock) Release() {
	if l == nil || l.on {
		return
	}
	l.on = true
	jsonPath := filepath.Join(l.dir, DefaultHolderJSONName)
	_ = os.Remove(jsonPath)
	if l.file != nil {
		_ = releaseFlock(l.file)
		_ = l.file.Close()
	}
}

// ReadHolder returns the holder metadata from the sidecar JSON in
// the given registry directory, or (nil, nil) if the sidecar is
// absent. Useful for diagnostics when [Acquire] reports a
// conflict.
func ReadHolder(dir string) (*Holder, error) {
	return readHolder(filepath.Join(dir, DefaultHolderJSONName))
}
