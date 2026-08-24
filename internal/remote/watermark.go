//go:build remote

package remote

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
	"github.com/SupermodularAI/agents-wake/internal/config"
)

const (
	// deliveryStateFileName is the whole of this package's on-disk footprint.
	// The extension matches the encoding, as every other name in this project
	// does.
	deliveryStateFileName = "remote-delivery.json"

	// deliveryStateVersion is stamped on every write. A future format read as
	// this one would report a position that means something else, and a position
	// that means something else is either a re-send of everything or a permanent
	// skip. An unrecognised version is refused instead — the rule health.json
	// and remote-auth.json both follow.
	deliveryStateVersion = 1

	// deliveryStateFileMode is the mode the file is written with. It holds no
	// path, no label and no credential — a position and a time — but it is state
	// about this user's machine and the rest of the local layout is 0600.
	deliveryStateFileMode = fs.FileMode(0o600)
)

// deliveryState is the whole file: how far delivery got, and when it last tried.
//
// Position is a store position and never a timestamp. ADR-0004 derives every
// event id from its source event, so the store appends each event exactly once
// and position is monotonic even when timestamps are not. A timestamp watermark
// would be wrong for exactly that reason: re-scanning discovers older events
// later — a transcript imported today can carry an event from last week — and a
// timestamp cursor would skip every one of them permanently. This is the same
// reasoning ADR-0004 used to reject timestamp-based deduplication, and ADR-0018
// settled it for delivery.
//
// LastFlush is not a cursor. It is never consulted when selecting records: it
// gates remote.min_interval, and it is what `remote status` and `doctor` print
// as the last flush time. TestLastFlushIsNeverACursor asserts the separation
// mechanically, because the two fields living in one struct is exactly the
// arrangement in which someone later reaches for the wrong one.
//
// It records when a flush was *attempted*, not when one succeeded — Flush stamps
// it on the failure path too. The alternative, taking the watermark file's mtime
// as the signal, would not advance on a failed or empty run, so a dead endpoint
// would be retried on every single trigger, which is precisely what the minimum
// interval exists to prevent.
type deliveryState struct {
	Version   int       `json:"version"`
	Position  uint64    `json:"position"`
	LastFlush time.Time `json:"last_flush"`
}

// deliveryStatePath is where the state lives: under the data root, because it is
// derived and non-precious. ADR-0015 makes the spool safe to delete, and this
// position is meaningless without the spool it indexes, so it has to die with it
// rather than outlive it and describe records that are gone (ADR-0014).
//
// Composed here rather than added to config.Paths, for the reason
// remoteAuthPath gives: a field on Paths would be a build-tagged field on an
// untagged struct, which is how the default build ends up disclosing a file it
// can never have.
func deliveryStatePath(p config.Paths) string {
	return filepath.Join(p.DataDir, deliveryStateFileName)
}

// readDeliveryState reports how far delivery got, and returns no error on
// purpose.
//
// A missing file, an unparseable one, and one from a version this build does not
// read all mean the same thing here: "delivered through nothing". That is the
// conservative direction for a cursor, and the asymmetry is the whole argument.
// Failing backward costs a re-send, which is free — DG-63 derives span_id from
// the deterministic event_id, so the receiver collapses a duplicate on
// (trace_id, span_id) and at-least-once delivery is safe by construction
// (ADR-0018, ADR-0027). Failing forward would skip records permanently, and
// nothing downstream would ever notice.
//
// It is the same argument the rebuild self-heal in Flush makes, and it is the
// reason neither path clamps.
func readDeliveryState(path string) deliveryState {
	raw, err := os.ReadFile(path)
	if err != nil {
		return deliveryState{}
	}
	var stored deliveryState
	if err := json.Unmarshal(raw, &stored); err != nil {
		return deliveryState{}
	}
	if stored.Version != deliveryStateVersion {
		return deliveryState{}
	}
	return stored
}

// writeDeliveryState publishes the state whole, so a reader sees the old
// position or the new one and never a truncated file that reads as zero and
// re-sends the spool.
//
// It stamps the version itself rather than trusting the caller's field: a caller
// that forgot would write a file this build then refuses to read back, which
// presents as delivery silently restarting from the beginning on every flush.
func writeDeliveryState(path string, s deliveryState) error {
	s.Version = deliveryStateVersion
	data, err := json.Marshal(s)
	if err != nil {
		// Three scalars, so this cannot fail — and the message still names no
		// field, because a marshal error embeds the value it choked on.
		return err
	}
	return atomicfile.Publish(path, append(data, '\n'), deliveryStateFileMode)
}
