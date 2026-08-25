package remote

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/record"
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
	// and internal/config's own versioned stores both follow.
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
//
// SchemaVersion is the record schema the spool held when Position was taken, and
// it is what makes Position mean something. Position counts the records the spool
// holds; a record schema bump makes every record the spool held unreadable, so the
// scan discards the spool and re-derives it (ADR-0007, ADR-0015) and every position
// over the old one now indexes a record that is gone. The rebuild self-heal in
// Flush cannot catch that on its own: it fires on a position past head, and a
// re-derived spool is about as long as the one it replaced, so head passes the
// stale position again and the records below it are skipped permanently. Stamping
// the schema version is what turns that into a re-send, which is free.
//
// It is half of that defence and not the whole of it, because it is spent once: the
// first flush after the bump clears the position and stamps the new version, and the
// rebuild that renumbers the spool can come later — the scan a hook fires leaves a
// stale spool for the scan the user asks for (ADR-0025), and a flush is spawned after
// both. The other half is in Flush and is a property of the spool rather than of this
// file: a position is only ever taken from, or recorded over, a spool this build reads
// whole. The two are complementary. This field covers a rebuild that happened before
// any flush saw the new schema; the spool check covers a rebuild that happens after
// one already did.
type deliveryState struct {
	Version       int       `json:"version"`
	SchemaVersion uint      `json:"schema_version"`
	Position      uint64    `json:"position"`
	LastFlush     time.Time `json:"last_flush"`
}

// deliveryStatePath is where the state lives: under the data root, because it is
// derived and non-precious. ADR-0015 makes the spool safe to delete, and this
// position is meaningless without the spool it indexes, so it has to die with it
// rather than outlive it and describe records that are gone (ADR-0014).
//
// Composed here rather than added to config.Paths, for the reason
// remoteAuthPath gives: Paths is the list `init` discloses under ADR-0010, and a
// file that exists only once delivery has actually run is not a path to
// disclose.
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
	if stored.SchemaVersion != record.SchemaVersion {
		// Only the position is forgotten. LastFlush is not a cursor — it gates
		// remote.min_interval — so a schema bump must not turn into an unthrottled
		// flush on top of a full re-send. A file written before this field existed
		// reads as version 0 and lands here, which is the correct answer for it:
		// nothing vouches for what its position counted.
		stored.Position = 0
	}
	return stored
}

// writeDeliveryState publishes the state whole, so a reader sees the old
// position or the new one and never a truncated file that reads as zero and
// re-sends the spool.
//
// It stamps both versions itself rather than trusting the caller's fields: a caller
// that forgot either one would write a file this build then refuses, or whose
// position this build then discards, which presents as delivery silently restarting
// from the beginning on every flush.
func writeDeliveryState(path string, s deliveryState) error {
	s.Version = deliveryStateVersion
	s.SchemaVersion = record.SchemaVersion
	data, err := json.Marshal(s)
	if err != nil {
		// Four scalars, so this cannot fail — and the message still names no
		// field, because a marshal error embeds the value it choked on.
		return err
	}
	return atomicfile.Publish(path, append(data, '\n'), deliveryStateFileMode)
}
