//go:build !unix

package daemonlock

import "os"

// Acquire on non-unix (Windows etc.) refuses explicitly. ADR-0003
// already refuses Windows at runtime; this stub mirrors that
// policy at the lock layer so the error surfaces in the same
// place (the conductor serve startup path).
func acquireFlock(f *os.File) error {
	return ErrUnsupportedPlatform
}

func releaseFlock(f *os.File) error {
	return nil
}
