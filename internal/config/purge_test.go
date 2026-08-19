package config

import (
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedConfigRoot fills the config root with everything RemoveConfigRoot has to take
// with it: the identity salt (written by OpenRepos) and config.toml (written by Set).
func seedConfigRoot(t *testing.T, p Paths) {
	t.Helper()
	if _, err := OpenRepos(p); err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	if _, err := Set(p, "ui.default_window", "7d"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	for _, path := range []string{p.SaltFile, p.ConfigFile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("seeding %s: %v", path, err)
		}
	}
}

func TestRemoveConfigRootRemovesTheSaltAndTheConfigFile(t *testing.T) {
	p := testPaths(t)
	seedConfigRoot(t, p)

	if err := RemoveConfigRoot(p); err != nil {
		t.Fatalf("RemoveConfigRoot() error = %v", err)
	}

	if _, err := os.Stat(p.ConfigDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(ConfigDir) error = %v, want fs.ErrNotExist", err)
	}
}

// The removal is one root, not both. `wake uninstall` deletes the data root through
// activation.Uninstall's purge, and this function must not quietly do it too.
func TestRemoveConfigRootKeepsTheDataRoot(t *testing.T) {
	p := testPaths(t)
	seedConfigRoot(t, p)
	repos, err := OpenRepos(p)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	root := t.TempDir()
	if _, err := repos.Register(root, "fixture"); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := os.Stat(p.ProjectsFile); err != nil {
		t.Fatalf("seeding the project table: %v", err)
	}

	if err := RemoveConfigRoot(p); err != nil {
		t.Fatalf("RemoveConfigRoot() error = %v", err)
	}

	if _, err := os.Stat(p.DataDir); err != nil {
		t.Errorf("Stat(DataDir) error = %v, want the data root left in place", err)
	}
	if _, err := os.Stat(p.ProjectsFile); err != nil {
		t.Errorf("Stat(the project table) error = %v, want it left in place", err)
	}
}

// `uninstall` on a machine that was never `init`ed has nothing here to remove, and a
// re-run after a partial failure must converge rather than report a fault for work
// that is already done.
func TestRemoveConfigRootOnAMissingConfigRootIsNotAnError(t *testing.T) {
	p := testPaths(t)

	for attempt := 1; attempt <= 2; attempt++ {
		if err := RemoveConfigRoot(p); err != nil {
			t.Errorf("RemoveConfigRoot() attempt %d error = %v, want nil", attempt, err)
		}
	}
}

// The salt's bytes are the secret this package exists to confine; an error message
// is exactly where that kind of promise usually leaks.
func TestRemoveConfigRootErrorNamesTheConfigRootAndNotTheSaltBytes(t *testing.T) {
	p := testPaths(t)
	seedConfigRoot(t, p)
	salt, err := os.ReadFile(p.SaltFile)
	if err != nil {
		t.Fatalf("reading the salt: %v", err)
	}

	parent := filepath.Dir(p.ConfigDir)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("Chmod(%s) error = %v", parent, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Errorf("restoring %s: %v", parent, err)
		}
	})

	err = RemoveConfigRoot(p)

	if err == nil {
		t.Fatal("RemoveConfigRoot() = nil, want an error for an unremovable config root")
	}
	message := err.Error()
	if !strings.Contains(message, p.ConfigDir) {
		t.Errorf("error = %q, want it to name %q", message, p.ConfigDir)
	}
	for name, secret := range map[string]string{"raw": string(salt), "hex": hex.EncodeToString(salt)} {
		if strings.Contains(message, secret) {
			t.Errorf("error carries the %s salt bytes", name)
		}
	}
}
