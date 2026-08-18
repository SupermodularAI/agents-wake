package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
