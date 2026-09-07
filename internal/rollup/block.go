// Package rollup keeps a bounded, queryable summary of the local event store.
//
// The store's spool grows linearly with history and every reader that answers a
// question about it decodes the whole file (internal/report, internal/ui,
// internal/inventory). This package answers the same questions from sealed
// blocks whose count is logarithmic in history, and it does so without reading
// or modifying the spool's older bytes.
//
// The mechanism is a tiered rollup of immutable blocks, adopted from headlong's
// sealed-block design with one deliberate change: headlong calls a model to
// summarise each block, which is right for prose memory and wrong for
// telemetry. The reduce here is arithmetic — counts and latency histograms — so
// it is deterministic, needs no network, and produces the same block for the
// same records on every machine. internal/remote/otlp.go declares its encoder
// pure and free of model calls; this package holds the same property.
//
// # What is bounded
//
// The assembled view and the cost of reading it. Not disk.
//
// The spool is never modified and never truncated here. store.Entries assigns
// Position by counting decoded lines from the start of the file, so a position
// is an ordinal rather than a stored field, and the remote delivery watermark is
// one of those positions (internal/remote/watermark.go: "Position counts the
// records the spool held"). Truncating the spool would renumber every position
// and leave that cursor indexing a different record, with nothing downstream
// able to notice. Pruning needs a durable record identity that does not exist
// today, so it is out of scope by design rather than merely unimplemented.
//
// # Blocks are sealed once
//
// A block covers a half-open range of store positions and is written to a file
// named by that range. If the file exists, the work is skipped — so the cost of
// a seal is only ever the new frontier, and incrementality is a property of the
// naming rather than of bookkeeping that could drift.
//
// Tier k covers Fanout^k positions. A tier-2 block is sealed by merging the ten
// tier-1 blocks below it, never by re-reading records, which is what keeps the
// total work linear in the number of events and the re-read count at zero.
package rollup

import (
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// Fanout is how many blocks of tier k make one block of tier k+1, and so how
// many positions a tier-1 block covers.
//
// Ten, from the upstream design. It is not tunable: the value is baked into
// every sealed block's name and range, so changing it would not migrate blocks,
// it would silently reinterpret them. A build that wanted a different fanout
// would have to invalidate the whole directory, which is what BlockVersion is
// for.
const Fanout uint64 = 10

// MaxTier is the highest tier this build seals. Tier 5 covers 100,000 positions
// — an order of magnitude beyond the 31,288 records measured on the first
// machine to install wake — and the cap exists so a corrupt or adversarial
// position cannot drive an unbounded tier loop.
const MaxTier = 5

// BlockVersion is the on-disk contract for a sealed block, and it is this
// package's own number rather than record.SchemaVersion.
//
// The two are stamped for different reasons and change independently. A block
// carries record.SchemaVersion because its contents are derived from records of
// that version and mean nothing across a bump. It carries BlockVersion because
// the reduce itself — which keys exist, how latency is bucketed, which ids are
// retained — can change while the record contract does not. Either one being
// foreign makes a block unreadable, and the answer to both is the same: refuse
// it and seal again from source, which is free because the reduce is arithmetic.
const BlockVersion = 1

// Key is the grain the reduce aggregates to.
//
// Outcome is part of the key rather than a counter inside it, so "this skill
// failed twice and succeeded four hundred times" survives the rollup as two
// rows. It is a string and not a *record.Outcome because a block is JSON on
// disk: the empty string is the harness having reported no outcome, which is
// what a nil Outcome means on a record, and a pointer here would add a
// nil-versus-empty distinction that no reader could act on.
type Key struct {
	Kind    record.Kind       `json:"kind"`
	Name    record.Identifier `json:"name"`
	Outcome string            `json:"outcome"`
}

// Aggregate is what one Key reduced to over a block's range.
//
// Every field is a count or a set of counts, which is the whole point: counts
// merge by addition, so a higher tier is sealed from the tiers below it without
// ever revisiting a record. There is no mean, no rate and no percentile stored
// anywhere in a block — those are derived at view-assembly time from these
// counts, because a derived value cannot be merged back.
type Aggregate struct {
	// Invocations is how many records carried this key.
	Invocations uint64 `json:"invocations"`

	// Latency is the distribution of duration_ms, bucketed by LatencyBounds.
	// Its length is always len(LatencyBounds)+1.
	Latency Histogram `json:"latency"`

	// FirstEventID is the earliest record's event id for this key, ordered by
	// (timestamp, event_id), and it is what keeps a summary queryable rather
	// than a dead string. A zero Invocations answers "this primitive was never
	// used"; any positive count drills from the summary down to an exact
	// invocation in the spool.
	//
	// One id per key, not a sample of many: it bounds a block by how many
	// distinct keys it saw rather than by how many events it covered, which is
	// what keeps the higher tiers small. Ten thousand invocations of one skill
	// roll up to one row carrying one id.
	FirstEventID record.Hash `json:"first_event_id"`

	// FirstSeen and LastSeen bound this key's activity in the range. Both are
	// kept because a merge takes the min of one and the max of the other, and
	// neither can be recovered from the other after the fact.
	FirstSeen UnixMilli `json:"first_seen"`
	LastSeen  UnixMilli `json:"last_seen"`

	// DroppedObservations counts durations the histogram refused. It is always
	// zero for records that passed record.Validate, which refuses a negative
	// duration, so a non-zero value here means an adapter produced something
	// impossible. Recorded rather than discarded because the alternative is a
	// bad value quietly landing in the fastest bucket and reading as a fast
	// call — omitempty so the normal case adds nothing to a block.
	DroppedObservations uint64 `json:"dropped_observations,omitempty"`
}

// Block is one sealed, immutable summary of a half-open position range.
//
// Start and End are store positions, which renumber whenever the spool is
// rebuilt. That is safe, and it is worth saying why, because "the file name is
// the identity" invites the opposite assumption: a range is not durable
// identity, and nothing here relies on it being so. Blocks are invalidated as a
// set — store.Discard removes the whole directory, and a foreign version stamp
// is refused — so a block never outlives the numbering it was sealed under.
type Block struct {
	BlockVersion  uint   `json:"block_version"`
	SchemaVersion uint   `json:"schema_version"`
	Tier          uint   `json:"tier"`
	Start         uint64 `json:"start"`
	End           uint64 `json:"end"`

	// Sessions is the number of distinct session ids seen in the range.
	//
	// It is a count and not a set, so it does not merge exactly: a session
	// spanning two blocks is counted in both, and a merge that added the two
	// would double it. Merge takes the maximum instead, which is the honest
	// bound — never fewer sessions than the busiest child saw — and the reason
	// the field is documented as approximate at tiers above one. Retaining the
	// ids themselves would make it exact and would make a block grow with
	// sessions rather than with keys, which is the growth this package exists
	// to stop.
	Sessions uint64 `json:"sessions"`

	// Keys is the reduce, sorted by (kind, name, outcome).
	//
	// A slice and not a map: a block is compared byte-for-byte in tests and a
	// Go map's iteration order is deliberately random, so a map would make an
	// identical reduce serialise differently on every run and destroy the
	// property that sealing twice is a no-op.
	Keys []KeyAggregate `json:"keys"`
}

// KeyAggregate pairs a Key with its Aggregate for serialisation. The two are
// separate types because Key is also a map key during the reduce, where an
// Aggregate cannot go.
type KeyAggregate struct {
	Key       Key       `json:"key"`
	Aggregate Aggregate `json:"aggregate"`
}
