package activation

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
)

// readScan reads the counters back out of the counter file, so every assertion below
// is about what reached disk rather than about what a struct held in memory.
func readScan(t *testing.T, paths config.Paths) health.Scan {
	t.Helper()
	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("reading the counter file: %v", err)
	}
	return report.Scan
}

// The headline case, and the install ADR-0047 was written for: a plain `wake init` in
// a repository, no collection boundary recorded at all, and a session run in a linked
// worktree of that repository.
//
// Before this change the same fixture reported Skipped: 1 and nothing else. That is
// the blindness the ticket is about — the user consented the repository, ran sessions
// in its worktree, and the scan reported healthy while collecting none of them.
func TestAScanCountsATranscriptFromAnUnregisteredWorktreeOfAConsentedRepository(t *testing.T) {
	requireGit(t)
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	main, worktree := initWorktreeOutside(t, base)
	// No InitGlobal: the machine has no boundary, which is the shape every
	// boundary-gated option could not reach.
	if _, err := Init(paths, main, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-w", worktree, time.Now().UTC())

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	scan := readScan(t, paths)
	if scan.Skipped != 1 || scan.SkippedUnregisteredWorktree != 1 {
		t.Fatalf("Skipped = %d with SkippedUnregisteredWorktree = %d, want 1 and 1", scan.Skipped, scan.SkippedUnregisteredWorktree)
	}
	if scan.SkippedNotARepository != 0 || scan.SkippedUnconsentedRepository != 0 ||
		scan.SkippedOutsideCollectionWindow != 0 || scan.SkippedUnclassified != 0 || scan.SkippedNothingTerminal != 0 {
		t.Errorf("another reason claimed the transcript: %+v", scan)
	}
}

func TestAScanCountsATranscriptFromADirectoryThatIsNotARepository(t *testing.T) {
	requireGit(t)
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	// GIT_CEILING_DIRECTORIES so a developer whose temporary directory sits inside a
	// repository does not make git answer about that one.
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
	root := filepath.Join(base, "repo")
	plain := filepath.Join(base, "plain")
	for _, dir := range []string{root, plain} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	if _, err := Init(paths, root, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-p", plain, time.Now().UTC())

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	if scan := readScan(t, paths); scan.Skipped != 1 || scan.SkippedNotARepository != 1 {
		t.Errorf("Skipped = %d with SkippedNotARepository = %d, want 1 and 1: %+v", scan.Skipped, scan.SkippedNotARepository, scan)
	}
}

func TestAScanCountsATranscriptFromARepositoryNobodyConsented(t *testing.T) {
	requireGit(t)
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	main, _ := initWorktreeOutside(t, base)
	other := filepath.Join(realTempDir(t), "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	initRepoAt(t, other)
	if _, err := Init(paths, main, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-o", other, time.Now().UTC())

	if scan := scanOnce(t, paths, claudeDir); scan.Skipped != 1 || scan.SkippedUnconsentedRepository != 1 {
		t.Errorf("Skipped = %d with SkippedUnconsentedRepository = %d, want 1 and 1: %+v", scan.Skipped, scan.SkippedUnconsentedRepository, scan)
	}
}

// DG-110's population, now on a line of its own: the directory *is* consented, and the
// transcript predates the instant collection began for it (ADR-0024, ADR-0025). It is
// the one reason that is not about consent at all.
func TestAScanCountsATranscriptOutsideTheCollectionWindow(t *testing.T) {
	requireGit(t)
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	// A plain Init: forward-only, so the consent instant is now and the fixture below
	// is historical relative to it.
	if _, err := Init(paths, root, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-h", root, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))

	// The hook-fired scope, which is the one the window applies under: `wake ingest`
	// imports the whole history and would collect this transcript rather than skip it.
	if scan := hookScan(t, paths, claudeDir); scan.Skipped != 1 || scan.SkippedOutsideCollectionWindow != 1 {
		t.Errorf("Skipped = %d with SkippedOutsideCollectionWindow = %d, want 1 and 1: %+v", scan.Skipped, scan.SkippedOutsideCollectionWindow, scan)
	}
}

// The invariant the whole design rests on, over every population at once plus a
// directory deleted before the scan. A breakdown that does not sum to the number above
// it cannot be read, and this is what catches a future attribution bug.
func TestTheSkippedBreakdownSumsToTheSkippedCount(t *testing.T) {
	requireGit(t)
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
	main, worktree := initWorktreeOutside(t, base)
	plain := filepath.Join(base, "plain")
	unconsented := filepath.Join(realTempDir(t), "other")
	vanished := filepath.Join(realTempDir(t), "gone")
	for _, dir := range []string{plain, unconsented, vanished} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	initRepoAt(t, unconsented)
	if _, err := Init(paths, main, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	now := time.Now().UTC()
	transcriptAt(t, claudeDir, "session-w", worktree, now)
	transcriptAt(t, claudeDir, "session-p", plain, now)
	transcriptAt(t, claudeDir, "session-o", unconsented, now)
	transcriptAt(t, claudeDir, "session-g", vanished, now)
	transcriptAt(t, claudeDir, "session-h", main, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if err := os.RemoveAll(vanished); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}

	scan := hookScan(t, paths, claudeDir)

	sum := scan.SkippedNotARepository + scan.SkippedUnconsentedRepository + scan.SkippedUnregisteredWorktree +
		scan.SkippedOutsideCollectionWindow + scan.SkippedUnclassified + scan.SkippedNothingTerminal
	if !scan.SkippedClassified {
		t.Fatalf("SkippedClassified = false after a walk that finished: %+v", scan)
	}
	if sum != scan.Skipped {
		t.Fatalf("the reasons sum to %d for %d skipped transcripts: %+v", sum, scan.Skipped, scan)
	}
	if scan.SkippedUnregisteredWorktree != 1 || scan.SkippedNotARepository != 1 ||
		scan.SkippedUnconsentedRepository != 1 || scan.SkippedUnclassified != 1 ||
		scan.SkippedOutsideCollectionWindow != 1 {
		t.Errorf("the five populations did not land one each: %+v", scan)
	}
}

// ADR-0047 §2 end to end. A directory the scan classified as a worktree of a consented
// repository is still not consented and still not collected from: classification
// registers nothing, and a number going up is the whole of what it does.
func TestClassifyingASkippedTranscriptRegistersNothing(t *testing.T) {
	requireGit(t)
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	main, worktree := initWorktreeOutside(t, base)
	if _, err := Init(paths, main, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-w", worktree, time.Now().UTC())
	before, err := os.ReadFile(paths.ProjectsFile)
	if err != nil {
		t.Fatalf("reading the project table: %v", err)
	}
	beforeRoots := recordedRoots(t, paths)

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	if scan := readScan(t, paths); scan.SkippedUnregisteredWorktree != 1 {
		t.Fatalf("SkippedUnregisteredWorktree = %d, want 1; the fixture no longer exercises the case", scan.SkippedUnregisteredWorktree)
	}
	after, err := os.ReadFile(paths.ProjectsFile)
	if err != nil {
		t.Fatalf("re-reading the project table: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("classification rewrote the project table:\nbefore %s\nafter  %s", before, after)
	}
	if got := recordedRoots(t, paths); len(got) != len(beforeRoots) {
		t.Errorf("recorded roots = %v, want the %v the scan started with", got, beforeRoots)
	}
}

// The pair the ticket named: registration's silence and classification's number, side
// by side. A directory the second admission arm turns away is not a refusal — the
// boundary is working — and until now that was the end of the story.
func TestAScanUnderABoundaryStillClassifiesWhatRegistrationRefused(t *testing.T) {
	requireGit(t)
	paths := testPaths(t)
	claudeDir, base := boundaryFixture(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(base))
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	outside := filepath.Join(realTempDir(t), "plain")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if _, err := Init(paths, root, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := InitGlobal(paths, base, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("InitGlobal() error = %v", err)
	}
	transcriptAt(t, claudeDir, "session-x", outside, time.Now().UTC())

	scan := hookScan(t, paths, claudeDir)

	if scan.BoundaryRefused != 0 {
		t.Errorf("BoundaryRefused = %d, want 0; a directory the second arm does not admit is not a refusal", scan.BoundaryRefused)
	}
	if scan.SkippedNotARepository != 1 {
		t.Errorf("SkippedNotARepository = %d, want 1: %+v", scan.SkippedNotARepository, scan)
	}
}

// initRepoAt makes an existing directory a git repository, for the cases that need a
// repository nobody consented rather than a worktree of one.
func initRepoAt(t *testing.T, dir string) {
	t.Helper()
	if output, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", dir, err, output)
	}
}

// scanOnce runs the user-asked scan and reads the counters back off disk.
func scanOnce(t *testing.T, paths config.Paths, claudeDir string) health.Scan {
	t.Helper()
	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	return readScan(t, paths)
}

// hookScan runs the scan a hook fires — the one that honours each repository's
// recorded collection window (ADR-0024, ADR-0025) — and reads the counters back.
func hookScan(t *testing.T, paths config.Paths, claudeDir string) health.Scan {
	t.Helper()
	if _, err := Trigger(paths, claudeDir); err != nil {
		t.Fatalf("Trigger() error = %v", err)
	}
	return readScan(t, paths)
}
