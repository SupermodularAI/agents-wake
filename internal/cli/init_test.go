package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// Acceptance item 2, mechanically: `init`'s RunE starts no process and discovers no
// root itself.
//
// The test reads the source because "no logic lives here" is a property of the file,
// not of an output: a command that shelled out to git and printed the same thing
// would satisfy every behavioural assertion in this package. ADR-0001 and plan §6.2
// put every decision below this layer, and which directory gets consented is the
// decision `init` used to make here.
func TestInitCommandRunsNoProcessAndDiscoversNoRootItself(t *testing.T) {
	// A `go test` binary starts in its own package directory and no test in this
	// package calls t.Parallel(), so the relative path is stable.
	raw, err := os.ReadFile("init.go")
	if err != nil {
		t.Fatalf("reading init.go: %v", err)
	}
	for _, forbidden := range []string{"exec.Command", "os/exec", "rev-parse"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("init.go names %q; root discovery belongs in internal/config, and this layer only parses and prints", forbidden)
		}
	}
}

// Acceptance item 3, end to end: the behaviour the move has to preserve.
//
// Run from a subdirectory, `init` must still consent to the enclosing repository —
// consent is given for a repository, and a record of the subdirectory would make
// every later scan collect part of it and report a complete pass (ADR-0019 §1).
// This is also the assertion that fails if the delegated resolver silently fell back
// to the working directory.
func TestInitRegistersTheEnclosingRepositoryRootFromASubdirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	paths := isolate(t)

	// Symlink-resolved before git sees it: on darwin t.TempDir() sits behind
	// /var → /private/var, and both git and config.Register report the resolved
	// spelling.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temporary directory: %v", err)
	}
	repo := filepath.Join(base, "repo")
	nested := filepath.Join(repo, "nested")
	if mkErr := os.MkdirAll(nested, 0o700); mkErr != nil {
		t.Fatalf("creating %s: %v", nested, mkErr)
	}
	if output, initErr := exec.Command("git", "init", repo).CombinedOutput(); initErr != nil {
		t.Fatalf("git init: %v: %s", initErr, output)
	}
	t.Chdir(nested)

	out, err := run(t, "init")
	if err != nil {
		t.Fatalf("init from a repository subdirectory: %v; output:\n%s", err, out)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolving the working directory: %v", err)
	}
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	got, err := repos.ConsentedRoot(cwd)
	if err != nil {
		t.Fatalf("ConsentedRoot() error = %v", err)
	}
	if got != repo {
		t.Errorf("the consented root is %q, want the repository root %q", got, repo)
	}

	// The disclosure still precedes the result, and still names the files this run
	// will change (ADR-0010). The full five-path disclosure is covered by
	// claudedir_test.go; these keep this case honest about the ordering without
	// duplicating it.
	for _, want := range []string{
		paths.ConfigFile,
		filepath.Join(claudeHome(t), "settings.json"),
		"Claude Code collection enabled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init output is missing %q; got:\n%s", want, out)
		}
	}
}
