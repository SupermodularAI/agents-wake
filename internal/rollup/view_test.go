package rollup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestViewBudgetIsAbsolute asserts the cap does not scale with history, which is
// the whole reason this package exists. The upstream design records finding that
// a view sized as a fraction of what is available made the cost of reading
// history grow with the history — so the ceiling is a constant, and a view over
// a hundred thousand records must read no more blocks than one over a thousand.
func TestViewBudgetIsAbsolute(t *testing.T) {
	small, eventsSmall := seededStore(t, 1_000)
	if _, err := Backfill(small, eventsSmall, 1_000); err != nil {
		t.Fatalf("sealing the small store: %v", err)
	}
	smallView, err := Assemble(small, eventsSmall, 1_000)
	if err != nil {
		t.Fatalf("assembling the small view: %v", err)
	}

	large, eventsLarge := seededStore(t, 12_000)
	if _, sealErr := Backfill(large, eventsLarge, 12_000); sealErr != nil {
		t.Fatalf("sealing the large store: %v", sealErr)
	}
	largeView, err := Assemble(large, eventsLarge, 12_000)
	if err != nil {
		t.Fatalf("assembling the large view: %v", err)
	}

	if len(largeView.Blocks) > MaxViewBlocks {
		t.Errorf("a view over 12,000 records read %d blocks, above the %d ceiling", len(largeView.Blocks), MaxViewBlocks)
	}
	if len(smallView.Blocks) > MaxViewBlocks {
		t.Errorf("a view over 1,000 records read %d blocks, above the %d ceiling", len(smallView.Blocks), MaxViewBlocks)
	}
	if len(largeView.Verbatim) > MaxVerbatimEntries+int(Fanout) {
		t.Errorf("verbatim tail is %d records, want at most about %d", len(largeView.Verbatim), MaxVerbatimEntries)
	}

	// Twelve times the history must not cost twelve times the blocks. This is
	// the statement that the growth is logarithmic rather than linear.
	if len(largeView.Blocks) > 2*len(smallView.Blocks)+int(Fanout) {
		t.Errorf("blocks grew from %d to %d for 12x the history, which is not sublinear",
			len(smallView.Blocks), len(largeView.Blocks))
	}
}

// TestViewCoversWithoutOverlap asserts a record is never both summarised and
// verbatim. A reader that added the two would double count it, and the blocks
// carry no ids for most members so the overlap would be undetectable downstream.
func TestViewCoversWithoutOverlap(t *testing.T) {
	dataDir, events := seededStore(t, 1_000)
	if _, err := Backfill(dataDir, events, 1_000); err != nil {
		t.Fatalf("seal: %v", err)
	}
	view, err := Assemble(dataDir, events, 1_000)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(view.Blocks) == 0 {
		t.Fatal("view read no blocks")
	}
	for index, block := range view.Blocks {
		if block.End > view.CoveredTo {
			t.Errorf("block %d ends at %d, past the verbatim boundary %d", index, block.End, view.CoveredTo)
		}
		if index > 0 && block.Start < view.Blocks[index-1].End {
			t.Errorf("block %d starts at %d, overlapping the previous block's end %d",
				index, block.Start, view.Blocks[index-1].End)
		}
	}
	for _, entry := range view.Verbatim {
		if entry.Position <= view.CoveredTo {
			t.Errorf("verbatim record at position %d is at or below the covered boundary %d",
				entry.Position, view.CoveredTo)
		}
	}
}

// TestViewIsCoarseToFine asserts the blocks arrive in ascending position order,
// which is the order a reader consumes them in.
func TestViewIsCoarseToFine(t *testing.T) {
	dataDir, events := seededStore(t, 1_000)
	if _, err := Backfill(dataDir, events, 1_000); err != nil {
		t.Fatalf("seal: %v", err)
	}
	view, err := Assemble(dataDir, events, 1_000)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	for index := 1; index < len(view.Blocks); index++ {
		if view.Blocks[index].Start < view.Blocks[index-1].Start {
			t.Errorf("block %d starts before block %d", index, index-1)
		}
	}
}

// TestViewWithNoBlocksIsVerbatimOnly asserts a store that has never been sealed
// still answers, from records alone. Rollups are opt-in, so this is the state
// of every machine that has not set store.rollup_after — the default must not
// depend on a directory that does not exist.
func TestViewWithNoBlocksIsVerbatimOnly(t *testing.T) {
	dataDir, events := seededStore(t, 50)
	view, err := Assemble(dataDir, events, 50)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(view.Blocks) != 0 {
		t.Errorf("view read %d blocks from an unsealed store, want 0", len(view.Blocks))
	}
	if len(view.Verbatim) != 50 {
		t.Errorf("verbatim = %d records, want all 50", len(view.Verbatim))
	}
	if view.Truncated {
		t.Error("a view covering everything verbatim reported Truncated")
	}
}

// TestViewSummaryCountsEveryRecord asserts the summary of a view accounts for
// all of it — blocks and verbatim tail combined, through the same merge a tier
// is sealed with. It is what makes the boundary between summarised and exact
// history invisible to a reader.
func TestViewSummaryCountsEveryRecord(t *testing.T) {
	const total = 1_000
	dataDir, events := seededStore(t, total)
	if _, err := Backfill(dataDir, events, total); err != nil {
		t.Fatalf("seal: %v", err)
	}
	view, err := Assemble(dataDir, events, total)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if view.Truncated {
		t.Fatal("the view was truncated, so this assertion would not be about full coverage")
	}
	summary := view.Summary()
	var invocations uint64
	for _, entry := range summary.Keys {
		invocations += entry.Aggregate.Invocations
	}
	if invocations != total {
		t.Errorf("summary counted %d invocations across %d keys, want %d",
			invocations, len(summary.Keys), total)
	}
}

// TestViewSummaryMatchesDirectReduce asserts a bounded view over full coverage
// answers exactly what reading every record would have. This is the claim that
// makes the rollup usable in place of the spool rather than merely cheaper than
// it.
func TestViewSummaryMatchesDirectReduce(t *testing.T) {
	const total = 1_000
	dataDir, events := seededStore(t, total)
	if _, err := Backfill(dataDir, events, total); err != nil {
		t.Fatalf("seal: %v", err)
	}
	view, err := Assemble(dataDir, events, total)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if view.Truncated {
		t.Fatal("the view was truncated, so full-coverage equality is not the right assertion")
	}
	all, err := events.Entries(0)
	if err != nil {
		t.Fatalf("reading every record: %v", err)
	}
	direct := Reduce(0, 0, total, all)
	summary := view.Summary()

	if len(summary.Keys) != len(direct.Keys) {
		t.Fatalf("summary has %d keys, direct reduce has %d", len(summary.Keys), len(direct.Keys))
	}
	for index, want := range direct.Keys {
		got := summary.Keys[index]
		if got.Key != want.Key {
			t.Errorf("key %d = %+v, want %+v", index, got.Key, want.Key)
			continue
		}
		if got.Aggregate.Invocations != want.Aggregate.Invocations {
			t.Errorf("%+v invocations = %d, want %d", got.Key, got.Aggregate.Invocations, want.Aggregate.Invocations)
		}
		if got.Aggregate.Latency.Count != want.Aggregate.Latency.Count {
			t.Errorf("%+v latency count = %d, want %d", got.Key, got.Aggregate.Latency.Count, want.Aggregate.Latency.Count)
		}
		if got.Aggregate.Latency.Sum != want.Aggregate.Latency.Sum {
			t.Errorf("%+v latency sum = %d, want %d", got.Key, got.Aggregate.Latency.Sum, want.Aggregate.Latency.Sum)
		}
		// The drill-down id has to survive the whole path: reduce, seal, merge
		// upward, assemble, summarise. If it does not, "skill X was never used"
		// stops being answerable down to an exact invocation.
		if got.Aggregate.FirstEventID != want.Aggregate.FirstEventID {
			t.Errorf("%+v drill-down id = %q, want %q", got.Key, got.Aggregate.FirstEventID, want.Aggregate.FirstEventID)
		}
	}
}

// TestViewIgnoresForeignBlocks asserts a view still answers while a reseal is
// pending: a block this build refuses contributes nothing rather than failing
// the read, and the shortfall surfaces as Truncated instead of a partial answer
// presented as a complete one.
//
// The record count has to exceed MaxVerbatimEntries for this to be a test of
// anything. Below that the verbatim tail covers the whole store, no block is
// read, and the assertion would pass without exercising a block at all.
func TestViewIgnoresForeignBlocks(t *testing.T) {
	const total = 1_000
	dataDir, events := seededStore(t, total)
	if _, err := Backfill(dataDir, events, total); err != nil {
		t.Fatalf("seal: %v", err)
	}
	view, err := Assemble(dataDir, events, total)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(view.Blocks) == 0 {
		t.Fatal("the baseline view read no blocks, so invalidating one proves nothing")
	}
	if view.Truncated {
		t.Fatal("the baseline view was already truncated")
	}

	// Corrupt the oldest block the view depended on.
	oldest := view.Blocks[0]
	path := filepath.Join(Dir(dataDir), blockName(oldest.Tier, oldest.Start, oldest.End))
	foreign := oldest
	foreign.BlockVersion = BlockVersion + 99
	data, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}

	after, err := Assemble(dataDir, events, total)
	if err != nil {
		t.Fatalf("assembling over a foreign block: %v", err)
	}
	// The refused block must not appear, and the read must still succeed. The
	// view is free to cover the same range with finer blocks instead — which is
	// what it does, and it is the better answer: coverage is preserved by
	// dropping to a lower tier rather than by reporting a hole.
	for _, block := range after.Blocks {
		if block.Tier == oldest.Tier && block.Start == oldest.Start {
			t.Errorf("the view read the block at t%d [%d,%d) that this build refuses",
				block.Tier, block.Start, block.End)
		}
	}
	// Whichever way it covered the range, a record must still be accounted for
	// exactly once — the invariant a reader depends on.
	for index := 1; index < len(after.Blocks); index++ {
		if after.Blocks[index].Start < after.Blocks[index-1].End {
			t.Errorf("blocks %d and %d overlap after the fallback", index-1, index)
		}
	}
}

// TestPrunedDirectoryStillCoversHistory is the regression guard for the bug that
// cost the most to find here, because its symptom was a wrong number rather than
// an error.
//
// Pruning is what keeps the rollup directory small, and an earlier retention
// rule kept a fixed count of blocks per tier. That left a hole between where a
// coarse tier stopped covering and where the retained fine blocks began — a
// tier-3 block ends every 1,000 positions, so retaining tier-2 blocks from
// 30,100 onwards left [30,000, 30,100) described by nothing. The view walked
// backwards, stalled at the hole, and reported 4% of history as though it were
// all of it.
//
// The assertion is coverage, not block count: after a prune a view must still
// account for every record, whatever arrangement of tiers it uses to do it.
func TestPrunedDirectoryStillCoversHistory(t *testing.T) {
	// Enough history for tier 3 to exist and for pruning to have work to do.
	const total = 11_000
	dataDir, events := seededStore(t, total)
	result, err := Backfill(dataDir, events, total)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if result.Pruned == 0 {
		t.Fatal("nothing was pruned, so this does not test a pruned directory")
	}

	view, err := Assemble(dataDir, events, total)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if view.Truncated {
		t.Error("a view over a pruned directory could not reach the start of history")
	}

	summary := view.Summary()
	var invocations uint64
	for _, entry := range summary.Keys {
		invocations += entry.Aggregate.Invocations
	}
	if invocations != total {
		t.Errorf("a view over a pruned directory accounted for %d of %d records", invocations, total)
	}
}

// TestPruneKeepsCoverageContiguous asserts the structural property the coverage
// test depends on: the retained blocks must chain from zero up to the coverage
// boundary with no gap, since a view walks that chain backwards and stops at the
// first position no block ends at.
func TestPruneKeepsCoverageContiguous(t *testing.T) {
	const total = 11_000
	dataDir, events := seededStore(t, total)
	if _, err := Backfill(dataDir, events, total); err != nil {
		t.Fatalf("seal: %v", err)
	}
	view, err := Assemble(dataDir, events, total)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(view.Blocks) == 0 {
		t.Fatal("no blocks were read")
	}
	if view.Blocks[0].Start != 0 {
		t.Errorf("the oldest block starts at %d, want 0 — coverage must reach the start of history",
			view.Blocks[0].Start)
	}
	for index := 1; index < len(view.Blocks); index++ {
		if view.Blocks[index].Start != view.Blocks[index-1].End {
			t.Errorf("a gap between block %d ending at %d and block %d starting at %d",
				index-1, view.Blocks[index-1].End, index, view.Blocks[index].Start)
		}
	}
	if view.Verbatim[0].Position != view.CoveredTo+1 {
		t.Errorf("the verbatim tail starts at position %d, want %d — blocks and records must be adjacent",
			view.Verbatim[0].Position, view.CoveredTo+1)
	}
}

// TestPruneIsIdempotent asserts a second prune over an unchanged directory
// removes nothing, so a repeated scan does not keep churning files.
func TestPruneIsIdempotent(t *testing.T) {
	const total = 11_000
	dataDir, events := seededStore(t, total)
	if _, err := Backfill(dataDir, events, total); err != nil {
		t.Fatalf("seal: %v", err)
	}
	again, err := Prune(Dir(dataDir), total)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if again != 0 {
		t.Errorf("a second prune removed %d more blocks, want 0", again)
	}
}

// TestPruneLeavesForeignFilesAlone asserts the directory is this package's but a
// stray file in it is not this package's to delete.
func TestPruneLeavesForeignFilesAlone(t *testing.T) {
	const total = 11_000
	dataDir, events := seededStore(t, total)
	if _, err := Backfill(dataDir, events, total); err != nil {
		t.Fatalf("seal: %v", err)
	}
	stray := filepath.Join(Dir(dataDir), "notes.txt")
	if err := os.WriteFile(stray, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(Dir(dataDir), total); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("prune removed a file it did not write: %v", err)
	}
}
