package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The install ADR-0047 was written for: a plain `wake init` in a repository, no
// collection boundary recorded at all, and a session run in a linked worktree of that
// repository. Every option that gated on a boundary could not reach this case, and it
// is the one where 223 of 1,422 transcripts went uncounted.
func TestClassificationNamesAnUnregisteredWorktreeOfAConsentedRepositoryWithNoBoundary(t *testing.T) {
	requireGit(t)
	main, worktree := initWorktree(t)
	repos := openRepos(t, testPaths(t))
	if _, err := repos.Register(main, "main", time.Time{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if got := repos.ClassifyUnresolvedDirectory(worktree); got != DirectoryUnregisteredWorktree {
		t.Errorf("ClassifyUnresolvedDirectory(the worktree) = %v, want DirectoryUnregisteredWorktree", got)
	}
}

// The same shape with a boundary recorded, so the disjunct consentsRepository reaches
// only under one is exercised too.
func TestClassificationNamesAnUnregisteredWorktreeUnderARecordedBoundary(t *testing.T) {
	requireGit(t)
	main, worktree := initWorktree(t)
	repos := openRepos(t, testPaths(t))
	if err := repos.SetGlobalRoot(filepath.Dir(main)); err != nil {
		t.Fatalf("SetGlobalRoot() error = %v", err)
	}
	if _, err := repos.Register(main, "main", time.Time{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if got := repos.ClassifyUnresolvedDirectory(worktree); got != DirectoryUnregisteredWorktree {
		t.Errorf("ClassifyUnresolvedDirectory(the worktree) = %v, want DirectoryUnregisteredWorktree", got)
	}
}

func TestClassificationNamesADirectoryThatIsNotARepository(t *testing.T) {
	requireGit(t)
	base := tempRealDir(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
	plain := mkdirAll(t, filepath.Join(base, "plain"))
	repos := openRepos(t, testPaths(t))

	if got := repos.ClassifyUnresolvedDirectory(plain); got != DirectoryNotARepository {
		t.Errorf("ClassifyUnresolvedDirectory(a plain directory) = %v, want DirectoryNotARepository", got)
	}
}

// Two shapes, one answer: a repository nobody consented, and a worktree whose main
// checkout nobody consented. Neither is the actionable population, and folding either
// into it would report lost collection where there is none.
func TestClassificationNamesARepositoryNobodyConsented(t *testing.T) {
	requireGit(t)
	t.Run("a repository with nothing registered", func(t *testing.T) {
		root, _ := initRepo(t)
		repos := openRepos(t, testPaths(t))

		if got := repos.ClassifyUnresolvedDirectory(root); got != DirectoryUnconsentedRepository {
			t.Errorf("ClassifyUnresolvedDirectory(an unconsented repository) = %v, want DirectoryUnconsentedRepository", got)
		}
	})
	t.Run("a worktree whose main checkout is not registered", func(t *testing.T) {
		_, worktree := initWorktree(t)
		repos := openRepos(t, testPaths(t))

		if got := repos.ClassifyUnresolvedDirectory(worktree); got != DirectoryUnconsentedRepository {
			t.Errorf("ClassifyUnresolvedDirectory(a worktree of an unconsented repository) = %v, want DirectoryUnconsentedRepository", got)
		}
	})
}

// A directory the table does match is not an unresolved one for want of consent: the
// user consented it, and the transcript was skipped by the collection window instead
// (ADR-0024, ADR-0025). It costs no git call, which is why the string question is
// asked first.
func TestClassificationNamesAConsentedDirectory(t *testing.T) {
	root, nested := initRepo(t)
	repos := openRepos(t, testPaths(t))
	if _, err := repos.Register(root, "repo", time.Time{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if got := repos.ClassifyUnresolvedDirectory(nested); got != DirectoryConsented {
		t.Errorf("ClassifyUnresolvedDirectory(a subdirectory of a consented root) = %v, want DirectoryConsented", got)
	}
}

// ADR-0047 §4: a directory that is gone gets no classification rather than the nearest
// bucket. git fails in a directory that is not there exactly as it fails in one that
// is not a repository, so the stat has to come first.
func TestClassificationLeavesAVanishedDirectoryUnclassified(t *testing.T) {
	repos := openRepos(t, testPaths(t))
	vanished := filepath.Join(tempRealDir(t), "never-created")

	got := repos.ClassifyUnresolvedDirectory(vanished)
	if got == DirectoryNotARepository {
		t.Fatalf("ClassifyUnresolvedDirectory(a vanished directory) = DirectoryNotARepository; a directory that is gone is not a directory that is not a repository")
	}
	if got != DirectoryUnclassified {
		t.Errorf("ClassifyUnresolvedDirectory(a vanished directory) = %v, want DirectoryUnclassified", got)
	}
}

// The other half of ADR-0047 §4: a probe that could not answer classifies nothing.
func TestClassificationLeavesADirectoryUnclassifiedWhenTheProbeCannotAnswer(t *testing.T) {
	requireGit(t)
	main, worktree := initWorktree(t)
	repos := openRepos(t, testPaths(t))
	if _, err := repos.Register(main, "main", time.Time{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	restore := gitCallTimeout
	gitCallTimeout = time.Nanosecond
	t.Cleanup(func() { gitCallTimeout = restore })

	if got := repos.ClassifyUnresolvedDirectory(worktree); got != DirectoryUnclassified {
		t.Errorf("ClassifyUnresolvedDirectory() = %v, want DirectoryUnclassified; the probe outlived its deadline", got)
	}
}

// ADR-0047 §2's own test. A number went up, and that is all that happened: nothing is
// registered, nothing is consented, and nothing is recorded — including about the
// directory that classified as a worktree of a consented repository, which is
// precisely the one a careless implementation would admit.
func TestClassificationRegistersNothingAndConsentsNothing(t *testing.T) {
	requireGit(t)
	main, worktree := initWorktree(t)
	paths := testPaths(t)
	repos := openRepos(t, paths)
	if _, err := repos.Register(main, "main", time.Time{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	before, err := os.ReadFile(paths.ProjectsFile)
	if err != nil {
		t.Fatalf("reading the project table: %v", err)
	}
	beforeDir := configDirNames(t, paths)

	for _, dir := range []string{worktree, mkdirAll(t, filepath.Join(tempRealDir(t), "plain")), filepath.Join(tempRealDir(t), "gone")} {
		repos.ClassifyUnresolvedDirectory(dir)
	}

	after, err := os.ReadFile(paths.ProjectsFile)
	if err != nil {
		t.Fatalf("re-reading the project table: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("classification rewrote the project table:\nbefore: %s\nafter:  %s", before, after)
	}
	if got := configDirNames(t, paths); len(got) != len(beforeDir) {
		t.Errorf("classification left %v in the config directory, want the %v it found", got, beforeDir)
	}
}

// configDirNames lists the config directory, so "recorded nothing" is asserted against
// the filesystem rather than against the one file the test happens to know about.
func configDirNames(t *testing.T, p Paths) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(p.ProjectsFile))
	if err != nil {
		t.Fatalf("reading the config directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// The layering rule for the shared probe, now that ADR-0047 §2 has widened who may
// read it: classification may, registration may, and the derivation path may not.
func TestTheSharedProbeIsNamedOnlyOffTheDerivationPath(t *testing.T) {
	assertSymbolNamedOnlyIn(t, "probeWorktree",
		"the shared worktree probe runs on the registration path and on classification's, never on the derivation path (ADR-0019 §1, ADR-0044 §4)",
		map[string]bool{
			"internal/config/root.go":          true,
			"internal/config/classify.go":      true,
			"internal/config/root_test.go":     true,
			"internal/config/classify_test.go": true,
		})
}

// What keeps a later ticket from reaching git out of Identify: classification is named
// where a scan's counters are assembled, and nowhere on the path that attributes an
// event to a repository (ADR-0019 §1).
func TestClassificationIsNamedOnlyOffTheDerivationPath(t *testing.T) {
	assertSymbolNamedOnlyIn(t, "ClassifyUnresolvedDirectory",
		"classification runs after a walk, beside the counters it explains, and never on the derivation path (ADR-0019 §1, ADR-0047 §3)",
		map[string]bool{
			"internal/config/classify.go":          true,
			"internal/config/classify_test.go":     true,
			"internal/activation/classify.go":      true,
			"internal/activation/classify_test.go": true,
		})
}
