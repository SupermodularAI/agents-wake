package rollup

import (
	"os"
	"testing"
)

// TestSealBuildsForwardFromEnablement asserts the default the design calls for:
// switching rollups on does not reach back over history already in the spool.
//
// Building forward is what keeps enablement cheap and predictable, and the floor
// is persisted because nothing in the spool records when the key was set — a
// floor recomputed per run would seal everything on the second run and make the
// opt-in meaningless.
func TestSealBuildsForwardFromEnablement(t *testing.T) {
	const existing = 100
	dataDir, events := seededStore(t, existing)

	result, err := Seal(dataDir, events, existing)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if result.Sealed != 0 {
		t.Errorf("a first seal wrote %d blocks over pre-existing history, want 0", result.Sealed)
	}

	mark, found := ReadEnablement(Dir(dataDir))
	if !found {
		t.Fatal("no enablement mark was recorded")
	}
	if mark.Floor != existing {
		t.Errorf("floor = %d, want %d (the head when rollups were enabled)", mark.Floor, existing)
	}
	if mark.Backfilled {
		t.Error("a plain seal recorded a backfill")
	}
}

// TestEnablementFloorSnapsToFanoutBoundary asserts the snap that prevents a
// permanent coverage hole. A floor mid-block would leave that block neither
// sealable whole nor coverable by anything finer, and a block is sealed once.
func TestEnablementFloorSnapsToFanoutBoundary(t *testing.T) {
	dir := Dir(t.TempDir())
	if err := WriteEnablement(dir, Enablement{Floor: 34}); err != nil {
		t.Fatalf("WriteEnablement: %v", err)
	}
	mark, found := ReadEnablement(dir)
	if !found {
		t.Fatal("the mark was not readable")
	}
	if mark.Floor%Fanout != 0 {
		t.Errorf("floor = %d, which is not a fanout boundary", mark.Floor)
	}
	if mark.Floor != 30 {
		t.Errorf("floor = %d, want 30 (34 snapped down)", mark.Floor)
	}
}

// TestSealsForwardOfEnablement asserts records appended after enablement are
// sealed, which is the whole point of building forward rather than not at all.
func TestSealsForwardOfEnablement(t *testing.T) {
	const existing = 100
	dataDir, events := seededStore(t, existing)
	if _, err := Seal(dataDir, events, existing); err != nil {
		t.Fatalf("first seal: %v", err)
	}

	// A second store's worth of records lands after the floor.
	appendRecords(t, events, existing, 200)
	result, err := Seal(dataDir, events, existing+200)
	if err != nil {
		t.Fatalf("second seal: %v", err)
	}
	if result.Sealed == 0 {
		t.Error("no blocks were sealed for history appended after enablement")
	}
	for _, name := range blockNames(t, dataDir) {
		_, start, _, ok := ParseBlockName(name)
		if ok && start < existing {
			t.Errorf("block %q covers history from before enablement", name)
		}
	}
}

// TestBackfillIsAnExplicitOptIn asserts history before the floor is sealed only
// when asked for, and that asking is recorded so a later plain seal does not
// undo it.
func TestBackfillIsAnExplicitOptIn(t *testing.T) {
	const existing = 200
	dataDir, events := seededStore(t, existing)
	if _, err := Seal(dataDir, events, existing); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if names := blockNames(t, dataDir); len(names) != 0 {
		t.Fatalf("a plain seal produced %d blocks over pre-existing history", len(names))
	}

	result, err := Backfill(dataDir, events, existing)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if result.Sealed == 0 {
		t.Fatal("backfill sealed nothing")
	}
	mark, found := ReadEnablement(Dir(dataDir))
	if !found {
		t.Fatal("the mark went missing")
	}
	if mark.Floor != 0 {
		t.Errorf("floor = %d after a backfill, want 0", mark.Floor)
	}
	if !mark.Backfilled {
		t.Error("the backfill was not recorded, so a later seal could raise the floor again")
	}

	// A plain seal afterwards must not undo it.
	if _, err := Seal(dataDir, events, existing); err != nil {
		t.Fatalf("seal after backfill: %v", err)
	}
	after, _ := ReadEnablement(Dir(dataDir))
	if after.Floor != 0 || !after.Backfilled {
		t.Errorf("a plain seal after a backfill reset the mark to %+v", after)
	}
}

// TestForeignEnablementIsReEstablished asserts a mark this build cannot read is
// replaced rather than trusted. The conservative direction for a floor is to
// re-establish it, the same choice readDeliveryState makes for a cursor.
func TestForeignEnablementIsReEstablished(t *testing.T) {
	dir := Dir(t.TempDir())
	if err := WriteEnablement(dir, Enablement{Floor: 50}); err != nil {
		t.Fatal(err)
	}
	// Rewrite it with a version this build does not read.
	if err := os.WriteFile(enablementPath(dir),
		[]byte(`{"block_version":999,"schema_version":999,"floor":50}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found := ReadEnablement(dir); found {
		t.Error("a mark with foreign version stamps was accepted")
	}
}

// TestPruneKeepsTheEnablementMark asserts the mark survives a prune. Removing it
// would reset the floor to the head on the next run and strand every block
// below it.
func TestPruneKeepsTheEnablementMark(t *testing.T) {
	const total = 11_000
	dataDir, events := seededStore(t, total)
	if _, err := Backfill(dataDir, events, total); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if _, err := Prune(Dir(dataDir), total); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, found := ReadEnablement(Dir(dataDir)); !found {
		t.Error("prune removed the enablement mark")
	}
}
