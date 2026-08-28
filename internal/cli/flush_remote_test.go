package cli

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// The default for the whole cli test build: never actually detach.
//
// afterScan fires from every scan in this package, so tests that predate this
// seam reach spawnFlush without knowing it exists — the three ingest tests that
// scan an unusable data root, an unreadable project table, or fail visibly. In
// a test binary os.Executable() is the suite itself, which ignores the
// positional arguments and re-runs every test, each of which scans and spawns
// again. hookChild never needed this because only `ingest --quiet` reaches it
// and all of its tests stub it.
//
// Production is untouched: flushChild is detach.Start in every build, and there
// `self` is the wake binary and `wake remote flush` terminates. The four tests
// below that assert on the spawn install their own recorder over this.
func init() { flushChild = func([]string) error { return nil } }

// recordFlushChild replaces the detached flush spawn with a recorder, so the
// test does not start a background copy of the test binary. It records every
// argv rather than the last, because "exactly one" is the property under test.
func recordFlushChild(t *testing.T, err error) *[][]string {
	t.Helper()
	recorded := &[][]string{}
	original := flushChild
	flushChild = func(argv []string) error {
		*recorded = append(*recorded, argv)
		return err
	}
	t.Cleanup(func() { flushChild = original })
	return recorded
}

// wantFlushArgv is the argv a spawned flush must carry: this same executable,
// which carries the same delivery code by construction.
func wantFlushArgv(t *testing.T) []string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	return []string{self, "remote", "flush"}
}

// Acceptance criterion 6 on the SessionStart path. ADR-0018 puts a flush there,
// and the existing trigger already re-execs into runHookScan, so hooking the
// scan is that flush point — no new hook and no new trigger is installed.
func TestHookScanSpawnsExactlyOneDetachedFlush(t *testing.T) {
	isolateRemote(t)
	recorded := recordFlushChild(t, nil)

	stdout, stderr, err := runSplit(t, "ingest", "--quiet", "--hook-scan")
	if err != nil {
		t.Errorf("error = %v, want nil", err)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("stdout = %q, stderr = %q; want both empty", stdout, stderr)
	}

	if len(*recorded) != 1 {
		t.Fatalf("the scan spawned %d flushes, want exactly 1: %v", len(*recorded), *recorded)
	}
	want := wantFlushArgv(t)
	if strings.Join((*recorded)[0], " ") != strings.Join(want, " ") {
		t.Errorf("spawned %v, want %v", (*recorded)[0], want)
	}
}

// Acceptance criterion 6 on the supplement path: ADR-0018's "after any command
// that scanned". That ingest returns without waiting is carried by there being
// no Wait anywhere on this path — detach.Start has none by construction — plus
// runSplit returning at all.
func TestInteractiveIngestSpawnsExactlyOneDetachedFlush(t *testing.T) {
	isolateRemote(t)
	recorded := recordFlushChild(t, nil)

	stdout, _, err := runSplit(t, "ingest")
	if err != nil {
		t.Fatalf("ingest error = %v", err)
	}
	if !strings.Contains(stdout, "Imported") {
		t.Errorf("ingest did not report what it imported:\n%s", stdout)
	}

	if len(*recorded) != 1 {
		t.Fatalf("the scan spawned %d flushes, want exactly 1: %v", len(*recorded), *recorded)
	}
	want := wantFlushArgv(t)
	if strings.Join((*recorded)[0], " ") != strings.Join(want, " ") {
		t.Errorf("spawned %v, want %v", (*recorded)[0], want)
	}
}

// Acceptance criterion 7. The hook-invoked scan exits 0 in silence whatever
// happened (ADR-0016), and a spawn that was lost costs nothing the next scan's
// flush cannot recover, because every id is derived from its source event
// (ADR-0004). The sentinel assertion is the difference between a discard and a
// swallowed-then-printed error.
func TestHookScanExitsZeroWhenTheFlushChildCannotBeSpawned(t *testing.T) {
	isolateRemote(t)
	recordFlushChild(t, errors.New("sentinel"))

	stdout, stderr, err := runSplit(t, "ingest", "--quiet", "--hook-scan")

	if err != nil {
		t.Errorf("error = %v, want nil — the trigger reports nothing, ever", err)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("stdout = %q, stderr = %q; want both empty", stdout, stderr)
	}
	if strings.Contains(stdout, "sentinel") || strings.Contains(stderr, "sentinel") {
		t.Errorf("the spawn failure reached a stream: stdout = %q, stderr = %q", stdout, stderr)
	}
}

// The seam's placement, and the reason the trigger stays fast: `ingest --quiet`
// spawns the scan and scans nothing itself, so it must spawn no flush either.
// The flush rides the detached child, which is what keeps the hook's own latency
// unchanged (ADR-0016).
func TestQuietIngestDoesNotFlushInTheParent(t *testing.T) {
	isolateRemote(t)
	recordHookChild(t, nil)
	recorded := recordFlushChild(t, nil)

	if _, _, err := runSplit(t, "ingest", "--quiet"); err != nil {
		t.Fatalf("ingest --quiet error = %v", err)
	}

	if len(*recorded) != 0 {
		t.Errorf("the trigger flushed in the parent: %v", *recorded)
	}
}
