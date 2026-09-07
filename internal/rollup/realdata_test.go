package rollup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/store"
)

// TestAgainstRealSpool runs a backfill over a copy of a real spool when one is
// pointed at by WAKE_TEST_SPOOL. Skipped otherwise, so it never gates CI and
// never needs a fixture committed.
//
// It exists because synthetic fixtures choose their own key cardinality, and
// key cardinality is what bounds a block. A generated spool with four skill
// names says nothing about whether the design is economical on real telemetry.
//
// Measured on a real 21.9 MB spool of 32,032 records (2026-09-07): 69 blocks
// totalling 218 KB, or 1.00% of the spool; a view read 12 blocks plus 502
// records rather than decoding 32,032; a repeated seal wrote and pruned
// nothing; coverage was complete. The spool held only 98 distinct
// (kind, name, outcome) keys across all 32,032 invocations — better than the
// synthetic fixtures, and the reason the economics hold.
//
// Point it at a COPY. It writes only into t.TempDir(), but a test that reads a
// developer's live telemetry should not be one keystroke from writing to it.
func TestAgainstRealSpool(t *testing.T) {
	source := os.Getenv("WAKE_TEST_SPOOL")
	if source == "" {
		t.Skip("set WAKE_TEST_SPOOL to a copy of a real events.ndjson")
	}
	dataDir := t.TempDir()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(filepath.Join(dataDir, "events.ndjson"), raw, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	events := store.New(filepath.Join(dataDir, "events.ndjson"))
	head, err := events.Head()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real spool: %d bytes, head=%d", len(raw), head)

	result, err := Backfill(dataDir, events, head, head)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	t.Logf("backfill: %+v", result)

	repeat, err := Seal(dataDir, events, head, head)
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if repeat.Sealed != 0 || repeat.Pruned != 0 {
		t.Errorf("repeat seal on real data wrote %d, pruned %d — want 0, 0", repeat.Sealed, repeat.Pruned)
	}

	view, err := Assemble(dataDir, events, head)
	if err != nil {
		t.Fatal(err)
	}
	summary := view.Summary()
	var invocations uint64
	for _, entry := range summary.Keys {
		invocations += entry.Aggregate.Invocations
	}
	if invocations != head {
		t.Errorf("the view accounted for %d of %d real records", invocations, head)
	}
	if view.Truncated {
		t.Error("the view could not reach the start of real history")
	}

	var blockBytes int64
	names, _ := os.ReadDir(Dir(dataDir))
	for _, name := range names {
		info, _ := name.Info()
		blockBytes += info.Size()
	}
	t.Logf("REAL DATA: spool %d bytes -> %d blocks, %d bytes (%.2f%%); view reads %d blocks + %d records; %d distinct keys",
		len(raw), len(names), blockBytes, 100*float64(blockBytes)/float64(len(raw)),
		len(view.Blocks), len(view.Verbatim), len(summary.Keys))
}
