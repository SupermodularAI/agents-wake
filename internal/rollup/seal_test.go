package rollup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// seededStore writes count valid records to a spool and returns it with its
// data directory.
func seededStore(t *testing.T, count int) (string, *store.Store) {
	t.Helper()
	dataDir := t.TempDir()
	events := store.New(filepath.Join(dataDir, "events.ndjson"))
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	records := make([]record.Record, 0, count)
	for index := range count {
		outcome := record.OutcomeOK
		duration := int64(index % 900)
		records = append(records, record.Record{
			SchemaVersion: record.SchemaVersion,
			EventID:       record.DeriveEventID("claude-code", record.Identifier(fmt.Sprintf("seed-%d", index))),
			Timestamp:     base.Add(time.Duration(index) * time.Minute),
			Harness:       "claude-code",
			SessionID:     record.Identifier(fmt.Sprintf("session-%d", index/10)),
			Repo:          record.Hash("abcdef0123456789abcdef0123456789"),
			Kind:          record.KindSkill,
			Name:          record.Identifier(fmt.Sprintf("skill-%d", index%4)),
			Invoker:       record.InvokerModel,
			Outcome:       &outcome,
			DurationMS:    &duration,
		})
	}
	result, err := events.Append(records)
	if err != nil {
		t.Fatalf("seeding store: %v", err)
	}
	// Asserted, not assumed. Append drops a record that fails validation rather
	// than failing, so a fixture with one bad field seeds an empty spool and
	// every assertion below it passes vacuously — which is exactly what happened
	// while this file was being written.
	if result.Written != count {
		t.Fatalf("seeded %d records, want %d (dropped %d)", result.Written, count, result.Dropped)
	}
	return dataDir, events
}

// TestSealIsIncrementalBuildingForward asserts the steady state: history
// appended since the last scan is sealed, and everything already sealed is
// skipped. This is the path a hook-fired scan takes on every session end, so it
// is the one whose cost has to stay proportional to what is new.
func TestSealIsIncrementalBuildingForward(t *testing.T) {
	// Enable on an empty store so the floor is zero and everything appended
	// afterwards is forward of it.
	dataDir := t.TempDir()
	events := store.New(filepath.Join(dataDir, "events.ndjson"))
	if _, err := Seal(dataDir, events, 0, 0); err != nil {
		t.Fatalf("enabling: %v", err)
	}

	appendRecords(t, events, 0, 100)
	first, err := Seal(dataDir, events, 100, 100)
	if err != nil {
		t.Fatalf("first seal: %v", err)
	}
	if first.Sealed == 0 {
		t.Fatal("no blocks were sealed for history appended after enablement")
	}

	second, err := Seal(dataDir, events, 100, 100)
	if err != nil {
		t.Fatalf("second seal: %v", err)
	}
	if second.Sealed != 0 {
		t.Errorf("a second seal over unchanged history wrote %d blocks, want 0", second.Sealed)
	}

	// More history, and only the new frontier costs anything.
	appendRecords(t, events, 100, 100)
	third, err := Seal(dataDir, events, 200, 200)
	if err != nil {
		t.Fatalf("third seal: %v", err)
	}
	if third.Sealed == 0 {
		t.Error("appending 100 records sealed nothing")
	}
	if third.Sealed > int(Fanout)+2 {
		t.Errorf("appending 100 records sealed %d blocks, want about %d — the cost must be the new frontier only",
			third.Sealed, Fanout)
	}
}

// TestBackfillIsIncremental asserts the same skip-if-present property over the
// backfill path, since that is where the cost of re-sealing history would be
// largest.
func TestBackfillIsIncremental(t *testing.T) {
	dataDir, events := seededStore(t, 100)

	first, err := Backfill(dataDir, events, 100, 100)
	if err != nil {
		t.Fatalf("first seal: %v", err)
	}
	if first.Sealed == 0 {
		t.Fatal("first seal wrote no blocks")
	}
	if first.Skipped != 0 {
		t.Errorf("first seal skipped %d blocks, want 0", first.Skipped)
	}

	second, err := Backfill(dataDir, events, 100, 100)
	if err != nil {
		t.Fatalf("second seal: %v", err)
	}
	if second.Sealed != 0 {
		t.Errorf("second seal wrote %d blocks, want 0 — a sealed block must never be recomputed", second.Sealed)
	}
	if second.Skipped != first.Sealed {
		t.Errorf("second seal skipped %d, want the %d already sealed", second.Skipped, first.Sealed)
	}
}

// TestSealTiersUpward asserts a tier-2 block appears once ten tier-1 blocks
// exist, and that it is the merge of them. This is the O(log n) property: one
// hundred events are described by one tier-2 block rather than ten tier-1 ones.
func TestSealTiersUpward(t *testing.T) {
	dataDir, events := seededStore(t, 100)
	if _, err := Backfill(dataDir, events, 100, 100); err != nil {
		t.Fatalf("seal: %v", err)
	}

	tier2 := filepath.Join(Dir(dataDir), blockName(2, 0, 100))
	block, err := ReadBlock(tier2)
	if err != nil {
		t.Fatalf("reading tier-2 block: %v", err)
	}
	if block.Tier != 2 || block.Start != 0 || block.End != 100 {
		t.Errorf("tier-2 block covers t%d [%d,%d), want t2 [0,100)", block.Tier, block.Start, block.End)
	}

	// The tier-2 block must equal a merge of its ten children.
	children := make([]Block, 0, Fanout)
	for group := range Fanout {
		child, err := ReadBlock(filepath.Join(Dir(dataDir), blockName(1, group*Fanout, (group+1)*Fanout)))
		if err != nil {
			t.Fatalf("reading child %d: %v", group, err)
		}
		children = append(children, child)
	}
	wantJSON, _ := json.Marshal(Merge(2, 0, 100, children))
	gotJSON, _ := json.Marshal(block)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("tier-2 block is not the merge of its children\ngot:  %s\nwant: %s", gotJSON, wantJSON)
	}
}

// TestSealLeavesPartialFrontierUnsealed asserts an incomplete block is not
// sealed. A block is written once and never revisited, so sealing positions
// 30-40 while the spool holds 34 records would freeze six missing records into
// the summary permanently.
func TestSealLeavesPartialFrontierUnsealed(t *testing.T) {
	dataDir, events := seededStore(t, 34)
	if _, err := Backfill(dataDir, events, 34, 34); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(Dir(dataDir), blockName(1, 30, 40))); !errors.Is(err, os.ErrNotExist) {
		t.Error("a partial range was sealed; only complete blocks may be sealed")
	}
	// The three complete blocks below it must exist.
	for group := range uint64(3) {
		path := filepath.Join(Dir(dataDir), blockName(1, group*Fanout, (group+1)*Fanout))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("complete block [%d,%d) was not sealed: %v", group*Fanout, (group+1)*Fanout, err)
		}
	}
}

// TestReadBlockRefusesForeignVersions asserts both stamps are enforced. A block
// is derived data whose meaning depends on the record contract and on the reduce
// that produced it; either changing makes the stored numbers describe something
// else. This is the same rule readDeliveryState applies to a stored position.
func TestReadBlockRefusesForeignVersions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Block)
	}{
		{"foreign block version", func(b *Block) { b.BlockVersion = BlockVersion + 1 }},
		{"foreign record schema", func(b *Block) { b.SchemaVersion = record.SchemaVersion + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			block := Block{BlockVersion: BlockVersion, SchemaVersion: record.SchemaVersion, Tier: 1, Start: 0, End: 10}
			tc.mutate(&block)
			data, _ := json.MarshalIndent(block, "", "  ")
			path := filepath.Join(dir, blockName(1, 0, 10))
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadBlock(path); !errors.Is(err, ErrForeignBlock) {
				t.Errorf("ReadBlock error = %v, want ErrForeignBlock", err)
			}
		})
	}
}

// TestSealResealsForeignBlocks asserts a block this build cannot read is
// removed and written again rather than left to be skipped forever. The records
// are still in the spool, so there is no case where keeping the unreadable file
// serves a reader.
func TestSealResealsForeignBlocks(t *testing.T) {
	dataDir, events := seededStore(t, 20)
	if _, err := Backfill(dataDir, events, 20, 20); err != nil {
		t.Fatalf("initial seal: %v", err)
	}

	// Corrupt one sealed block the way a version bump would.
	path := filepath.Join(Dir(dataDir), blockName(1, 0, 10))
	stale := Block{BlockVersion: BlockVersion + 99, SchemaVersion: record.SchemaVersion}
	data, _ := json.MarshalIndent(stale, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Backfill(dataDir, events, 20, 20)
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if result.Refused != 1 {
		t.Errorf("Refused = %d, want 1", result.Refused)
	}
	if result.Sealed != 1 {
		t.Errorf("Sealed = %d, want 1 — the refused block must be written again", result.Sealed)
	}
	if _, err := ReadBlock(path); err != nil {
		t.Errorf("the resealed block is still unreadable: %v", err)
	}
}

// TestRemoveDeletesEverything asserts the directory goes wholesale, and that
// doing it twice is not an error. Blocks are invalidated as a set because a
// position range is not durable identity.
func TestRemoveDeletesEverything(t *testing.T) {
	dataDir, events := seededStore(t, 20)
	if _, err := Backfill(dataDir, events, 20, 20); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := Remove(dataDir); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(Dir(dataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the rollup directory survived Remove")
	}
	if err := Remove(dataDir); err != nil {
		t.Errorf("Remove on an absent directory returned %v, want nil — dropping derived data is idempotent", err)
	}
}

// TestSealOnEmptyStore asserts a machine that never ingested is a normal first
// run and not an error, and that nothing is created for it.
func TestSealOnEmptyStore(t *testing.T) {
	dataDir := t.TempDir()
	events := store.New(filepath.Join(dataDir, "events.ndjson"))
	result, err := Seal(dataDir, events, 0, 0)
	if err != nil {
		t.Fatalf("Seal on an empty store: %v", err)
	}
	if result.Sealed != 0 || result.Skipped != 0 {
		t.Errorf("result = %+v, want all zero", result)
	}
	if names := blockNames(t, dataDir); len(names) != 0 {
		t.Errorf("Seal wrote %d blocks for a store with no records", len(names))
	}
	// The enablement mark is written even here, and deliberately: recording a
	// floor of zero on an empty store is what makes everything appended
	// afterwards sealable. Deferring it to the first scan with sealable history
	// would put the floor above whatever arrived in between, and nothing below
	// a floor is ever sealed.
	mark, found := ReadEnablement(Dir(dataDir))
	if !found {
		t.Fatal("enabling on an empty store recorded no floor")
	}
	if mark.Floor != 0 {
		t.Errorf("floor = %d on an empty store, want 0", mark.Floor)
	}
}

// TestBlockNameRoundTrips asserts the name is a reliable identity, since the
// file name is what makes a seal skip work.
func TestBlockNameRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		tier       uint
		start, end uint64
	}{{1, 0, 10}, {2, 100, 200}, {5, 0, 100000}} {
		name := blockName(tc.tier, tc.start, tc.end)
		tier, start, end, ok := ParseBlockName(name)
		if !ok {
			t.Errorf("ParseBlockName(%q) failed", name)
			continue
		}
		if tier != tc.tier || start != tc.start || end != tc.end {
			t.Errorf("ParseBlockName(%q) = t%d [%d,%d), want t%d [%d,%d)",
				name, tier, start, end, tc.tier, tc.start, tc.end)
		}
	}
}

// TestParseBlockNameRejectsForeignNames asserts a stray file in the directory is
// left alone rather than parsed as a block.
//
// This predicate is load-bearing beyond tidiness: Prune builds an os.Remove path
// from any name it accepts, so a name that escaped the pattern would be a
// deletion outside the rollup directory. The traversal cases below are checked
// for that reason, not because a directory listing is expected to contain them.
// The pattern is fully anchored and admits only digits, hyphens and the .json
// suffix, so no separator or traversal sequence can match.
func TestParseBlockNameRejectsForeignNames(t *testing.T) {
	for _, name := range []string{
		// Ordinary neighbours in the data directory.
		"events.ndjson", "notes.txt", "", "enabled.json",
		"t1-000000000000-000000000010.json.bak",
		// Unpadded, so not a name this package writes.
		"t1-0-10.json",
		// A non-digit inside the padded range.
		"t1-00000000000a-000000000010.json",
		// Traversal and separators, which must never reach os.Remove.
		"../../../etc/passwd",
		"../t1-000000000000-000000000010.json",
		"t1-000000000000-000000000010.json/../../x",
		"/etc/shadow",
		"t1-000000000000-000000000010.json\x00.txt",
	} {
		if _, _, _, ok := ParseBlockName(name); ok {
			t.Errorf("ParseBlockName(%q) accepted a name that is not a block", name)
		}
	}
}

// TestBlockNamesSortInPositionOrder asserts the zero padding does its job, so a
// directory listing is in position order without a numeric sort.
func TestBlockNamesSortInPositionOrder(t *testing.T) {
	low := blockName(1, 90, 100)
	high := blockName(1, 100, 110)
	if low >= high {
		t.Errorf("%q does not sort before %q", low, high)
	}
}

// appendRecords adds count more records to a seeded store, starting at the given
// offset so their positions continue the existing history.
func appendRecords(t *testing.T, events *store.Store, offset, count int) {
	t.Helper()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	records := make([]record.Record, 0, count)
	for index := range count {
		position := offset + index
		outcome := record.OutcomeOK
		duration := int64(position % 900)
		records = append(records, record.Record{
			SchemaVersion: record.SchemaVersion,
			EventID:       record.DeriveEventID("claude-code", record.Identifier(fmt.Sprintf("seed-%d", position))),
			Timestamp:     base.Add(time.Duration(position) * time.Minute),
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
	result, err := events.Append(records)
	if err != nil {
		t.Fatalf("appending: %v", err)
	}
	if result.Written != count {
		t.Fatalf("appended %d of %d records", result.Written, count)
	}
}

// blockNames lists the block files in a data directory, ignoring anything that
// is not one.
func blockNames(t *testing.T, dataDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(Dir(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, _, _, ok := ParseBlockName(entry.Name()); ok {
			names = append(names, entry.Name())
		}
	}
	return names
}
