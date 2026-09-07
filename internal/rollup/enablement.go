package rollup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// enablementFileName records where sealing began. It sits inside the rollup
// directory so that removing the directory forgets it too: the floor describes
// blocks, and a floor that outlived them would leave a permanent hole no reseal
// could fill.
const enablementFileName = "enabled.json"

// Enablement is the position sealing starts from, and the version stamps that
// make it meaningful.
//
// Why persist it at all: rollups build forward from the moment they were
// switched on, so a store with history predating that is left unsealed unless
// the user asks for it. Recomputing the floor on each run would defeat that —
// there is nothing in the spool that says when the key was set — so it is
// written once and read thereafter.
//
// Snapped to a fanout boundary when it is written. That is what stops a
// permanent coverage hole: a floor at position 34 would leave [30,40) neither
// sealable as a whole block nor coverable by a finer one, and because a block is
// sealed once and never recomputed the hole would never close.
type Enablement struct {
	BlockVersion  uint `json:"block_version"`
	SchemaVersion uint `json:"schema_version"`
	// Floor is the lowest position sealing may cover. Positions at or below it
	// are history from before rollups were enabled.
	Floor uint64 `json:"floor"`
	// Backfilled records that the user asked for history before the floor to be
	// sealed after all. It is kept so that a later run does not undo the
	// backfill by pruning back to the floor.
	Backfilled bool `json:"backfilled"`
}

// enablementPath is where the mark lives.
func enablementPath(dir string) string { return filepath.Join(dir, enablementFileName) }

// ReadEnablement reports the recorded floor, and whether one exists.
//
// A missing file means sealing has never run. A file this build cannot read is
// treated the same way, for the reason readDeliveryState gives about a cursor:
// the conservative direction is to re-establish the mark rather than to trust a
// number nothing vouches for. Re-establishing it costs a fresh floor at the
// current head, which leaves older history unsealed — the same state a first run
// is in, and the state backfill exists to resolve.
func ReadEnablement(dir string) (Enablement, bool) {
	raw, err := os.ReadFile(enablementPath(dir))
	if err != nil {
		return Enablement{}, false
	}
	var stored Enablement
	if err := json.Unmarshal(raw, &stored); err != nil {
		return Enablement{}, false
	}
	if stored.BlockVersion != BlockVersion || stored.SchemaVersion != record.SchemaVersion {
		return Enablement{}, false
	}
	return stored, true
}

// WriteEnablement records the floor, snapping it down to a fanout boundary.
//
// It stamps both versions itself rather than trusting the caller, the way
// writeDeliveryState does: a caller that forgot one would write a file this
// build then refuses, which presents as the floor silently resetting to the head
// on every run and no history ever being sealed.
func WriteEnablement(dir string, mark Enablement) error {
	if err := os.MkdirAll(dir, blockDirMode); err != nil {
		return fmt.Errorf("creating rollup directory: %w", err)
	}
	mark.BlockVersion = BlockVersion
	mark.SchemaVersion = record.SchemaVersion
	mark.Floor = snapToFanout(mark.Floor)
	data, err := json.Marshal(mark)
	if err != nil {
		return fmt.Errorf("encoding the enablement mark: %w", err)
	}
	return atomicfile.Publish(enablementPath(dir), append(data, '\n'), blockFileMode)
}

// isEnablementFile reports whether a directory entry is the mark rather than a
// block, so a prune leaves it alone.
func isEnablementFile(name string) bool { return name == enablementFileName }
