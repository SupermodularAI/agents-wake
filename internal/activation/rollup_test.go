package activation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/rollup"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// writeConfig puts one key in the config file the paths point at.
func writeConfig(t *testing.T, paths config.Paths, body string) {
	t.Helper()
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedEvents writes count records ending at the given age, oldest first.
func seedEvents(t *testing.T, paths config.Paths, count int, oldest time.Duration) *store.Store {
	t.Helper()
	events := openEvents(paths)
	base := time.Now().UTC().Add(-oldest)
	records := make([]record.Record, 0, count)
	for index := range count {
		outcome := record.OutcomeOK
		duration := int64(index % 500)
		records = append(records, record.Record{
			SchemaVersion: record.SchemaVersion,
			EventID:       record.DeriveEventID("claude-code", record.Identifier(fmt.Sprintf("roll-%d", index))),
			Timestamp:     base.Add(time.Duration(index) * time.Second),
			Harness:       "claude-code",
			SessionID:     record.Identifier(fmt.Sprintf("session-%d", index/10)),
			Repo:          record.Hash("abcdef0123456789abcdef0123456789"),
			Kind:          record.KindSkill,
			Name:          record.Identifier(fmt.Sprintf("skill-%d", index%3)),
			Invoker:       record.InvokerModel,
			Outcome:       &outcome,
			DurationMS:    &duration,
		})
	}
	result, err := events.Append(records)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if result.Written != count {
		t.Fatalf("seeded %d of %d records", result.Written, count)
	}
	return events
}

// TestRollupsAreOffByDefault is the compatibility assertion for this change.
//
// store.rollup_after has existed since T002 with the sentinel default "never"
// and has enforced nothing (ADR-0014). This PR gives it an effect, so the
// default must keep doing exactly what it did: a user who has not set the key
// gets no rollup directory and no new files anywhere.
func TestRollupsAreOffByDefault(t *testing.T) {
	paths := testPaths(t)
	events := seedEvents(t, paths, 50, 90*24*time.Hour)

	if err := sealRollups(paths, events); err != nil {
		t.Fatalf("sealRollups with the default config: %v", err)
	}
	if _, err := os.Stat(rollup.Dir(paths.DataDir)); !os.IsNotExist(err) {
		t.Error("a default configuration created a rollup directory; the key's default must stay inert")
	}
	// And a backfill is equally inert: there is no floor to lower until sealing
	// is switched on.
	if err := BackfillRollups(paths, events); err != nil {
		t.Fatalf("BackfillRollups with the default config: %v", err)
	}
	if _, err := os.Stat(rollup.Dir(paths.DataDir)); !os.IsNotExist(err) {
		t.Error("a backfill created a rollup directory while rollups were off")
	}
}

// TestRollupsStayOffWhenSetToNever asserts the sentinel spelled out explicitly
// behaves as the default does.
func TestRollupsStayOffWhenSetToNever(t *testing.T) {
	paths := testPaths(t)
	writeConfig(t, paths, "[store]\nrollup_after = \"never\"\n")
	events := seedEvents(t, paths, 50, 90*24*time.Hour)

	if err := sealRollups(paths, events); err != nil {
		t.Fatalf("sealRollups: %v", err)
	}
	if _, err := os.Stat(rollup.Dir(paths.DataDir)); !os.IsNotExist(err) {
		t.Error("rollup_after = never created a rollup directory")
	}
}

// TestRollupsSealWhenEnabled asserts a duration turns sealing on, and that what
// gets sealed is old enough to be outside the window.
func TestRollupsSealWhenEnabled(t *testing.T) {
	paths := testPaths(t)
	writeConfig(t, paths, "[store]\nrollup_after = \"7d\"\n")
	// Fifty records, all of them thirty days old, so every one is sealable.
	events := seedEvents(t, paths, 50, 30*24*time.Hour)

	// Backfill, not a plain seal: sealing builds forward from enablement, so
	// history already in the spool is only sealed when it is asked for.
	if err := BackfillRollups(paths, events); err != nil {
		t.Fatalf("BackfillRollups: %v", err)
	}
	names := blockFiles(t, paths)
	if len(names) == 0 {
		t.Fatal("a backfill sealed nothing over fifty records that are all thirty days old")
	}
}

// TestRecentRecordsAreNotSealed asserts the window is honoured. Records inside
// it stay verbatim, because a block is sealed once and never recomputed — so
// sealing a range that is still being written to would freeze it incomplete.
func TestRecentRecordsAreNotSealed(t *testing.T) {
	paths := testPaths(t)
	writeConfig(t, paths, "[store]\nrollup_after = \"7d\"\n")
	// Every record is one hour old, so all of them are inside the window.
	events := seedEvents(t, paths, 50, time.Hour)

	if err := BackfillRollups(paths, events); err != nil {
		t.Fatalf("BackfillRollups: %v", err)
	}
	if names := blockFiles(t, paths); len(names) != 0 {
		t.Errorf("records inside the retention window were sealed into %d blocks", len(names))
	}
}

// TestSealableHeadSnapsToFanoutBoundary asserts the snap the design calls for.
// Building forward from a fanout boundary rather than from wherever the window
// falls is what stops a permanent coverage hole appearing at the moment rollups
// were switched on.
func TestSealableHeadSnapsToFanoutBoundary(t *testing.T) {
	paths := testPaths(t)
	// Twenty-five old records and none recent, so the window puts the frontier
	// at 25 — which is not a fanout boundary.
	events := seedEvents(t, paths, 25, 30*24*time.Hour)

	sealable, err := sealableHead(events, 25, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("sealableHead: %v", err)
	}
	if sealable%rollup.Fanout != 0 {
		t.Errorf("sealable head = %d, which is not a multiple of the fanout %d", sealable, rollup.Fanout)
	}
	if sealable != 20 {
		t.Errorf("sealable head = %d, want 20 (25 snapped down to a fanout boundary)", sealable)
	}
}

// TestSealIsSkippedForAnEmptyStore asserts a machine that never ingested is a
// normal first run: nothing to seal, no directory, no error.
func TestSealIsSkippedForAnEmptyStore(t *testing.T) {
	paths := testPaths(t)
	writeConfig(t, paths, "[store]\nrollup_after = \"7d\"\n")
	events := openEvents(paths)

	if err := sealRollups(paths, events); err != nil {
		t.Fatalf("sealRollups on an empty store: %v", err)
	}
	if names := blockFiles(t, paths); len(names) != 0 {
		t.Errorf("an empty store produced %d blocks", len(names))
	}
	// The enablement mark is expected: recording the floor on an empty store is
	// what makes everything appended afterwards sealable.
	if _, found := rollup.ReadEnablement(rollup.Dir(paths.DataDir)); !found {
		t.Error("enabling on an empty store recorded no floor, so nothing appended later would ever seal")
	}
}

// TestOpenEventsAttachesDerivedData asserts the wiring that makes invalidation
// work: a Discard through this package's own accessor takes the rollup directory
// with it. Both Discard call sites in this package go through openEvents, so
// this is the test that a third one would have to keep passing.
func TestOpenEventsAttachesDerivedData(t *testing.T) {
	paths := testPaths(t)
	writeConfig(t, paths, "[store]\nrollup_after = \"7d\"\n")
	events := seedEvents(t, paths, 50, 30*24*time.Hour)
	if err := sealRollups(paths, events); err != nil {
		t.Fatalf("sealRollups: %v", err)
	}
	if _, err := os.Stat(rollup.Dir(paths.DataDir)); err != nil {
		t.Fatalf("nothing was sealed, so this proves nothing: %v", err)
	}

	if err := openEvents(paths).Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Stat(rollup.Dir(paths.DataDir)); !os.IsNotExist(err) {
		t.Error("blocks survived a Discard, so a rebuild would leave summaries of records that no longer exist")
	}
}

// TestRollupWindowIgnoresAnUnusableValue asserts a config file this build cannot
// use leaves rollups off rather than guessing a window. config.Load reports an
// unusable value as a Problem and falls back to the default, which is the
// sentinel — so the safe direction is already the one the config layer takes,
// and this pins it.
func TestRollupWindowIgnoresAnUnusableValue(t *testing.T) {
	paths := testPaths(t)
	writeConfig(t, paths, "[store]\nrollup_after = \"not-a-duration\"\n")
	if _, enabled := rollupWindow(paths); enabled {
		t.Error("an unusable rollup_after value enabled sealing")
	}
}

// TestSealRollupsIsIdempotent asserts a second scan over unchanged history
// writes nothing new, which is what keeps a hook-fired scan cheap.
func TestSealRollupsIsIdempotent(t *testing.T) {
	paths := testPaths(t)
	writeConfig(t, paths, "[store]\nrollup_after = \"7d\"\n")
	events := seedEvents(t, paths, 50, 30*24*time.Hour)

	if err := sealRollups(paths, events); err != nil {
		t.Fatalf("first seal: %v", err)
	}
	before, err := os.ReadDir(rollup.Dir(paths.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	stamps := make(map[string]time.Time, len(before))
	for _, name := range before {
		info, infoErr := name.Info()
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		stamps[name.Name()] = info.ModTime()
	}

	if sealErr := sealRollups(paths, events); sealErr != nil {
		t.Fatalf("second seal: %v", sealErr)
	}
	after, err := os.ReadDir(rollup.Dir(paths.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("a second seal changed the block count from %d to %d", len(before), len(after))
	}
	for _, name := range after {
		info, err := name.Info()
		if err != nil {
			t.Fatal(err)
		}
		if was, found := stamps[name.Name()]; !found {
			t.Errorf("a second seal wrote a new block %q", name.Name())
		} else if !info.ModTime().Equal(was) {
			t.Errorf("a second seal rewrote %q, but a sealed block must never be recomputed", name.Name())
		}
	}
}

// TestNoStrayFilesInTheRollupDirectory asserts the directory holds only blocks
// once a seal returns — in particular that no temporary file is left behind,
// since WriteBlock publishes through a rename rather than atomicfile.
func TestNoStrayFilesInTheRollupDirectory(t *testing.T) {
	paths := testPaths(t)
	writeConfig(t, paths, "[store]\nrollup_after = \"7d\"\n")
	events := seedEvents(t, paths, 120, 30*24*time.Hour)
	if err := BackfillRollups(paths, events); err != nil {
		t.Fatalf("BackfillRollups: %v", err)
	}
	names, err := os.ReadDir(rollup.Dir(paths.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(blockFiles(t, paths)) == 0 {
		t.Fatal("nothing was sealed")
	}
	// Only blocks and this package's own enablement mark. In particular no
	// temporary file, since a block is published through a rename of its own
	// rather than through atomicfile.
	for _, name := range names {
		if _, _, _, ok := rollup.ParseBlockName(name.Name()); ok {
			continue
		}
		if name.Name() == "enabled.json" {
			continue
		}
		t.Errorf("stray file %q left in the rollup directory", filepath.Base(name.Name()))
	}
}

// blockFiles lists the sealed blocks under a data directory, ignoring the
// enablement mark and anything else that is not a block.
func blockFiles(t *testing.T, paths config.Paths) []string {
	t.Helper()
	entries, err := os.ReadDir(rollup.Dir(paths.DataDir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, _, _, ok := rollup.ParseBlockName(entry.Name()); ok {
			names = append(names, entry.Name())
		}
	}
	return names
}

// seedMixedAges writes count records that are all old except the one at the
// given index, which is recent. The index is a position in write order, so the
// recent record sits early in the spool.
func seedMixedAges(t *testing.T, paths config.Paths, count, recentAt int) *store.Store {
	t.Helper()
	events := openEvents(paths)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	recent := time.Now().UTC().Add(-time.Hour)
	records := make([]record.Record, 0, count)
	for index := range count {
		stamp := old.Add(time.Duration(index) * time.Minute)
		if index == recentAt {
			stamp = recent
		}
		outcome := record.OutcomeOK
		duration := int64(index % 500)
		records = append(records, record.Record{
			SchemaVersion: record.SchemaVersion,
			EventID:       record.DeriveEventID("claude-code", record.Identifier(fmt.Sprintf("mixed-%d", index))),
			Timestamp:     stamp,
			Harness:       "claude-code",
			SessionID:     record.Identifier(fmt.Sprintf("session-%d", index/10)),
			Repo:          record.Hash("abcdef0123456789abcdef0123456789"),
			Kind:          record.KindSkill,
			Name:          record.Identifier(fmt.Sprintf("skill-%d", index%3)),
			Invoker:       record.InvokerModel,
			Outcome:       &outcome,
			DurationMS:    &duration,
		})
	}
	result, err := events.Append(records)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if result.Written != count {
		t.Fatalf("seeded %d of %d records", result.Written, count)
	}
	return events
}

// TestOneRecentRecordDoesNotVetoTheWholeRange is the regression guard for a
// silent never-seal.
//
// Positions are not time-ordered — a transcript imported today can carry an
// event from last week — so the first record inside the retention window is not
// the end of the sealable region. Taking it as the end meant a single recent
// record at a low position collapsed the frontier to zero: nothing was ever
// sealed on that machine, with no error anywhere. Every earlier test seeded
// records of a uniform age, so none of them could see it.
func TestOneRecentRecordDoesNotVetoTheWholeRange(t *testing.T) {
	paths := testPaths(t)
	// One recent record at position 4, ninety-nine records thirty days old.
	events := seedMixedAges(t, paths, 100, 3)

	frontier, err := sealableHead(events, 100, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("sealableHead: %v", err)
	}
	// The block holding the recent record is not sealable, so the frontier stops
	// below it — but the ninety records after it must not be lost with it.
	if frontier != 0 {
		t.Errorf("frontier = %d, want 0: the recent record is in the first block, so nothing below it is sealable", frontier)
	}

	// The same spool with the recent record later on: everything below its block
	// must still seal.
	later := testPaths(t)
	laterEvents := seedMixedAges(t, later, 100, 55)
	laterFrontier, err := sealableHead(laterEvents, 100, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("sealableHead: %v", err)
	}
	if laterFrontier != 50 {
		t.Errorf("frontier = %d, want 50: a recent record in the block at [50,60) must not veto the fifty positions below it",
			laterFrontier)
	}
}

// TestSealableHeadNeverSealsAnInWindowRecord asserts the direction the fix must
// not trade away: the frontier may be conservative, but it must never rise above
// a record still inside the window.
func TestSealableHeadNeverSealsAnInWindowRecord(t *testing.T) {
	for _, recentAt := range []int{0, 3, 27, 55, 99} {
		paths := testPaths(t)
		events := seedMixedAges(t, paths, 100, recentAt)
		frontier, err := sealableHead(events, 100, 7*24*time.Hour)
		if err != nil {
			t.Fatalf("recent at %d: %v", recentAt, err)
		}
		// The recent record's position is one-based.
		if frontier > uint64(recentAt) {
			t.Errorf("recent record at position %d but frontier = %d: an in-window record would be sealed",
				recentAt+1, frontier)
		}
		if frontier%rollup.Fanout != 0 {
			t.Errorf("frontier = %d, which is not a fanout boundary", frontier)
		}
	}
}

// TestASealFailureDoesNotFailTheIngest asserts the claim the package comments
// make: a rollup is a derived cache, so its failure must not fail the command
// that produced the records.
//
// The records are already durable when a seal runs, and the next scan retries
// the same blocks — so returning the error would make `wake ingest` exit
// non-zero and skip its "Imported N" line over a failure that cost nothing.
// noteRollupFailure records it instead.
func TestASealFailureDoesNotFailTheIngest(t *testing.T) {
	before := RollupFailures()
	if noteRollupFailure(nil) {
		t.Error("noteRollupFailure reported a failure for a nil error")
	}
	if RollupFailures() != before {
		t.Error("a nil error incremented the failure count")
	}
	if !noteRollupFailure(errSealFailed) {
		t.Error("noteRollupFailure did not report a real failure")
	}
	if RollupFailures() != before+1 {
		t.Errorf("failure count = %d, want %d", RollupFailures(), before+1)
	}
}

// errSealFailed stands in for a seal error in the test above.
var errSealFailed = errors.New("seal failed")
