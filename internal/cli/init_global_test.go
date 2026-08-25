package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// Acceptance item 1. The disclosure names every file this invocation writes and says,
// in words, that repositories will be registered under the boundary as sessions run in
// them — including repositories created later. ADR-0010 rests on that sentence
// reaching the user before anything is written.
//
// config.toml's absence afterwards is the other half of the same rule: a disclosure
// that names a file the command leaves alone is as wrong as one that omits a file it
// writes. `--global` records the boundary in the project table and writes no config
// key (ADR-0032 §2 against requirement 10).
func TestInitGlobalDisclosesEveryFileAndTheRegistrationBeforeWriting(t *testing.T) {
	paths := isolate(t)
	boundary := filepath.Join(t.TempDir(), "boundary")
	if err := os.MkdirAll(boundary, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	out, err := run(t, "init", "-g", boundary)
	if err != nil {
		t.Fatalf("init -g error = %v; output:\n%s", err, out)
	}

	for _, want := range []string{
		paths.SaltFile,
		paths.ProjectsFile,
		paths.PrimitivesFile,
		paths.HealthFile,
		filepath.Join(claudeHome(t), "settings.json"),
		boundary,
		"including repositories created later",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init -g output is missing %q; got:\n%s", want, out)
		}
	}
	disclosure := strings.Index(out, "Wake will modify:")
	confirmation := strings.Index(out, "Collection boundary recorded")
	if disclosure < 0 || confirmation < 0 || disclosure > confirmation {
		t.Errorf("the disclosure at %d does not precede the confirmation at %d:\n%s", disclosure, confirmation, out)
	}
	if _, statErr := os.Stat(paths.ConfigFile); !os.IsNotExist(statErr) {
		t.Errorf("Stat(config.toml) = %v; --global writes no config key, so the disclosure must not name it and the file must not appear", statErr)
	}
	if strings.Contains(out, paths.ConfigFile) {
		t.Errorf("the disclosure names config.toml, which --global leaves alone:\n%s", out)
	}
}

// `wake init -g` with no argument means the home directory, which is the invocation
// the feature exists for.
//
// The state is asserted rather than a string: SetGlobalRoot records the
// symlink-resolved spelling and the disclosure prints the one the user typed, so an
// assertion on the printed path would be about EvalSymlinks rather than about the
// default.
func TestInitGlobalDefaultsToTheHomeDirectory(t *testing.T) {
	paths := isolate(t)

	out, err := run(t, "init", "-g")
	if err != nil {
		t.Fatalf("init -g error = %v; output:\n%s", err, out)
	}

	state, err := config.GlobalRootStateFor(paths)
	if err != nil {
		t.Fatalf("GlobalRootStateFor() error = %v", err)
	}
	if !state.Set {
		t.Errorf("GlobalRootStateFor() = %+v, want a boundary recorded at the home directory", state)
	}
}

// Acceptance item 6. Plain `init` keeps cobra.NoArgs exactly: only --global takes a
// path, and widening the argument rule for both would let a typo consent a directory
// the user never meant.
func TestPlainInitStillRejectsAPositionalArgument(t *testing.T) {
	isolate(t)

	if out, err := run(t, "init", "somewhere"); err == nil {
		t.Errorf("init with a positional argument succeeded; output:\n%s", out)
	}
}

// The refusal names the requirement and never the path. A boundary is a directory of
// the user's, and this error reaches the same terminal as everything else (plan §4.2).
func TestInitRefusesARelativeGlobalPath(t *testing.T) {
	isolate(t)
	const relative = "unmistakable-relative-boundary"

	out, err := run(t, "init", "-g", filepath.Join(relative, "inner"))
	if err == nil {
		t.Fatalf("init -g with a relative path succeeded; output:\n%s", out)
	}
	message := err.Error()
	if strings.Contains(message, relative) {
		t.Errorf("the refusal %q echoes the path the user typed", message)
	}
	if strings.Contains(message, string(filepath.Separator)) {
		t.Errorf("the refusal %q carries a path separator", message)
	}
}

// One boundary at a time. The second run is in effect and the first is not, so a user
// who narrows the boundary is not left with the wider one still consented.
func TestInitGlobalTwiceReplacesTheBoundary(t *testing.T) {
	paths := isolate(t)
	// Symlink-resolved, so the negative assertion below is about the replacement
	// rather than about /var → /private/var: SetGlobalRoot records the resolved
	// spelling, and an unresolved one would fail to match for the wrong reason.
	base, resolveErr := filepath.EvalSymlinks(t.TempDir())
	if resolveErr != nil {
		t.Fatalf("resolving the temporary directory: %v", resolveErr)
	}
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}

	if out, err := run(t, "init", "-g", first); err != nil {
		t.Fatalf("first init -g error = %v; output:\n%s", err, out)
	}
	if out, err := run(t, "init", "-g", second); err != nil {
		t.Fatalf("second init -g error = %v; output:\n%s", err, out)
	}

	state, stateErr := config.GlobalRootStateFor(paths)
	if stateErr != nil {
		t.Fatalf("GlobalRootStateFor() error = %v", stateErr)
	}
	if state != (config.GlobalRootState{Set: true}) {
		t.Errorf("GlobalRootStateFor() = %+v, want exactly one boundary recorded", state)
	}
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	if repos.WithinGlobalRoot(filepath.Join(first, "project")) {
		t.Error("the replaced boundary is still in effect")
	}
}
