package config

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The default case, which is the whole of acceptance criterion 3: the one
// resolver returns byte-identically what the six call sites each used to build by
// hand.
func TestClaudeCodeDirIsTheHomeRelativeHarnessDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvDataDir, "")

	dir, err := ClaudeCodeDir()
	if err != nil {
		t.Fatalf("ClaudeCodeDir() error = %v", err)
	}
	if want := filepath.Join(home, ".claude"); dir != want {
		t.Errorf("ClaudeCodeDir() = %q, want %q", dir, want)
	}
}

// WAKE_DIR moves the data root wake owns and nothing that belongs to the harness
// (ADR-0014). A resolver that honoured it would point every command at a
// directory Claude Code never writes, and each one would then report collecting
// zero rather than collecting nothing.
func TestWakeDirDoesNotMoveTheHarnessDirectory(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvDataDir, elsewhere)

	dir, err := ClaudeCodeDir()
	if err != nil {
		t.Fatalf("ClaudeCodeDir() error = %v", err)
	}
	if want := filepath.Join(home, ".claude"); dir != want {
		t.Errorf("ClaudeCodeDir() = %q, want %q — %s moves wake's data root only", dir, want, EnvDataDir)
	}
	if strings.HasPrefix(dir, elsewhere) {
		t.Errorf("ClaudeCodeDir() = %q moved into the data root; the harness's directory is not wake's to relocate", dir)
	}
}

// Relocating the harness's directory is out of scope and would need a decision of
// its own (ADR-0014), so the absence of every candidate variable is asserted
// rather than assumed. Mirrors TestNoSecondEnvOverrideIsHonoured.
func TestNoEnvOverrideRelocatesTheHarnessDirectory(t *testing.T) {
	home := t.TempDir()
	decoy := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvDataDir, "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(decoy, "config"))
	t.Setenv("CLAUDE_HOME", filepath.Join(decoy, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(decoy, "xdg"))

	dir, err := ClaudeCodeDir()
	if err != nil {
		t.Fatalf("ClaudeCodeDir() error = %v", err)
	}
	if want := filepath.Join(home, ".claude"); dir != want {
		t.Errorf("ClaudeCodeDir() = %q, want %q — an environment override was honoured", dir, want)
	}
}

// Resolving where the harness keeps its files is not the same as creating them.
// Mirrors TestResolvePathsCreatesNothing: a `wake report` in a home with no
// Claude Code install must leave that home exactly as it found it.
func TestClaudeCodeDirCreatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := ClaudeCodeDir()
	if err != nil {
		t.Fatalf("ClaudeCodeDir() error = %v", err)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("reading HOME: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ClaudeCodeDir() created %d entries under HOME, want none", len(entries))
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("os.Lstat(%q) = %v, want ErrNotExist — ClaudeCodeDir must create nothing", dir, err)
	}
}

// plan §4.2: never write raw content anywhere, error messages included. The
// message names the home directory as a concept, the way ResolvePaths' wrap does,
// and carries no path — not the home directory's value, and not the directory
// under it this function would have returned.
func TestClaudeCodeDirErrorNamesNoPath(t *testing.T) {
	t.Setenv("HOME", "")

	dir, err := ClaudeCodeDir()
	if err == nil {
		t.Fatalf("ClaudeCodeDir() = (%q, nil), want an error with no HOME set", dir)
	}
	message := err.Error()
	if !strings.Contains(message, "resolving the home directory") {
		t.Errorf("error = %q, want it to name what it was resolving", message)
	}
	for _, forbidden := range []string{".claude", string(filepath.Separator)} {
		if strings.Contains(message, forbidden) {
			t.Errorf("error = %q carries %q; it names the home directory, never a path", message, forbidden)
		}
	}
}

// harnessDirUse is what one Go source file does about the harness's directory:
// whether it spells the directory's name in a string, and whether it resolves the
// home directory. A file doing both is constructing ~/.claude for itself.
type harnessDirUse struct {
	namesDir     bool
	resolvesHome bool
}

// scanHarnessDirUse reads one file's *parsed* source rather than its bytes.
//
// The distinction is load-bearing: paths.go's own doc comment explains that
// claudeSettingsLockName guards ~/.claude/settings.json, and lock.go's says much
// the same. Prose about where the harness keeps its files is not resolution, and a
// byte-level check would have to be told to ignore both files by name — which is a
// standing invitation to add a third exception rather than to fix a third
// resolver. Neither a string literal nor an identifier can appear inside a
// comment, so scanning the syntax tree needs no such exception, and cannot be
// fooled by a "//" inside a string either.
func scanHarnessDirUse(t *testing.T, path string) harnessDirUse {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var use harnessDirUse
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			// An unquote failure means a literal this check cannot read, which is not
			// the same as one it has cleared, so the raw form is searched instead.
			value, unquoteErr := strconv.Unquote(n.Value)
			if unquoteErr != nil {
				value = n.Value
			}
			if strings.Contains(value, claudeCodeDirName) {
				use.namesDir = true
			}
		case *ast.Ident:
			if n.Name == "UserHomeDir" {
				use.resolvesHome = true
			}
		}
		return true
	})
	return use
}

// walkGoSources visits every .go file under root, skipping dot-directories, and
// fails rather than passes if it found none: a boundary check that scanned nothing
// is indistinguishable in CI output from one that held.
func walkGoSources(t *testing.T, root string, visit func(relative, path string)) {
	t.Helper()
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing under a dot-directory is built into the binary, and .agent/
			// holds pipeline artifacts that legitimately discuss this in prose.
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
		scanned++
		visit(relative, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("the walk scanned no Go file under %s; the check proved nothing", root)
	}
}

// Acceptance criterion 1, mechanically: exactly one function resolves the Claude
// Code directory, and grepping the codebase finds no second construction of it.
//
// The rule is co-occurrence rather than either name alone, and that is what makes
// it precise enough to be worth keeping. inventory/claudecode.go builds .claude
// under a *consented repository root* rather than under $HOME, and so do two test
// fixtures; routing those through this resolver would blur the consent boundary,
// so they name the directory, resolve no home, and correctly pass. paths.go
// resolves the home directory and names no directory of the harness's, and
// correctly passes too.
func TestOnlyOneFunctionResolvesTheClaudeCodeDirectory(t *testing.T) {
	root := moduleRoot(t)
	// The resolver, and the tests of the resolver. Two exact files rather than the
	// whole package: a second resolver appearing next to this one is precisely what
	// is being prevented, so the rest of internal/config is not exempt.
	allowed := map[string]bool{
		filepath.Join(packageDir, "harness.go"):      true,
		filepath.Join(packageDir, "harness_test.go"): true,
	}

	walkGoSources(t, root, func(relative, path string) {
		if allowed[relative] {
			return
		}
		if use := scanHarnessDirUse(t, path); use.namesDir && use.resolvesHome {
			t.Errorf("%s resolves the home directory and names the harness's directory itself; call config.ClaudeCodeDir instead", relative)
		}
	})
}

// The ADR-0001 half of the same criterion: internal/cli only parses and prints, so
// nothing under it resolves anything — not through a join this check would
// recognise, and not through one it would not.
//
// Stated as "no home resolution at all" rather than as co-occurrence on purpose. A
// call site that resolved the home directory and then joined a name assembled out
// of pieces would slip past the co-occurrence rule, and this is the one directory
// where the stronger claim is true and worth pinning.
func TestInternalCliResolvesNoHomeDirectory(t *testing.T) {
	cliDir := filepath.Join(moduleRoot(t), "internal", "cli")

	walkGoSources(t, cliDir, func(relative, path string) {
		if scanHarnessDirUse(t, path).resolvesHome {
			t.Errorf("internal/cli/%s resolves the home directory; resolution lives below internal/cli, in config.ClaudeCodeDir (ADR-0001)", relative)
		}
	})
}
