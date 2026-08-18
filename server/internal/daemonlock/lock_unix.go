//go:build unix

package daemonlock

import (
	"errors"
	"os"
	"syscall"
)

// acquireFlock takes LOCK_EX | LOCK_NB on f. A contention result
// is translated to [ErrAlreadyHeld]; all other errors are returned
// verbatim. The caller is responsible for closing the file
// descriptor on error.
func acquireFlock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrAlreadyHeld
		}
		return err
	}
	return nil
}

// releaseFlock drops the flock (LOCK_UN). Idempotent: subsequent
// calls return nil per syscall.Flock semantics, but the caller
// should not rely on this.
func releaseFlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
