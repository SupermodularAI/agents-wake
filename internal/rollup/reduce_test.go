package rollup

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// entryAt builds one valid store entry. Only the dimensions the reduce reads are
// varied; everything else is whatever record.Validate accepts.
func entryAt(t *testing.T, position uint64, kind record.Kind, name string, outcome record.Outcome, ms int64, session string, at time.Time) store.Entry {
	t.Helper()
	out := outcome
	duration := ms
	return store.Entry{
		Position: position,
		Record: record.Record{
			SchemaVersion: record.SchemaVersion,
			EventID:       record.DeriveEventID("claude-code", record.Identifier(fmt.Sprintf("event-%d", position))),
			Timestamp:     at,
			Harness:       "claude-code",
			SessionID:     record.Identifier(session),
			Repo:          record.Hash("abcdef0123456789abcdef0123456789"),
			Kind:          kind,
			Name:          record.Identifier(name),
			Invoker:       record.InvokerModel,
			Outcome:       &out,
			DurationMS:    &duration,
		},
	}
}

// TestMergeEqualsDirectReduce is the property the whole tiered design rests on.
//
// A tier-2 block is sealed by merging the ten tier-1 blocks below it and never by
// re-reading records. That is only sound if merging produces exactly what
// reducing the same records directly would have produced. If it does not, every
// tier above the first reports numbers that no drill-down can reproduce — and
// because a sealed block is never recomputed, the error is permanent.
//
// This is also the test that would fail if anyone stored a percentile in a
// block: p95 of a union is not derivable from the p95 of its parts, so the two
// sides would diverge the moment the buckets did.
func TestMergeEqualsDirectReduce(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	kinds := []record.Kind{record.KindSkill, record.KindMCPTool, record.KindSubagent}
	names := []string{"alpha", "beta", "gamma", "delta"}
	outcomes := []record.Outcome{record.OutcomeOK, record.OutcomeError, record.OutcomeTimeout}

	// One hundred entries spread over every key combination, with latencies
	// deliberately crossing bucket boundaries.
	all := make([]store.Entry, 0, 100)
	for index := range uint64(100) {
		all = append(all, entryAt(t,
			index+1,
			kinds[index%uint64(len(kinds))],
			names[index%uint64(len(names))],
			outcomes[index%uint64(len(outcomes))],
			int64(index*37%1500),
			fmt.Sprintf("session-%d", index/7),
			base.Add(time.Duration(index)*time.Minute),
		))
	}

	direct := Reduce(2, 0, 100, all)

	children := make([]Block, 0, Fanout)
	for group := range Fanout {
		children = append(children, Reduce(1, group*Fanout, (group+1)*Fanout, all[group*Fanout:(group+1)*Fanout]))
	}
	merged := Merge(2, 0, 100, children)

	// Sessions is the one field that is documented as approximate above tier 1,
	// so it is compared separately below rather than being allowed to mask a
	// real difference in the aggregates.
	direct.Sessions, merged.Sessions = 0, 0

	directJSON, err := json.Marshal(direct)
	if err != nil {
		t.Fatalf("marshalling direct reduce: %v", err)
	}
	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		t.Fatalf("marshalling merged reduce: %v", err)
	}
	if string(directJSON) != string(mergedJSON) {
		t.Errorf("merge is not equivalent to a direct reduce\ndirect: %s\nmerged: %s", directJSON, mergedJSON)
	}
}

// TestMergeIsOrderIndependent asserts the other half of mergeability: the result
// must not depend on the order the children are combined in. Addition, min and
// max are all commutative, so this holds by construction — the test is what
// notices if someone later introduces a field that is not.
func TestMergeIsOrderIndependent(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	entries := make([]store.Entry, 0, 30)
	for index := range uint64(30) {
		entries = append(entries, entryAt(t, index+1, record.KindSkill, fmt.Sprintf("skill-%d", index%3),
			record.OutcomeOK, int64(index*11), "session-a", base.Add(time.Duration(index)*time.Second)))
	}
	first := Reduce(1, 0, 10, entries[0:10])
	second := Reduce(1, 10, 20, entries[10:20])
	third := Reduce(1, 20, 30, entries[20:30])

	forward := Merge(2, 0, 30, []Block{first, second, third})
	backward := Merge(2, 0, 30, []Block{third, second, first})

	forwardJSON, _ := json.Marshal(forward)
	backwardJSON, _ := json.Marshal(backward)
	if string(forwardJSON) != string(backwardJSON) {
		t.Errorf("merge depends on child order\nforward:  %s\nbackward: %s", forwardJSON, backwardJSON)
	}
}

// TestReduceIsDeterministic asserts a block's bytes are a function of its
// records alone. It runs the same reduce repeatedly because the hazard is Go's
// randomised map iteration: without the sort in sortedKeys this passes rarely
// and fails often, which is the worst possible failure mode for an on-disk
// format that is written once and never recomputed.
func TestReduceIsDeterministic(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	entries := make([]store.Entry, 0, 40)
	for index := range uint64(40) {
		entries = append(entries, entryAt(t, index+1, record.KindSkill, fmt.Sprintf("skill-%02d", index),
			record.OutcomeOK, int64(index), "session-a", base))
	}
	want, err := json.Marshal(Reduce(1, 0, 40, entries))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	for attempt := range 50 {
		got, err := json.Marshal(Reduce(1, 0, 40, entries))
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("reduce is not deterministic on attempt %d", attempt)
		}
	}
}

// TestReduceRetainsDrillDownID asserts the load-bearing property of the design:
// a summary is a queryable pointer set, not a dead string. A count answers
// "was this ever used"; the retained id is what makes any positive count
// drillable back to an exact invocation in the spool.
func TestReduceRetainsDrillDownID(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// Deliberately out of position order, and with the earliest timestamp last,
	// so a reduce that trusted arrival order would retain the wrong id.
	entries := []store.Entry{
		entryAt(t, 3, record.KindSkill, "alpha", record.OutcomeOK, 10, "s", base.Add(2*time.Hour)),
		entryAt(t, 1, record.KindSkill, "alpha", record.OutcomeOK, 10, "s", base.Add(3*time.Hour)),
		entryAt(t, 2, record.KindSkill, "alpha", record.OutcomeOK, 10, "s", base),
	}
	block := Reduce(1, 0, 10, entries)
	if len(block.Keys) != 1 {
		t.Fatalf("want 1 key, got %d", len(block.Keys))
	}
	got := block.Keys[0].Aggregate
	if got.Invocations != 3 {
		t.Errorf("invocations = %d, want 3", got.Invocations)
	}
	// Position 2 carries the earliest timestamp, so its id is the one retained.
	want := entries[2].Record.EventID
	if got.FirstEventID != want {
		t.Errorf("retained id = %q, want the earliest by timestamp %q", got.FirstEventID, want)
	}
}

// TestReduceIDTieBreakIsDeterministic covers the common case the tie break
// exists for: records derived from one transcript routinely share a
// millisecond, and without a second ordering term the retained id would depend
// on map iteration.
func TestReduceIDTieBreakIsDeterministic(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	entries := []store.Entry{
		entryAt(t, 1, record.KindSkill, "alpha", record.OutcomeOK, 1, "s", at),
		entryAt(t, 2, record.KindSkill, "alpha", record.OutcomeOK, 1, "s", at),
		entryAt(t, 3, record.KindSkill, "alpha", record.OutcomeOK, 1, "s", at),
	}
	want := Reduce(1, 0, 10, entries).Keys[0].Aggregate.FirstEventID
	for range 50 {
		if got := Reduce(1, 0, 10, entries).Keys[0].Aggregate.FirstEventID; got != want {
			t.Fatalf("tie break is not deterministic: got %q, want %q", got, want)
		}
	}
}

// TestReduceSeparatesOutcomes asserts outcome is part of the key. A rollup that
// collapsed outcomes would answer "how often was this used" and lose "how often
// did it fail", which is the distinction internal/metrics exists to keep.
func TestReduceSeparatesOutcomes(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	entries := []store.Entry{
		entryAt(t, 1, record.KindSkill, "alpha", record.OutcomeOK, 5, "s", base),
		entryAt(t, 2, record.KindSkill, "alpha", record.OutcomeError, 5, "s", base),
		entryAt(t, 3, record.KindSkill, "alpha", record.OutcomeOK, 5, "s", base),
	}
	block := Reduce(1, 0, 10, entries)
	if len(block.Keys) != 2 {
		t.Fatalf("want 2 keys (ok and error), got %d", len(block.Keys))
	}
	for _, entry := range block.Keys {
		want := uint64(2)
		if entry.Key.Outcome == string(record.OutcomeError) {
			want = 1
		}
		if entry.Aggregate.Invocations != want {
			t.Errorf("%s: invocations = %d, want %d", entry.Key.Outcome, entry.Aggregate.Invocations, want)
		}
	}
}

// TestReduceExcludesUnreportedDurations asserts a nil duration contributes to
// the invocation count and to nothing else. "Not reported" is not an observation
// of zero (ADR-0005 applied to counts), and a histogram that counted it would
// make every latency answer wrong in the direction of fast.
func TestReduceExcludesUnreportedDurations(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	withDuration := entryAt(t, 1, record.KindSkill, "alpha", record.OutcomeOK, 500, "s", base)
	without := entryAt(t, 2, record.KindSkill, "alpha", record.OutcomeOK, 0, "s", base)
	without.Record.DurationMS = nil

	block := Reduce(1, 0, 10, []store.Entry{withDuration, without})
	got := block.Keys[0].Aggregate
	if got.Invocations != 2 {
		t.Errorf("invocations = %d, want 2", got.Invocations)
	}
	if got.Latency.Count != 1 {
		t.Errorf("latency observations = %d, want 1 — an unreported duration must not be bucketed", got.Latency.Count)
	}
}

// TestMergeSessionsNeverDoubleCounts covers the field that does not merge
// exactly. A session spanning two blocks appears in both, so summing would
// report more sessions than ever existed; the maximum is the honest bound.
func TestMergeSessionsNeverDoubleCounts(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// One session, spanning both blocks.
	left := Reduce(1, 0, 10, []store.Entry{entryAt(t, 1, record.KindSkill, "a", record.OutcomeOK, 1, "same", base)})
	right := Reduce(1, 10, 20, []store.Entry{entryAt(t, 11, record.KindSkill, "a", record.OutcomeOK, 1, "same", base)})
	merged := Merge(2, 0, 20, []Block{left, right})
	if merged.Sessions != 1 {
		t.Errorf("sessions = %d, want 1: one session spanning two blocks must not be counted twice", merged.Sessions)
	}
}
