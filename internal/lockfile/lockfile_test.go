package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWithLockRunsFnAndReleasesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")

	runs := 0
	for attempt := range 2 {
		if err := WithLock(path, func() error {
			runs++
			return nil
		}); err != nil {
			t.Fatalf("WithLock() attempt %d error = %v", attempt, err)
		}
	}

	if runs != 2 {
		t.Fatalf("fn ran %d times, want 2 — the lock was not released", runs)
	}
}

func TestWithLockSerializesConcurrentHolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")

	// flock is invisible to the race detector, so the overlap counters have to be
	// atomic: a plain shared int would be reported as a data race rather than as
	// the overlap this test is looking for.
	var inFlight, overlaps, completed atomic.Int64
	var holders sync.WaitGroup
	for range 8 {
		holders.Add(1)
		go func() {
			defer holders.Done()
			if err := WithLock(path, func() error {
				if inFlight.Add(1) > 1 {
					overlaps.Add(1)
				}
				inFlight.Add(-1)
				return nil
			}); err != nil {
				return
			}
			completed.Add(1)
		}()
	}
	holders.Wait()

	if got := overlaps.Load(); got != 0 {
		t.Errorf("overlapping holders = %d, want 0", got)
	}
	if got := completed.Load(); got != 8 {
		t.Errorf("successful runs = %d, want 8", got)
	}
}

func TestWithLockReturnsTheFunctionError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	sentinel := errors.New("sentinel")

	err := WithLock(path, func() error { return sentinel })

	if !errors.Is(err, sentinel) {
		t.Fatalf("WithLock() error = %v, want %v", err, sentinel)
	}
}

func TestWithLockFailsWhenTheLockCannotBeOpened(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ran := false
	err := WithLock(filepath.Join(file, "child.lock"), func() error {
		ran = true
		return nil
	})

	if err == nil {
		t.Fatal("WithLock() error = nil, want a failure when the lock cannot be opened")
	}
	if ran {
		t.Error("fn ran without the lock being held")
	}
}
