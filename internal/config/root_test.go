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

// Every variable that re-points where git looks is dropped, for the reason
// boundedDiscoveryEnv drops them: the hook-fired registration path inherits a
// session's environment, and any of them would otherwise make a main checkout look
// like a worktree of whatever they name — or point a worktree at a common directory
// nowhere near it. GIT_COMMON_DIR is the one this lookup is most directly exposed to,
// since --git-common-dir reports it verbatim.
func TestTheParentLookupIgnoresAnInheritedGitDir(t *testing.T) {
	requireGit(t)
	main, worktree := initWorktree(t)
	elsewhere, _ := initRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(elsewhere, ".git"))
	t.Setenv("GIT_WORK_TREE", elsewhere)
	t.Setenv("GIT_COMMON_DIR", filepath.Join(elsewhere, ".git"))

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

// The probe DG-113 adds, from a subdirectory of a linked worktree. Both answers
// matter: the top level is what the ceiling is derived from and what the root git
// discovers under that ceiling is checked against, and the parent spellings are what
// the caller matches against the recorded table before anything is admitted
// (ADR-0044 §2).
func TestTheWorktreeProbeFindsTheTopLevelAndTheRepositoryFromASubdirectory(t *testing.T) {
	requireGit(t)
	main, worktree := initWorktree(t)
	sub := mkdirAll(t, filepath.Join(worktree, "sub"))

	topLevel, parent := discoverLinkedWorktreeForRegistration(sub)
	if topLevel != worktree {
		t.Errorf("discoverLinkedWorktreeForRegistration(%q) top level = %q, want the worktree %q", sub, topLevel, worktree)
	}
	if len(parent) == 0 || parent[0] != main {
		t.Errorf("discoverLinkedWorktreeForRegistration(%q) parent = %q, want the main checkout %q first", sub, parent, main)
	}
}

// A main checkout's subdirectory is not a worktree, and it is the case the relative
// `--git-common-dir` join exists for: git answers `../.git` from `main/sub`, so
// joining against the directory git was asked in and comparing the result against the
// *top level* is what tells the two apart. Comparing against the asked-in directory
// would read every main checkout's subdirectory as a worktree of the checkout itself.
func TestTheWorktreeProbeAnswersNothingInAMainCheckoutSubdirectory(t *testing.T) {
	requireGit(t)
	_, nested := initRepo(t)

	topLevel, parent := discoverLinkedWorktreeForRegistration(nested)
	if topLevel != "" || parent != nil {
		t.Errorf("discoverLinkedWorktreeForRegistration(%q) = (%q, %q), want nothing", nested, topLevel, parent)
	}
}

// A plain directory is a repository wake will happily consent (ADR-0019 §5) and a
// worktree of nothing. Every git failure direction answers nothing, which is the
// fail-closed answer: the caller then refuses the directory.
func TestTheWorktreeProbeAnswersNothingOutsideAGitRepository(t *testing.T) {
	requireGit(t)
	dir := mkdirAll(t, filepath.Join(tempRealDir(t), "plain"))

	topLevel, parent := discoverLinkedWorktreeForRegistration(dir)
	if topLevel != "" || parent != nil {
		t.Errorf("discoverLinkedWorktreeForRegistration(%q) = (%q, %q), want nothing", dir, topLevel, parent)
	}
}

// Every variable that re-points where git looks is dropped for the reason
// scrubbedGitEnv gives: the registration a hook fires inherits a session's
// environment, and any of them would otherwise make git answer about the repository
// they name rather than the directory being asked about — which here would admit a
// directory nobody consented. GIT_COMMON_DIR is the sharpest of the three: it leaves
// --show-toplevel alone, so every check made against the discovered root still passes
// while the parent the answer names is entirely the environment's.
//
// The enumeration is not the contract —
// TestScrubbedGitEnvDropsEveryGitVariableItDoesNotNameSafe is. These three are here
// because this is the call whose answer decides admission.
func TestTheWorktreeProbeIgnoresAnInheritedGitDir(t *testing.T) {
	requireGit(t)
	main, worktree := initWorktree(t)
	elsewhere, _ := initRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(elsewhere, ".git"))
	t.Setenv("GIT_WORK_TREE", elsewhere)
	t.Setenv("GIT_COMMON_DIR", filepath.Join(elsewhere, ".git"))

	if topLevel, parent := discoverLinkedWorktreeForRegistration(main); topLevel != "" || parent != nil {
		t.Errorf("discoverLinkedWorktreeForRegistration(%q) = (%q, %q), want nothing; the environment made a main checkout look like a worktree", main, topLevel, parent)
	}
	topLevel, parent := discoverLinkedWorktreeForRegistration(worktree)
	if topLevel != worktree {
		t.Errorf("discoverLinkedWorktreeForRegistration(%q) top level = %q, want its own %q", worktree, topLevel, worktree)
	}
	if len(parent) == 0 || parent[0] != main {
		t.Errorf("discoverLinkedWorktreeForRegistration(%q) parent = %q, want the real main checkout %q", worktree, parent, main)
	}
}

// The mechanical half of the layering rule, for the reason
// TestTheParentLookupIsNamedOnlyOnTheRegistrationPath gives: ADR-0019 §1 makes
// derivation a pure string operation over the recorded snapshot, and a probe that
// shelled out from the derivation path would attribute the same event differently
// depending on what the working tree looked like at the time.
//
// globalroot.go is in the allowed set because it holds ADR-0032 §2's second
// registration call site, which ADR-0044 §1 gives a second admission arm. That is a
// registration-path file, never a derivation-path one.
func TestTheWorktreeProbeIsNamedOnlyOnTheRegistrationPath(t *testing.T) {
	root := moduleRoot(t)
	allowed := map[string]bool{
		"internal/config/root.go":       true,
		"internal/config/globalroot.go": true,
		"internal/config/root_test.go":  true,
	}
	const symbol = "discoverLinkedWorktreeForRegistration"
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
			t.Errorf("%s names %s; the worktree probe runs only on the registration path, never on the derivation path", relative, symbol)
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

// The off-by-one registerLinkedWorktree depends on, asserted where it is a property
// of git rather than of this tree.
//
// GIT_CEILING_DIRECTORIES names directories git will not chdir up *into*, so a
// ceiling at a top level stops the walk one directory short of it and the starting
// directory becomes its own root. ADR-0044 §2's "may not walk above the worktree's
// own top level" is therefore spelled filepath.Dir(topLevel), and this pair is what
// would catch a later simplification to topLevel.
func TestDiscoverRootForRegistrationReachesATopLevelUnderItsParentCeilingButNotUnderItsOwn(t *testing.T) {
	requireGit(t)
	root, nested := initRepo(t)

	got, err := DiscoverRootForRegistration(nested, filepath.Dir(root))
	if err != nil {
		t.Fatalf("DiscoverRootForRegistration(%q, the top level's parent) error = %v", nested, err)
	}
	if got != root {
		t.Errorf("DiscoverRootForRegistration(%q, the top level's parent) = %q, want the top level %q", nested, got, root)
	}

	got, err = DiscoverRootForRegistration(nested, root)
	if err != nil {
		t.Fatalf("DiscoverRootForRegistration(%q, the top level itself) error = %v", nested, err)
	}
	if got != nested {
		t.Errorf("DiscoverRootForRegistration(%q, the top level itself) = %q, want the directory itself %q; a ceiling at the top level stops the walk one directory short", nested, got, nested)
	}
}

// The rule scrubbedGitEnv enforces is a property, not a list: a git call this package
// makes must not inherit anything that re-points where git looks. Enumerating the
// variables that do is how GIT_COMMON_DIR was missed once already, so the guarantee is
// the other way round — everything named GIT_ is dropped unless it is one of the three
// this package has a stated reason to keep — and this case pins that shape by including
// a variable that does not exist. A future git that adds a fourth location variable is
// then a refusal rather than an admission.
//
// GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM are kept because requireGit points them at
// os.DevNull: dropping them would make what git answers here depend on the developer's
// own configuration. Neither can re-point a working tree — git honours core.worktree
// only from a repository's own config (verified on git 2.50.1). GIT_CEILING_DIRECTORIES
// is kept because it can only make git find less, which is the fail-closed direction,
// and boundedDiscoveryEnv appends its own after it.
func TestScrubbedGitEnvDropsEveryGitVariableItDoesNotNameSafe(t *testing.T) {
	dropped := []string{
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_COMMON_DIR",
		"GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0",
		"GIT_NAMESPACE",
		"GIT_NOT_A_REAL_VARIABLE_YET",
	}
	kept := []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CEILING_DIRECTORIES"}
	for _, name := range append(append([]string{}, dropped...), kept...) {
		t.Setenv(name, "/somewhere/"+name)
	}
	t.Setenv("WAKE_TEST_UNRELATED", "kept")

	got := map[string]string{}
	for _, entry := range scrubbedGitEnv() {
		name, value, _ := strings.Cut(entry, "=")
		got[name] = value
	}

	for _, name := range dropped {
		if value, ok := got[name]; ok {
			t.Errorf("scrubbedGitEnv() kept %s=%q; a variable git reads for where the repository is must not be inherited", name, value)
		}
	}
	for _, name := range kept {
		if _, ok := got[name]; !ok {
			t.Errorf("scrubbedGitEnv() dropped %s, which this package has a stated reason to keep", name)
		}
	}
	if got["WAKE_TEST_UNRELATED"] != "kept" {
		t.Errorf("scrubbedGitEnv() dropped an unrelated variable; only git's own are in scope")
	}
}
