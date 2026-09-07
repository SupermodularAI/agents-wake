package activation

import (
	"path/filepath"
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
func rollupPass(paths config.Paths, events *store.Store, pass func(string, *store.Store, uint64) (rollup.SealResult, error)) error {
	window, enabled := rollupWindow(paths)
	if !enabled {
		return nil
	}
	head, err := events.Head()
	if err != nil {
		return err
	}
	sealable, err := sealableHead(events, head, window)
	if err != nil {
		return err
	}
	// Called even when nothing is sealable, so enabling on an empty or entirely
	// recent store still records the floor. Deferring that to the first scan
	// with sealable history would put the floor above everything appended in
	// between, and nothing below it is ever sealed.
	_, err = pass(paths.DataDir, events, sealable)
	return err
}

// sealableHead is the highest position whose record is older than the window,
// snapped down to a fanout boundary.
//
// Two properties come from the snap, and both matter. A block is sealed once and
// never recomputed, so sealing a partial range would freeze an incomplete
// summary forever; and building forward from a boundary rather than from
// wherever the window happens to fall is what stops a permanent coverage hole
// appearing at the moment rollups were switched on.
//
// Records are scanned in position order and the first one inside the window ends
// the sealable region. Timestamps are not monotonic in position — a transcript
// imported today can carry an event from last week (internal/remote/watermark.go
// makes the same point about cursors) — so this is deliberately the first
// crossing and not the last: it never seals a block containing an event the
// window should still have excluded.
func sealableHead(events *store.Store, head uint64, window time.Duration) (uint64, error) {
	if head == 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-window)
	entries, err := events.Entries(0)
	if err != nil {
		return 0, err
	}
	sealable := head
	for _, entry := range entries {
		if entry.Record.Timestamp.After(cutoff) {
			sealable = entry.Position - 1
			break
		}
	}
	return sealable - sealable%rollup.Fanout, nil
}
