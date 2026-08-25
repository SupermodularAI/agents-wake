package activation

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// realTempDir returns a fresh temporary directory with every symlink resolved.
//
// The boundary is recorded canonically and matched lexically, so a test whose
// boundary was spelled through /var while the transcript named /private/var would
// exercise the alias path instead of the enclosure rule it is about. The cases that
// want a symlink build one.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temporary directory: %v", err)
	}
	return dir
}

// boundaryFixture returns a Claude Code directory with no history yet and a
// symlink-resolved directory to record as the collection boundary.
func boundaryFixture(t *testing.T) (claudeDir, base string) {
	t.Helper()
	claudeDir = filepath.Join(realTempDir(t), "claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() claude dir error = %v", err)
	}
	return claudeDir, realTempDir(t)
}

// transcriptAt writes one session holding exactly one terminal tool call, attributed
// to cwd and timestamped at.
func transcriptAt(t *testing.T, claudeDir, name, cwd string, at time.Time) {
	t.Helper()
	stamp := at.UTC().Format(time.RFC3339)
	transcript := `{"uuid":"` + name + `-1","sessionId":"` + name + `","cwd":"` + cwd + `","timestamp":"` + stamp + `","message":{"content":[{"type":"tool_use","id":"` + name + `-call","name":"Bash"}]}}
{"uuid":"` + name + `-2","sessionId":"` + name + `","cwd":"` + cwd + `","timestamp":"` + stamp + `","message":{"content":[{"type":"tool_result","tool_use_id":"` + name + `-call","is_error":false}]}}`
	writeFixture(t, filepath.Join(claudeDir, "projects", name, "session.jsonl"), transcript)
}

// countWalks replaces the history walk with one that counts entries and forwards.
//
// A call counter rather than a result, for the reason
// TestInitWithoutFullNeverWalksHarnessHistory gives: how many times the source is
// walked is the decision under test, and a walk that found nothing returns the same
// number as one that never ran.
func countWalks(t *testing.T) *int {
	t.Helper()
	original := importHistory
	t.Cleanup(func() { importHistory = original })
	walks := 0
	importHistory = func(repos *config.Repos, claudeDir string, destination *store.Store, stale claudecode.Staleness, scope collectionScope, discover *boundaryDiscovery) (int, health.Scan, error) {
		walks++
		return original(repos, claudeDir, destination, stale, scope, discover)
	}
	return &walks
}

// spoolEntries reads the derived event spool.
func spoolEntries(t *testing.T, paths config.Paths) []store.Entry {
	t.Helper()
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	return entries
}

// recordedRoots decodes the consented roots straight out of the table, so an
// assertion about what registration did is about the bytes rather than about what a
// resolver would answer.
func recordedRoots(t *testing.T, paths config.Paths) []string {
	t.Helper()
	raw, err := os.ReadFile(paths.ProjectsFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the project table: %v", err)
	}
	var table struct {
		Projects []struct {
			Root string `json:"root"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("decoding the project table: %v", err)
	}
	roots := make([]string, 0, len(table.Projects))
	for _, entry := range table.Projects {
		roots = append(roots, entry.Root)
	}
	return roots
}

func mustIdentify(t *testing.T, repos *config.Repos, cwd string) config.Identity {
	t.Helper()
	identity, err := repos.Identify(cwd)
	if err != nil {
		t.Fatalf("Identify(%q) error = %v", cwd, err)
	}
	return identity
}

// Acceptance item 2, end to end: a session in a repository that was never `init`ed
// but sits under the recorded boundary is attributed to that repository's own id.
//
// The second assertion is the one that matters. Attributing it to the boundary would
// also produce a record, and would fold every repository on the machine into one —
// destroying the per-project breakdown `report` and `serve` exist for.
func TestAScanUnderTheGlobalRootRegistersTheEnclosedRepositoryAndAttributesItsOwnId(t *testing.T) {
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	project := filepath.Join(base, "proj")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatalf("MkdirAll() project error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-a", project, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))

	if _, err := InitGlobal(paths, base, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("InitGlobal() error = %v", err)
	}
	written, err := Ingest(paths, claudeDir)
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if written == 0 {
		t.Fatal("Ingest() wrote 0 events; the second walk never imported what the first walk's discovery made collectable")
	}
	if counters := scanCounters(t, paths); counters.EventsWritten == 0 {
		t.Errorf("the recorded scan reports %d events written; the two walks' counters were not merged", counters.EventsWritten)
	}

	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	projectID := mustIdentify(t, repos, project)
	if !projectID.Matched {
		t.Fatal("the repository under the boundary was never registered")
	}
	boundaryID := mustIdentify(t, repos, base)
	entries := spoolEntries(t, paths)
	if len(entries) != 1 {
		t.Fatalf("the spool holds %d records, want 1", len(entries))
	}
	if got := string(entries[0].Record.Repo); got != projectID.ID {
		t.Errorf("the record is attributed to %s, want the repository's own id %s", got, projectID.ID)
	}
	if string(entries[0].Record.Repo) == boundaryID.ID {
		t.Error("the record is attributed to the boundary; every repository under it would collapse into one")
	}
}

// The common case costs nothing. With no boundary recorded WithinGlobalRoot is always
// false, so the discovery set stays empty and the second walk is never reached.
func TestAScanWithNoGlobalRootWalksOnce(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	if _, err := Init(paths, root, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	walks := countWalks(t)

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if *walks != 1 {
		t.Errorf("Ingest() walked the source %d times, want 1", *walks)
	}
}

// Twice, and never a third time. Registration happens once after the walk that
// observed the directories, so the sequence is bounded however many repositories one
// scan discovers (ADR-0032 §5).
func TestAScanThatDiscoversARepositoryWalksTwiceAndNoMore(t *testing.T) {
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	project := filepath.Join(base, "proj")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatalf("MkdirAll() project error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-a", project, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if _, err := InitGlobal(paths, base, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("InitGlobal() error = %v", err)
	}
	walks := countWalks(t)

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if *walks != 2 {
		t.Errorf("Ingest() walked the source %d times, want exactly 2", *walks)
	}
}

// Acceptance item 4. Auto-registration changes which repository an event belongs to,
// and nothing else: re-running the scan writes nothing twice, because every id is
// derived from the source event (ADR-0004) and the spool deduplicates on it.
func TestASecondScanAfterAutoRegistrationLeavesTheSpoolByteIdentical(t *testing.T) {
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	project := filepath.Join(base, "proj")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatalf("MkdirAll() project error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-a", project, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if _, err := InitGlobal(paths, base, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("InitGlobal() error = %v", err)
	}
	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("first Ingest() error = %v", err)
	}
	spool := filepath.Join(paths.DataDir, eventsFile)
	before, readErr := os.ReadFile(spool)
	if readErr != nil {
		t.Fatalf("reading the spool: %v", readErr)
	}

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("second Ingest() error = %v", err)
	}
	after, readErr := os.ReadFile(spool)
	if readErr != nil {
		t.Fatalf("reading the spool: %v", readErr)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the spool changed on a re-scan:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// Requirement 7 and the second half of acceptance item 7: a working directory a
// transcript names that is no longer there is skipped, counted, and not an error.
//
// Counted rather than silent, because a swallowed refusal is indistinguishable from a
// machine with nothing to discover — and an honest zero rather than lost collection,
// because there is nothing left there to read.
func TestAVanishedDirectoryUnderTheBoundaryIsSkippedAndCounted(t *testing.T) {
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	transcriptAt(t, claudeDir, "session-a", filepath.Join(base, "gone"), time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if _, err := InitGlobal(paths, base, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("InitGlobal() error = %v", err)
	}

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	counters := scanCounters(t, paths)
	if counters.BoundarySkipped != 1 {
		t.Errorf("BoundarySkipped = %d, want 1", counters.BoundarySkipped)
	}
	if counters.BoundaryRefused != 0 {
		t.Errorf("BoundaryRefused = %d; a directory that is gone is an honest zero, not a refusal", counters.BoundaryRefused)
	}
	if roots := recordedRoots(t, paths); len(roots) != 0 {
		t.Errorf("the table records %v, want no root for a directory that does not exist", roots)
	}
	if entries := spoolEntries(t, paths); len(entries) != 0 {
		t.Errorf("the spool holds %d records for a directory nothing can be read from", len(entries))
	}
}

// An auto-registered repository collects forward only, like every repository a plain
// `wake init` consents (ADR-0024, ADR-0025). On the trigger path that means the first
// discovered session's events are excluded — they predate the registration instant —
// and the next session's are collected. That is correct rather than a bug, and it is
// pinned here so nobody "fixes" it by clearing the boundary.
func TestAutoRegisteredEntriesInheritTheForwardOnlyDefault(t *testing.T) {
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	project := filepath.Join(base, "proj")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatalf("MkdirAll() project error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-old", project, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if _, err := InitGlobal(paths, base, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("InitGlobal() error = %v", err)
	}

	if _, err := Trigger(paths, claudeDir); err != nil {
		t.Fatalf("Trigger() error = %v", err)
	}
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	identity := mustIdentify(t, repos, project)
	if !identity.Matched {
		t.Fatal("the repository under the boundary was never registered")
	}
	from := repos.CollectsFrom(identity.ID)
	if from.IsZero() {
		t.Fatal("CollectsFrom() = the zero time; an auto-registered repository records no boundary and would import its whole history unasked")
	}
	if entries := spoolEntries(t, paths); len(entries) != 0 {
		t.Errorf("the spool holds %d records timestamped before the registration instant", len(entries))
	}

	transcriptAt(t, claudeDir, "session-new", project, from.Add(time.Minute))
	if _, err := Trigger(paths, claudeDir); err != nil {
		t.Fatalf("second Trigger() error = %v", err)
	}
	if entries := spoolEntries(t, paths); len(entries) != 1 {
		t.Errorf("the spool holds %d records, want the one timestamped after the registration instant", len(entries))
	}
}

// The import and the instant are two separate facts, and `--full` records both.
//
// The import is a user-asked scan and ignores every recorded boundary already
// (ADR-0025), so making it work by clearing collect_from would be a widening with no
// disclosure behind it: every later trigger would then import the whole history of
// every repository the boundary discovers.
func TestInitGlobalFullImportsHistoryAndStillRecordsTheRegistrationInstant(t *testing.T) {
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	project := filepath.Join(base, "proj")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatalf("MkdirAll() project error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-old", project, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))

	written, err := InitGlobal(paths, base, claudeDir, testExecutable(t), true)
	if err != nil {
		t.Fatalf("InitGlobal(full) error = %v", err)
	}
	if written == 0 {
		t.Error("InitGlobal(full) imported nothing; the history under the boundary was there to import")
	}

	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	identity := mustIdentify(t, repos, project)
	if !identity.Matched {
		t.Fatal("the repository under the boundary was never registered")
	}
	if repos.CollectsFrom(identity.ID).IsZero() {
		t.Error("CollectsFrom() = the zero time; --full cleared the forward-only instant instead of recording it")
	}
}

// A registration the table refuses is counted, and doctor is the only surface that can
// say so (ADR-0016 keeps the hook-invoked scan silent).
//
// The refusal here is the realistic one: a discovered root that contains a repository
// the user consented separately, which ADR-0019 §5 refuses in both directions so
// longest-prefix resolution stays unique.
//
// It is counted on every scan, because every scan re-observes the same directory: no
// entry matches it, nothing records that it was refused, and no command removes the
// entry it nests with. Two scans are what this asserts, because a machine that is
// collecting normally must not be pinned to "collects nothing" by a refusal that will
// still be there on the next scan and the one after that — a state word that can never
// change again is not a diagnosis. The number is the report; the state word is about
// whether the numbers can be trusted.
func TestARefusedRegistrationUnderTheBoundaryIsCountedAndDoesNotPinTheIntegrationState(t *testing.T) {
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	outer := filepath.Join(base, "a")
	inner := filepath.Join(outer, "b")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	executable := testExecutable(t)
	if _, err := Init(paths, inner, claudeDir, executable, false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := InitGlobal(paths, base, claudeDir, executable, false); err != nil {
		t.Fatalf("InitGlobal() error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-a", outer, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	// The consented repository, so the machine is collecting rather than merely not
	// broken: the assertion below is about a refusal not blinding a working install.
	transcriptAt(t, claudeDir, "session-b", inner, time.Date(2026, 8, 13, 12, 5, 0, 0, time.UTC))

	for i, scan := range []string{"the first scan", "the second scan"} {
		// A new session for the second pass. Every id is derived from the source event
		// (ADR-0004), so re-reading what is already in the spool writes nothing and
		// EventsWritten counts the work done rather than the window — without a new
		// session the second scan would report an honest zero and the state word below
		// would be about that instead of about the refusal.
		transcriptAt(t, claudeDir, "session-"+string(rune('c'+i)), inner, time.Date(2026, 8, 13, 13, 0, i, 0, time.UTC))
		if _, err := Ingest(paths, claudeDir); err != nil {
			t.Fatalf("Ingest() on %s error = %v", scan, err)
		}
		counters := scanCounters(t, paths)
		if counters.BoundaryRefused != 1 {
			t.Errorf("BoundaryRefused after %s = %d, want 1", scan, counters.BoundaryRefused)
		}
		if counters.EventsWritten == 0 {
			t.Errorf("EventsWritten after %s = 0; the fixture is not collecting and the state assertion below means nothing", scan)
		}
		report, err := health.New(paths.HealthFile).Read()
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if got := health.Diagnose(report, nil, nil); got.State != health.StateCollecting {
			t.Errorf("Diagnose().State after %s = %q, want %q", scan, got.State, health.StateCollecting)
		}
	}
}

// The boundary encloses roots and is never one. `wake init -g` from the home
// directory is the common invocation, and registering that directory would enclose
// every repository the boundary later discovers — which ADR-0019 §5's nested-root
// refusal would then refuse, one by one, for every one of them.
func TestInitGlobalRegistersNoRootOfItsOwn(t *testing.T) {
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)

	if _, err := InitGlobal(paths, base, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("InitGlobal() error = %v", err)
	}

	if roots := recordedRoots(t, paths); len(roots) != 0 {
		t.Errorf("the table records %v, want no root at all", roots)
	}
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	if identity := mustIdentify(t, repos, base); identity.Matched {
		t.Errorf("Identify(the boundary) matched %s; the boundary is not a repository", identity.ID)
	}
}
