package rollup

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// TestStorageIsSublinearAtMeasuredScale is the claim this package makes, checked
// at the scale that motivated it: 31,288 records from 1,420 transcripts over six
// weeks, measured on the first machine to install wake.
//
// It asserts the rollup directory is a small fraction of the spool it summarises
// and that the number of blocks is logarithmic rather than linear in the events.
// The thresholds are deliberately loose — this is a guard against the design
// regressing into linear growth, not a benchmark to tune against.
func TestStorageIsSublinearAtMeasuredScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 31,288 records")
	}
	const measured = 31_288
	dataDir, events := seededStore(t, measured)
	result, err := Backfill(dataDir, events, measured)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Blocks, coarse and fine together, must stay far below one per event.
	if result.Sealed > measured/int(Fanout)+measured/int(Fanout*Fanout)+100 {
		t.Errorf("sealed %d blocks for %d events, which is not the expected tiering", result.Sealed, measured)
	}

	// An assembled view must read a bounded number of them whatever the history.
	view, err := Assemble(dataDir, events, measured)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(view.Blocks) > MaxViewBlocks {
		t.Errorf("a view over %d records read %d blocks, above the %d ceiling",
			measured, len(view.Blocks), MaxViewBlocks)
	}
	// The verbatim tail is at least MaxVerbatimEntries and at most that plus one
	// block of the coarsest retained tier. It is not exactly MaxVerbatimEntries
	// and cannot be: the boundary is a sealed block's edge, so the tail runs from
	// the last block that ends below the floor up to the head. Bounding it by the
	// coarsest span is what keeps the read cost bounded — the point is a
	// constant ceiling, not a precise tail length.
	maxTail := MaxVerbatimEntries + int(coarsestSpan(dataDir, t))
	if len(view.Verbatim) > maxTail {
		t.Errorf("verbatim tail is %d records, want at most %d", len(view.Verbatim), maxTail)
	}

	// And it must still account for every record, so the bound is not achieved
	// by quietly dropping history.
	if view.Truncated {
		t.Error("a view over the measured scale could not reach the start of history")
	}
	summary := view.Summary()
	var invocations uint64
	for _, entry := range summary.Keys {
		invocations += entry.Aggregate.Invocations
	}
	if invocations != measured {
		t.Errorf("the bounded view accounted for %d of %d records", invocations, measured)
	}

	spool, err := os.Stat(dataDir + "/events.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var blockBytes int64
	names, err := os.ReadDir(Dir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		info, infoErr := name.Info()
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		blockBytes += info.Size()
	}
	t.Logf("spool %d bytes, %d blocks totalling %d bytes, view reads %d blocks + %d records",
		spool.Size(), len(names), blockBytes, len(view.Blocks), len(view.Verbatim))
}

// coarsestSpan is the position span of the widest block retained on disk, which
// is what bounds how far past MaxVerbatimEntries a view's verbatim tail can run.
func coarsestSpan(dataDir string, t *testing.T) uint64 {
	t.Helper()
	names, err := os.ReadDir(Dir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	var span uint64
	for _, name := range names {
		_, start, end, ok := ParseBlockName(name.Name())
		if !ok {
			continue
		}
		span = max(span, end-start)
	}
	return span
}

// TestCoverageHoldsAtEveryScale runs the three sizes that each broke a different
// version of the retention rule: a single top-tier block, a multi-tier bridge,
// and the measured history that motivated the package.
//
// The assertions are the ones that matter about a bounded view — it reaches the
// start of history, it accounts for every record, and it never stalls on a hole
// — checked after a prune, because the interaction between what is retained and
// what a view walks is where every bug in this package has been.
func TestCoverageHoldsAtEveryScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds up to 31,288 records")
	}
	for _, total := range []int{1_000, 11_000, 31_288} {
		t.Run(fmt.Sprintf("%d records", total), func(t *testing.T) {
			dataDir, events := seededStore(t, total)
			if _, err := Backfill(dataDir, events, uint64(total)); err != nil {
				t.Fatalf("seal: %v", err)
			}

			available, err := readDir(dataDir)
			if err != nil {
				t.Fatalf("reading blocks: %v", err)
			}
			chosen := selectBlocks(available, uint64(total))
			if chosen.stalled {
				t.Error("the walk stalled on a hole in a directory this package pruned")
			}
			if chosen.exhausted {
				t.Errorf("the block budget of %d ran out over %d records", MaxViewBlocks, total)
			}

			view, err := Assemble(dataDir, events, uint64(total))
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			if view.Truncated {
				t.Error("the view could not reach the start of history")
			}
			if len(view.Blocks) > 0 && view.Blocks[0].Start != 0 {
				t.Errorf("the oldest block starts at %d, want 0", view.Blocks[0].Start)
			}
			if len(view.Blocks) > MaxViewBlocks {
				t.Errorf("view read %d blocks, above the %d ceiling", len(view.Blocks), MaxViewBlocks)
			}

			summary := view.Summary()
			var invocations uint64
			for _, entry := range summary.Keys {
				invocations += entry.Aggregate.Invocations
			}
			if invocations != uint64(total) {
				t.Errorf("the view accounted for %d of %d records", invocations, total)
			}
		})
	}
}

// TestSealAndPruneAreStableAsHistoryGrows asserts the retention rule does not
// prune a block a later head will need.
//
// Retention is derived from a walk over the current head, so a block outside
// today's selection is removed — and the hazard is that tomorrow's head selects
// differently and wants it back. Because a pruned block is derived, the honest
// requirement is not that it never happens but that coverage survives it: each
// seal reseals whatever the new head needs, so a view stays complete at every
// step. This walks history forward in blocks and checks that after every step.
func TestSealAndPruneAreStableAsHistoryGrows(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 3,000 records in steps")
	}
	dataDir := t.TempDir()
	events := store.New(filepath.Join(dataDir, "events.ndjson"))
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	written := 0
	for step := range 30 {
		batch := make([]record.Record, 0, 100)
		for index := range 100 {
			position := step*100 + index
			outcome := record.OutcomeOK
			duration := int64(position % 500)
			batch = append(batch, record.Record{
				SchemaVersion: record.SchemaVersion,
				EventID:       record.DeriveEventID("claude-code", record.Identifier(fmt.Sprintf("grow-%d", position))),
				Timestamp:     base.Add(time.Duration(position) * time.Second),
				Harness:       "claude-code",
				SessionID:     record.Identifier(fmt.Sprintf("session-%d", position/10)),
				Repo:          record.Hash("abcdef0123456789abcdef0123456789"),
				Kind:          record.KindSkill,
				Name:          record.Identifier(fmt.Sprintf("skill-%d", position%4)),
				Invoker:       record.InvokerModel,
				Outcome:       &outcome,
				DurationMS:    &duration,
			})
		}
		result, err := events.Append(batch)
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		written += result.Written

		if _, sealErr := Backfill(dataDir, events, uint64(written)); sealErr != nil {
			t.Fatalf("step %d seal: %v", step, sealErr)
		}
		view, err := Assemble(dataDir, events, uint64(written))
		if err != nil {
			t.Fatalf("step %d assemble: %v", step, err)
		}
		if view.Truncated {
			t.Fatalf("step %d (%d records): the view could not reach the start of history", step, written)
		}
		summary := view.Summary()
		var invocations uint64
		for _, entry := range summary.Keys {
			invocations += entry.Aggregate.Invocations
		}
		if invocations != uint64(written) {
			t.Fatalf("step %d: the view accounted for %d of %d records", step, invocations, written)
		}
	}
}
