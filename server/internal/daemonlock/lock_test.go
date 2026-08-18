package daemonlock_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"conductor/server/internal/daemonlock"
)

func TestAcquireReleaseRoundtrip(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	l, err := daemonlock.Acquire(ctx, dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if l == nil {
		t.Fatal("Acquire returned nil lock")
	}

	// Sidecar JSON should exist with our PID.
	h, err := daemonlock.ReadHolder(dir)
	if err != nil {
		t.Fatalf("ReadHolder: %v", err)
	}
	if h == nil {
		t.Fatal("ReadHolder returned nil; expected holder info")
	}
	if h.PID != os.Getpid() {
		t.Errorf("holder PID = %d, want %d", h.PID, os.Getpid())
	}
	if h.Host == "" {
		t.Error("holder Host is empty")
	}

	l.Release()

	// Same-process re-acquire must succeed since Release closed the fd.
	l2, err := daemonlock.Acquire(ctx, dir)
	if err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	defer l2.Release()
}

func TestAcquireContention(t *testing.T) {
	dir := t.TempDir()

	holder, err := daemonlock.Acquire(context.Background(), dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer holder.Release()

	contender, err := daemonlock.Acquire(context.Background(), dir)
	if err == nil {
		contender.Release()
		t.Fatal("second Acquire succeeded; expected ErrAlreadyHeld")
	}
	if !errors.Is(err, daemonlock.ErrAlreadyHeld) {
		t.Errorf("second Acquire error = %v, want ErrAlreadyHeld", err)
	}
	if !strings.Contains(err.Error(), "pid=") {
		t.Errorf("contention error missing holder info: %v", err)
	}
}

func TestAcquireCreatesMissingDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "deep", ".conductor")
	// dir doesn't yet exist; Acquire must MkdirAll.
	l, err := daemonlock.Acquire(context.Background(), dir)
	if err != nil {
		t.Fatalf("Acquire into missing dir: %v", err)
	}
	defer l.Release()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected dir, got mode %v", info.Mode())
	}
}

func TestReadHolderMissing(t *testing.T) {
	dir := t.TempDir()
	h, err := daemonlock.ReadHolder(dir)
	if err != nil {
		t.Fatalf("ReadHolder on missing sidecar: %v", err)
	}
	if h != nil {
		t.Fatalf("ReadHolder on missing sidecar = %v, want nil", h)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	l, err := daemonlock.Acquire(context.Background(), dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release()
	l.Release() // must not panic, must not double-remove errors
	l.Release()
}
