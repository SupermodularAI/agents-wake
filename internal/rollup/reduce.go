package rollup

import (
	"cmp"
	"slices"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// UnixMilli is a timestamp as milliseconds since the Unix epoch.
//
// A block stores this rather than a time.Time because time.Time marshals with a
// monotonic reading and a location, and two blocks reduced from identical
// records in different timezones would then differ byte-for-byte. An integer
// makes "sealing twice writes nothing" a property of the data rather than of
// the process that happened to write it.
type UnixMilli int64

// Time converts back for display.
func (u UnixMilli) Time() time.Time { return time.UnixMilli(int64(u)) }

// Reduce summarises a range of store entries into a sealed tier-1 block.
//
// It is deterministic and total: the same entries always produce the same block,
// there is no model call, no network access and no clock read. Every value in
// the result is a count, a bucketed count, a timestamp taken from a record, or
// an event id copied from one.
//
// Records are not required to arrive sorted. The reduce keys on identity and
// takes minima and maxima, so arrival order cannot reach the output — which is
// what lets a caller pass entries straight from store.Entries without paying
// for a sort it does not need.
func Reduce(tier uint, start, end uint64, entries []store.Entry) Block {
	aggregates := make(map[Key]*Aggregate, len(entries))
	sessions := make(map[record.Identifier]struct{})

	for _, entry := range entries {
		r := entry.Record
		sessions[r.SessionID] = struct{}{}

		key := Key{Kind: r.Kind, Name: r.Name}
		if r.Outcome != nil {
			key.Outcome = string(*r.Outcome)
		}

		stamp := UnixMilli(r.Timestamp.UnixMilli())
		aggregate, seen := aggregates[key]
		if !seen {
			aggregate = &Aggregate{FirstSeen: stamp, LastSeen: stamp, FirstEventID: r.EventID}
			aggregates[key] = aggregate
		}
		aggregate.Invocations++

		// The retained id is the earliest by (timestamp, event_id), and the tie
		// break on the id is what makes it deterministic: two records sharing a
		// millisecond are common — a session's records are derived from one
		// transcript — and without the second term the winner would depend on
		// map iteration order.
		if stamp < aggregate.FirstSeen || (stamp == aggregate.FirstSeen && r.EventID < aggregate.FirstEventID) {
			aggregate.FirstEventID = r.EventID
		}
		aggregate.FirstSeen = min(aggregate.FirstSeen, stamp)
		aggregate.LastSeen = max(aggregate.LastSeen, stamp)

		// A nil duration is the harness having reported none, which is not an
		// observation of zero (ADR-0005 applied to counts). It contributes to
		// Invocations and to nothing else, so the histogram's Count is its own
		// denominator and a reader can tell "slow" from "unreported".
		//
		// A negative one is refused by Observe and counted here. It is
		// unreachable for a record that passed record.Validate, so the counter
		// exists to make the impossible visible rather than to be non-zero: a
		// block reporting a dropped observation is a bug in an adapter, and one
		// silently absorbed into bucket 0 would present as a very fast call.
		if r.DurationMS != nil {
			if err := aggregate.Latency.Observe(*r.DurationMS); err != nil {
				aggregate.DroppedObservations++
			}
		}
	}

	return Block{
		BlockVersion:  BlockVersion,
		SchemaVersion: record.SchemaVersion,
		Tier:          tier,
		Start:         start,
		End:           end,
		Sessions:      uint64(len(sessions)),
		Keys:          sortedKeys(aggregates),
	}
}

// Merge seals a higher tier from the blocks below it, without reading a single
// record.
//
// This is the operation that makes the design incremental, and its correctness
// is one property: merging blocks must produce exactly what reducing their
// records directly would have produced. That holds because every field is
// combined by an operation that is associative and commutative — addition for
// counts and buckets, min and max for the bounds — so neither the order of the
// children nor the boundaries between them can reach the result.
//
// It is the reason no percentile is stored. Addition is exact on bucket counts
// and undefined on percentile values.
func Merge(tier uint, start, end uint64, blocks []Block) Block {
	aggregates := make(map[Key]*Aggregate)
	var sessions uint64

	for _, block := range blocks {
		// Sessions is the maximum and not the sum. A session spanning two child
		// blocks appears in both, so adding would double it; the maximum is the
		// truthful lower bound, and it is why Block.Sessions is documented as
		// approximate above tier 1. Exactness here would mean retaining session
		// ids, which would make a block grow with sessions instead of with
		// keys.
		sessions = max(sessions, block.Sessions)

		for _, entry := range block.Keys {
			aggregate, seen := aggregates[entry.Key]
			if !seen {
				clone := entry.Aggregate
				clone.Latency = Histogram{}
				clone.Latency.Merge(entry.Aggregate.Latency)
				aggregates[entry.Key] = &clone
				continue
			}
			// Same rule as the reduce, so a merged block retains the id the
			// reduce would have retained over the union of the records.
			if entry.Aggregate.FirstSeen < aggregate.FirstSeen ||
				(entry.Aggregate.FirstSeen == aggregate.FirstSeen && entry.Aggregate.FirstEventID < aggregate.FirstEventID) {
				aggregate.FirstEventID = entry.Aggregate.FirstEventID
			}
			aggregate.Invocations += entry.Aggregate.Invocations
			aggregate.DroppedObservations += entry.Aggregate.DroppedObservations
			aggregate.FirstSeen = min(aggregate.FirstSeen, entry.Aggregate.FirstSeen)
			aggregate.LastSeen = max(aggregate.LastSeen, entry.Aggregate.LastSeen)
			aggregate.Latency.Merge(entry.Aggregate.Latency)
		}
	}

	return Block{
		BlockVersion:  BlockVersion,
		SchemaVersion: record.SchemaVersion,
		Tier:          tier,
		Start:         start,
		End:           end,
		Sessions:      sessions,
		Keys:          sortedKeys(aggregates),
	}
}

// sortedKeys flattens the reduce into the stable order a block serialises in.
//
// The sort is what makes a block's bytes a function of its records alone. Go
// randomises map iteration on purpose, so without this an identical reduce would
// serialise differently on every run, and both "sealing twice writes nothing"
// and the byte-identical merge property would be untestable.
func sortedKeys(aggregates map[Key]*Aggregate) []KeyAggregate {
	out := make([]KeyAggregate, 0, len(aggregates))
	for key, aggregate := range aggregates {
		out = append(out, KeyAggregate{Key: key, Aggregate: *aggregate})
	}
	slices.SortFunc(out, func(a, b KeyAggregate) int { return compareKeys(a.Key, b.Key) })
	return out
}

// compareKeys is the canonical order a block's keys are stored in. It is one
// function because two call sites depend on it agreeing exactly: sortedKeys
// writes the order and ReadBlock verifies it, and a block whose order was
// produced by a different rule merges into different bytes.
func compareKeys(a, b Key) int {
	return cmp.Or(
		cmp.Compare(a.Kind, b.Kind),
		cmp.Compare(a.Name, b.Name),
		cmp.Compare(a.Outcome, b.Outcome),
	)
}
