package activation

import (
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/rollup"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// openEvents opens the event spool with its derived data attached.
//
// Every path in this package that touches the spool goes through here, so the
// rollup directory is removed by any Discard without each call site having to
// remember it — see store.Store.derived for why the spool owns that rather than
// its callers.
func openEvents(paths config.Paths) *store.Store {
	return store.New(filepath.Join(paths.DataDir, eventsFile)).WithDerived(rollup.DirName)
}

// rollupWindow reports how old an event must be before it may be sealed into a
// block, and whether rollups are enabled at all.
//
// store.rollup_after is the switch. It has existed since T002 with the sentinel
// default "never" and enforced nothing (ADR-0014); "never" still enforces
// nothing, so a user who has not set it sees exactly today's behaviour. A
// duration turns sealing on.
//
// The key stays a duration and is not reinterpreted as a count. How many events
// fall in seven days differs per machine, so a duration cannot name a
// position-range boundary; it decides only which events are old enough to seal,
// while the tiering underneath stays count-based on store positions.
func rollupWindow(paths config.Paths) (time.Duration, bool) {
	settings, err := config.Load(paths)
	if err != nil {
		return 0, false
	}
	window, usable, err := settings.Duration("store.rollup_after")
	if err != nil || !usable {
		return 0, false
	}
	return window, true
}

// sealRollups brings the rollup directory up to date, and does nothing at all
// unless store.rollup_after names a duration.
//
// It builds forward: enabling rollups records a floor at the current head and
// seals only what follows. History already in the spool stays unsealed until
// BackfillRollups is called, which is the explicit opt-in the design asks for.
func sealRollups(paths config.Paths, events *store.Store) error {
	return rollupPass(paths, events, rollup.Seal)
}

// BackfillRollups seals history from before rollups were enabled.
//
// Exported because it is a thing the user asks for rather than a thing a scan
// does, and it is a no-op unless store.rollup_after names a duration — there is
// no floor to lower until sealing is on.
func BackfillRollups(paths config.Paths, events *store.Store) error {
	return rollupPass(paths, events, rollup.Backfill)
}

// rollupPass is the shared body of a seal and a backfill: the two differ only in
// which of rollup's entry points they call.
func rollupPass(paths config.Paths, events *store.Store, pass func(string, *store.Store, uint64, uint64) (rollup.SealResult, error)) error {
	window, enabled := rollupWindow(paths)
	if !enabled {
		return nil
	}
	head, err := events.Head()
	if err != nil {
		return err
	}
	frontier, err := sealableHead(events, head, window)
	if err != nil {
		return err
	}
	// Called even when nothing is sealable, so enabling on an empty or entirely
	// recent store still records the floor. Deferring that to the first scan
	// with sealable history would put the floor above everything appended in
	// between, and nothing below a floor is ever sealed.
	//
	// Both numbers are passed: sealing stops at the frontier, while pruning has
	// to keep whatever a view over the whole store would read.
	_, err = pass(paths.DataDir, events, frontier, head)
	return err
}

// sealableHead is the highest position that may be sealed: the largest fanout
// boundary below which every record is older than the window.
//
// Positions are not time-ordered — a transcript imported today can carry an
// event from last week, which is the same reason internal/remote/watermark.go
// rejects a timestamp cursor — so "the first record inside the window" is not
// the end of the sealable region. Taking it as the end was a real bug and a
// silent one: a single recent record at a low position collapsed the frontier to
// zero and nothing was ever sealed on that machine, with no error anywhere.
//
// So the scan does not stop at the first crossing. It finds the last block
// boundary with no in-window record below it, which is the strongest frontier
// that is still honest: nothing newer than the window is ever sealed, and one
// early recent record costs only the block it falls in rather than all of
// history.
//
// The result is snapped down to a fanout boundary. A block is sealed once and
// never recomputed, so sealing a partial range would freeze an incomplete
// summary; snapping is also what stops a permanent coverage hole appearing where
// rollups were switched on.
//
// It decodes the whole spool, and that is the cost of the frontier being a
// property of timestamps rather than of positions: there is no cheaper way to
// learn which records are inside the window. Measured at 31,288 records it is
// roughly 400ms per scan, which dominates the seal itself — internal/rollup
// reads the spool once and writes only the blocks a view would read, so its own
// share is small. Reducing it means giving the store a position-indexed answer
// to "which records are older than t", which it does not have today.
func sealableHead(events *store.Store, head uint64, window time.Duration) (uint64, error) {
	if head == 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-window)
	entries, err := events.Entries(0)
	if err != nil {
		return 0, err
	}

	// The first in-window record caps the frontier at the boundary below it, and
	// scanning continues so a later one cannot raise that cap back up.
	sealable := head
	for _, entry := range entries {
		if entry.Record.Timestamp.After(cutoff) {
			boundary := (entry.Position - 1) - (entry.Position-1)%rollup.Fanout
			sealable = min(sealable, boundary)
		}
	}
	return sealable - sealable%rollup.Fanout, nil
}

// rollupFailures counts seals that failed since this process started.
//
// A derived cache's failure must not fail the command that produced the records
// — `wake ingest` exiting non-zero over a rollup would report a failure that did
// not happen — but it must not vanish either: errcheck's check-blank is enabled
// in this repo precisely so a counting tool cannot quietly swallow an error.
//
// A counter rather than a return, and deliberately not a log line: this package
// writes no free text about local state (ADR-0007), and the number is what
// `doctor` can render. It is process-local because the next scan retries the
// same blocks, so a durable record of a transient failure would outlive its own
// meaning.
var rollupFailures atomic.Uint64

// noteRollupFailure records a failed seal and returns whether there was one.
func noteRollupFailure(err error) bool {
	if err == nil {
		return false
	}
	rollupFailures.Add(1)
	return true
}

// RollupFailures reports how many seals have failed in this process. It exists
// so a caller can surface the fact without this package deciding how.
func RollupFailures() uint64 { return rollupFailures.Load() }
