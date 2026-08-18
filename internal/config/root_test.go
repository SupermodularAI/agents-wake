package config

import (
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

// Acceptance item 1: the discovery is exercised by a function call, with no command
// constructed and no stream read.
func TestDiscoverRootForRegistrationReturnsTheRepositoryRootFromASubdirectory(t *testing.T) {
	requireGit(t)
	root, nested := initRepo(t)
	t.Chdir(nested)

	got, err := DiscoverRootForRegistration()
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
	got, err := DiscoverRootForRegistration()
	if err != nil {
		t.Fatalf("DiscoverRootForRegistration() error = %v", err)
	}
	if got != cwd {
		t.Errorf("DiscoverRootForRegistration() = %q, want the working directory %q unchanged", got, cwd)
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
		"internal/cli/init.go":         true,
		"internal/config/root.go":      true,
		"internal/config/root_test.go": true,
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
