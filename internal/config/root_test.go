package config

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireGit skips a case that needs the real tool ADR-0019 §1 names.
//
// This is the one legitimate skip in this package's boundary-shaped tests: the two
// cases below assert what `git rev-parse --show-toplevel` answers, which is a
// property of an external program rather than of this tree. boundary_test.go's scans
// must never skip for the opposite reason — they assert a property of the source, so
// a skip there would be indistinguishable from a pass. It also neutralises the
// developer's own git configuration, so what `git init` produces here cannot depend
// on the machine the test runs on.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
}

// initRepo creates a git repository with one subdirectory and returns both, already
// symlink-resolved: on darwin t.TempDir() sits behind /var → /private/var, and both
// git and Register report the resolved spelling.
func initRepo(t *testing.T) (root, nested string) {
	t.Helper()
	root = mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	nested = mkdirAll(t, filepath.Join(root, "nested"))
	if output, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return root, nested
}

// initWorktree creates a git repository with one commit and one linked worktree,
// and returns both roots already symlink-resolved.
//
// The worktree is a sibling of the main checkout, never inside it: a worktree under
// the main root would be inside a consented root, which Register refuses as nesting
// (ADR-0019 §5), and it is not the shape the ticket is about.
//
// The commit identity is supplied per invocation because requireGit points
// GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM at os.DevNull, so there is no configured
// name to commit under — and `git worktree add` needs a commit to check out.
func initWorktree(t *testing.T) (main, worktree string) {
	t.Helper()
	base := tempRealDir(t)
	main = mkdirAll(t, filepath.Join(base, "main"))
	worktree = filepath.Join(base, "worktree")
	for _, step := range [][]string{
		{"init", main},
		{"-C", main, "-c", "user.email=wake@example.invalid", "-c", "user.name=wake", "commit", "--allow-empty", "-m", "init"},
		{"-C", main, "worktree", "add", worktree},
	} {
		if output, err := exec.Command("git", step...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", step, err, output)
		}
	}
	return main, worktree
}

// The lookup DG-104 adds: from inside a linked worktree, the repository it belongs
// to. It is what lets `wake init` in a worktree record the relation without the
// worktree ceasing to be its own repository (ADR-0019 §6).
func TestTheParentLookupFindsTheMainCheckoutFromALinkedWorktree(t *testing.T) {
	requireGit(t)
	main, worktree := initWorktree(t)

	got := discoverParentRepositoryForRegistration(worktree)
	if len(got) == 0 {
		t.Fatalf("discoverParentRepositoryForRegistration(%q) = nil, want the main checkout %q", worktree, main)
	}
	if got[0] != main {
		t.Errorf("discoverParentRepositoryForRegistration() = %q, want the main checkout %q first", got, main)
	}
}

// A main checkout is not a worktree of anything. Answering with itself would be a
// self-relation, which valid() refuses — and rightly, since every reader would then
// have to defend against the cycle.
func TestTheParentLookupAnswersNothingInAMainCheckout(t *testing.T) {
	requireGit(t)
	root, _ := initRepo(t)

	if got := discoverParentRepositoryForRegistration(root); got != nil {
		t.Errorf("discoverParentRepositoryForRegistration(%q) = %q, want nil", root, got)
	}
}

// A plain directory is a repository Wake will happily consent (ADR-0019 §5) and a
// worktree of nothing. Every git failure direction answers nil, which is the
// pre-change behaviour.
func TestTheParentLookupAnswersNothingOutsideAGitRepository(t *testing.T) {
	requireGit(t)
	dir := mkdirAll(t, filepath.Join(tempRealDir(t), "plain"))

	if got := discoverParentRepositoryForRegistration(dir); got != nil {
		t.Errorf("discoverParentRepositoryForRegistration(%q) = %q, want nil", dir, got)
	}
}

// GIT_DIR and GIT_WORK_TREE are dropped for the reason boundedDiscoveryEnv drops
// them: the hook-fired registration path inherits a session's environment, and an
// exported GIT_DIR would otherwise make a main checkout look like a worktree of
// whatever it names — or point a worktree at a common directory nowhere near it.
func TestTheParentLookupIgnoresAnInheritedGitDir(t *testing.T) {
	requireGit(t)
	main, worktree := initWorktree(t)
	elsewhere, _ := initRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(elsewhere, ".git"))
	t.Setenv("GIT_WORK_TREE", elsewhere)

	if got := discoverParentRepositoryForRegistration(main); got != nil {
		t.Errorf("discoverParentRepositoryForRegistration(%q) = %q, want nil; the environment made a main checkout look like a worktree", main, got)
	}
	got := discoverParentRepositoryForRegistration(worktree)
	if len(got) == 0 || got[0] != main {
		t.Errorf("discoverParentRepositoryForRegistration(%q) = %q, want the real main checkout %q", worktree, got, main)
	}
}

// The mechanical half of the layering rule, for the reason
// TestDiscoverRootForRegistrationIsNamedOnlyOnInitsPath gives: ADR-0019 §1 makes
// derivation a pure string operation over the recorded snapshot, and a lookup that
// shelled out from the derivation path would attribute the same event differently
// depending on what the working tree looked like at the time.
//
// The function is unexported, so no package outside this one can reach it at all;
// this keeps the in-package call sites to the registration step.
func TestTheParentLookupIsNamedOnlyOnTheRegistrationPath(t *testing.T) {
	root := moduleRoot(t)
	allowed := map[string]bool{
		"internal/config/root.go":      true,
		"internal/config/identity.go":  true,
		"internal/config/root_test.go": true,
	}
	const symbol = "discoverParentRepositoryForRegistration"
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		scanned++
		if allowed[relative] {
			return nil
		}

		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(raw), symbol) {
			t.Errorf("%s names %s; the parent lookup runs only on `wake init`'s registration step, never on the derivation path", relative, symbol)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if scanned == 0 {
		t.Fatal("the walk scanned no Go file; the check proved nothing")
	}
}

// Acceptance item 1: the discovery is exercised by a function call, with no command
// constructed and no stream read.
func TestDiscoverRootForRegistrationReturnsTheRepositoryRootFromASubdirectory(t *testing.T) {
	requireGit(t)
	root, nested := initRepo(t)
	t.Chdir(nested)

	got, err := DiscoverRootForRegistration("", "")
	if err != nil {
		t.Fatalf("DiscoverRootForRegistration() error = %v", err)
	}
	if got != root {
		t.Errorf("DiscoverRootForRegistration() = %q, want the repository root %q", got, root)
	}
	// The second assertion is what proves discovery ran: the fallback returns the
	// working directory, so a resolver that never reached git would satisfy the
	// first check on a repository whose root happened to be the cwd.
	if got == nested {
		t.Errorf("DiscoverRootForRegistration() = %q, the subdirectory it ran in; the git discovery did not happen", got)
	}
}

// ADR-0019 §5: a directory that is not a git repository is accepted as its own root.
// That is why a git failure is a fallback rather than an error, and this case must
// hold whether or not git is installed at all.
func TestDiscoverRootForRegistrationAcceptsANonRepositoryDirectoryAsItsOwnRoot(t *testing.T) {
	dir := tempRealDir(t)
	t.Chdir(dir)
	// A ceiling stops git from walking above the temporary directory, so the case
	// asserts the non-repository answer even on a machine whose TMPDIR happens to
	// sit inside a checkout.
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolving the working directory: %v", err)
	}
	got, err := DiscoverRootForRegistration("", "")
	if err != nil {
		t.Fatalf("DiscoverRootForRegistration() error = %v", err)
	}
	if got != cwd {
		t.Errorf("DiscoverRootForRegistration() = %q, want the working directory %q unchanged", got, cwd)
	}
}

// A directory that is gone is refused rather than invented as its own root.
//
// The git fallback exists for a directory that is not a repository, not for one that
// is not there: returning it would record consent for a path nothing can be read
// from, and every later scan would report a complete pass over nothing. The returned
// root has to be empty as well as the error non-nil — a caller that ignores the error
// must not receive the vanished path either.
func TestDiscoverRootForRegistrationRefusesADirectoryThatIsGone(t *testing.T) {
	gone := mkdirAll(t, filepath.Join(tempRealDir(t), "gone"))
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("removing %s: %v", gone, err)
	}

	got, err := DiscoverRootForRegistration(gone, "")
	if !errors.Is(err, errRootNotADirectory) {
		t.Errorf("DiscoverRootForRegistration(a vanished directory) error = %v, want errRootNotADirectory", err)
	}
	if got != "" {
		t.Errorf("DiscoverRootForRegistration(a vanished directory) = %q, want no root at all", got)
	}
}

// The ceiling is what bounds discovery under a recorded collection boundary: a
// toplevel at or above the boundary is unreachable, so the ingested directory becomes
// its own root instead (ADR-0019 §5's plain-directory case).
//
// git's own semantics are the load-bearing part — the starting directory is always
// searched and only the upward walk stops — so the case is written against the real
// tool rather than a stub.
func TestDiscoverRootForRegistrationStopsAtTheCeiling(t *testing.T) {
	requireGit(t)
	base := tempRealDir(t)
	if output, err := exec.Command("git", "init", base).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	ceiling := mkdirAll(t, filepath.Join(base, "a"))
	dir := mkdirAll(t, filepath.Join(ceiling, "b"))

	got, err := DiscoverRootForRegistration(dir, ceiling)
	if err != nil {
		t.Fatalf("DiscoverRootForRegistration() error = %v", err)
	}
	if got != dir {
		t.Errorf("DiscoverRootForRegistration() = %q, want the directory itself %q", got, dir)
	}
	if got == base {
		t.Errorf("DiscoverRootForRegistration() = %q, the repository above the ceiling; the walk was not bounded", got)
	}
}

// A ceiling is not the whole boundary on its own. git documents that it "will not
// exclude ... a GIT_DIR set on the command line or in the environment", and the
// bounded call is the unattended one: the scan a hook fires inherits the session's
// environment, so a shell that exported GIT_DIR would make every discovery under the
// boundary answer with a repository nowhere near it. Verified against git 2.50.1,
// which answers with GIT_WORK_TREE.
//
// Dropped only where a ceiling is given. With no ceiling there is no boundary to
// escape and the directory the user is standing in is the one they are consenting, so
// plain `wake init` keeps honouring the environment exactly as before.
func TestDiscoverRootForRegistrationIgnoresAnInheritedGitDirWhenBounded(t *testing.T) {
	requireGit(t)
	base := tempRealDir(t)
	if output, err := exec.Command("git", "init", base).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	ceiling := mkdirAll(t, filepath.Join(base, "a"))
	dir := mkdirAll(t, filepath.Join(ceiling, "b"))
	elsewhere := tempRealDir(t)
	if output, err := exec.Command("git", "init", elsewhere).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	t.Setenv("GIT_DIR", filepath.Join(elsewhere, ".git"))
	t.Setenv("GIT_WORK_TREE", elsewhere)

	got, err := DiscoverRootForRegistration(dir, ceiling)
	if err != nil {
		t.Fatalf("DiscoverRootForRegistration() error = %v", err)
	}
	if got != dir {
		t.Errorf("DiscoverRootForRegistration() = %q, want the directory itself %q; the environment redirected the bounded walk", got, dir)
	}
}

// The mechanical half of the layering rule this resolver has to keep.
//
// ADR-0019 §1 allows git at registration and forbids it on the derivation path:
// Identify and ConsentedRoot are pure string operations over the snapshot, and §9
// says `init` is the only operation that discovers a root. A function that can be
// called from anywhere would let a later ticket reach the git call from ingest and
// still pass every other test, so the guarantee is the set of files allowed to name
// it — not a hand review of call sites.
func TestDiscoverRootForRegistrationIsNamedOnlyOnInitsPath(t *testing.T) {
	root := moduleRoot(t)
	allowed := map[string]bool{
		"internal/cli/init.go":    true,
		"internal/config/root.go": true,
		// The one entry ADR-0032 §2 adds: a directory discovered under a recorded
		// collection boundary reaches this function through
		// RegisterUnderGlobalRoot, which is registration and not derivation.
		"internal/config/globalroot.go": true,
		"internal/config/root_test.go":  true,
	}
	const symbol = "DiscoverRootForRegistration"
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing under a dot-directory is built into the binary, and .agent/
			// holds pipeline artifacts that legitimately discuss this function in
			// prose.
			if name := d.Name(); path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		scanned++
		if allowed[relative] {
			return nil
		}

		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(raw), symbol) {
			t.Errorf("%s names %s; root discovery runs only on `wake init`'s registration step, never on the derivation path", relative, symbol)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if scanned == 0 {
		t.Fatal("the walk scanned no Go file; the check proved nothing")
	}
}
