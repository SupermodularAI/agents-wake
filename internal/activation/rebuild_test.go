package activation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/lockfile"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// The acceptance criterion in its strongest observable form: an append that is
// mid-write when a rebuild starts must not have its bytes land on an inode the
// rebuild already unlinked. The appender opens its descriptor first — exactly as
// store.appendLocked does — and only then lets the rebuild run, so with the removal
// unsynchronised the spool it is writing to is gone before it returns, and the bytes
// go nowhere no reader can reach.
func TestRebuildDoesNotUnlinkTheSpoolAConcurrentAppendIsWriting(t *testing.T) {
	paths, claudeDir, _ := triggerFixture(t)
	spool := filepath.Join(paths.DataDir, eventsFile)

	line, err := record.Marshal(concurrentEvent())
	if err != nil {
		t.Fatalf("record.Marshal() error = %v", err)
	}

	opened := make(chan struct{})
	appenderDone := make(chan error, 1)
	go func() {
		appenderDone <- lockfile.WithLock(spool+".lock", func() error {
			file, openErr := os.OpenFile(spool, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if openErr != nil {
				return openErr
			}
			defer file.Close()
			close(opened)
			// The window the unsynchronised removal races into. The sleep only widens
			// the failure window; the assertions below do not depend on it elapsing.
			time.Sleep(100 * time.Millisecond)
			if _, writeErr := file.Write(append(line, '\n')); writeErr != nil {
				return writeErr
			}
			if syncErr := file.Sync(); syncErr != nil {
				return syncErr
			}
			published, statErr := os.Stat(spool)
			if statErr != nil {
				return fmt.Errorf("the rebuild removed the spool this append was writing: %w", statErr)
			}
			written, statErr := file.Stat()
			if statErr != nil {
				return statErr
			}
			if !os.SameFile(published, written) {
				return errors.New("the append wrote to an inode the spool path no longer names")
			}
			return nil
		})
	}()
	<-opened

	if _, rebuildErr := Rebuild(paths, claudeDir); rebuildErr != nil {
		t.Fatalf("Rebuild() error = %v", rebuildErr)
	}
	if appendErr := <-appenderDone; appendErr != nil {
		t.Fatalf("concurrent append: %v", appendErr)
	}
}

// Rebuild takes the spool lock; Trigger takes ingest.lock and then, through Ingest
// and Append, the spool lock. Nothing takes them in the other order, so they cannot
// cycle — and Rebuild must hold neither across its own re-ingest, which would
// deadlock against that ingest's own Append.
func TestRebuildAndTriggerDoNotDeadlock(t *testing.T) {
	paths, claudeDir, _ := triggerFixture(t)

	done := make(chan error, 2)
	go func() { _, err := Rebuild(paths, claudeDir); done <- err }()
	go func() { _, err := Trigger(paths, claudeDir); done <- err }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("concurrent Rebuild/Trigger error = %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Rebuild and Trigger deadlocked")
		}
	}

	// Both locks are free afterwards: a Rebuild that held either across its re-ingest
	// would still be holding it here.
	for _, path := range []string{filepath.Join(paths.DataDir, eventsFile) + ".lock", filepath.Join(paths.DataDir, ingestLockName)} {
		locked, err := lockfile.TryWithLock(path, func() error { return nil })
		if err != nil {
			t.Fatalf("TryWithLock(%s) error = %v", filepath.Base(path), err)
		}
		if !locked {
			t.Errorf("locked = false for %s, want true — the lock is still held after Rebuild returned", filepath.Base(path))
		}
	}
}

// concurrentEvent is a valid record whose bytes the interleaving test appends by
// hand, because an Append inside the lock would block on the lock it already holds.
// It mirrors the store's own test fixture; only the source identity differs.
func concurrentEvent() record.Record {
	outcome := record.OutcomeOK
	return record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID("claude-code", "concurrent-append"),
		Timestamp:     time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC),
		Harness:       "claude-code",
		SessionID:     "session-1",
		Repo:          "0123456789abcdef0123456789abcdef",
		Kind:          record.KindSkill,
		Name:          "review",
		Invoker:       record.InvokerModel,
		Outcome:       &outcome,
	}
}
