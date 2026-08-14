package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPaths points the whole layout at a fresh temporary HOME and returns the
// resolved paths. Every test in the package goes through it, so no test can read
// or write the developer's real ~/.config/wake — and clearing EnvDataDir means a
// developer who exports WAKE_DIR does not see different results from CI.
func testPaths(t *testing.T) Paths {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvDataDir, "")
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() with HOME set to a temp dir: %v", err)
	}
	return p
}

func TestPathsResolveToTheXDGLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvDataDir, "")

	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() = %v", err)
	}

	for _, c := range []struct {
		name string
		got  string
		want string
	}{
		{"ConfigDir", p.ConfigDir, filepath.Join(home, ".config", "wake")},
		{"DataDir", p.DataDir, filepath.Join(home, ".local", "state", "wake")},
		{"ConfigFile", p.ConfigFile, filepath.Join(home, ".config", "wake", "config.toml")},
		{"SaltFile", p.SaltFile, filepath.Join(home, ".config", "wake", "repo-salt")},
		{"ProjectsFile", p.ProjectsFile, filepath.Join(home, ".local", "state", "wake", "projects.json")},
		{"PrimitivesFile", p.PrimitivesFile, filepath.Join(home, ".local", "state", "wake", "primitives.json")},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// WAKE_DIR moves the data root and nothing else. A change that also moved the
// salt would silently re-identify every repo (ADR-0019 §3), so the config-side
// paths are asserted to be unmoved rather than merely assumed.
func TestWakeDirOverridesTheDataRootOnly(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvDataDir, elsewhere)

	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() = %v", err)
	}

	if p.DataDir != elsewhere {
		t.Errorf("DataDir = %q, want %q", p.DataDir, elsewhere)
	}
	if want := filepath.Join(elsewhere, "projects.json"); p.ProjectsFile != want {
		t.Errorf("ProjectsFile = %q, want %q", p.ProjectsFile, want)
	}
	if want := filepath.Join(elsewhere, "primitives.json"); p.PrimitivesFile != want {
		t.Errorf("PrimitivesFile = %q, want %q", p.PrimitivesFile, want)
	}
	if want := filepath.Join(home, ".config", "wake"); p.ConfigDir != want {
		t.Errorf("ConfigDir = %q, want %q — WAKE_DIR must not move the config root", p.ConfigDir, want)
	}
	for _, got := range []string{p.ConfigFile, p.SaltFile} {
		if strings.HasPrefix(got, elsewhere) {
			t.Errorf("%q moved into the data root; WAKE_DIR must not move a config file", got)
		}
	}
}

// The literal XDG paths plus WAKE_DIR are the whole override surface. No ADR
// grants a second variable, and honouring XDG_CONFIG_HOME would relocate the
// salt for anyone who sets it.
func TestNoSecondEnvOverrideIsHonoured(t *testing.T) {
	home := t.TempDir()
	decoy := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvDataDir, "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(decoy, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(decoy, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(decoy, "data"))

	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() = %v", err)
	}

	for _, got := range []string{p.ConfigDir, p.DataDir, p.ConfigFile, p.SaltFile, p.ProjectsFile, p.PrimitivesFile} {
		if !strings.HasPrefix(got, home) {
			t.Errorf("%q is not under HOME; an XDG_* variable was honoured", got)
		}
	}
}

// The data root is a documented public integration surface (ADR-0017), so
// resolving a relative WAKE_DIR against the process cwd would make the store
// location ambient. It is an error instead.
func TestRelativeWakeDirIsRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, value := range []string{"relative/dir", "./dir", "~/dir", "dir"} {
		t.Setenv(EnvDataDir, value)

		p, err := ResolvePaths()
		if !errors.Is(err, ErrDataDirNotAbsolute) {
			t.Errorf("ResolvePaths() with %s=%q = (%+v, %v), want ErrDataDirNotAbsolute", EnvDataDir, value, p, err)
			continue
		}
		if strings.Contains(err.Error(), value) {
			t.Errorf("error message %q contains the rejected value; it must name the variable, not its value", err)
		}
	}
}

func TestEmptyWakeDirIsTreatedAsUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, value := range []string{"", " ", "\t\n"} {
		t.Setenv(EnvDataDir, value)

		p, err := ResolvePaths()
		if err != nil {
			t.Fatalf("ResolvePaths() with %s=%q = %v, want the default layout", EnvDataDir, value, err)
		}
		if want := filepath.Join(home, ".local", "state", "wake"); p.DataDir != want {
			t.Errorf("DataDir with %s=%q = %q, want %q", EnvDataDir, value, p.DataDir, want)
		}
	}
}

// ADR-0010 rests on uninstall being able to remove the config root and keep the
// data root, or the reverse, without ambiguity. That is only unambiguous while
// neither root contains the other.
func TestConfigRootAndDataRootAreSeparate(t *testing.T) {
	p := testPaths(t)

	if p.ConfigDir == p.DataDir {
		t.Fatalf("ConfigDir and DataDir are the same path (%q)", p.ConfigDir)
	}
	for _, c := range [][2]string{{p.ConfigDir, p.DataDir}, {p.DataDir, p.ConfigDir}} {
		if strings.HasPrefix(c[1], c[0]+string(filepath.Separator)) {
			t.Errorf("%q is nested inside %q; uninstall could not remove one and keep the other", c[1], c[0])
		}
	}
	if filepath.Dir(p.SaltFile) != p.ConfigDir {
		t.Errorf("SaltFile is in %q, want the config root %q", filepath.Dir(p.SaltFile), p.ConfigDir)
	}
	if filepath.Dir(p.ProjectsFile) != p.DataDir {
		t.Errorf("ProjectsFile is in %q, want the data root %q", filepath.Dir(p.ProjectsFile), p.DataDir)
	}
	if filepath.Dir(p.PrimitivesFile) != p.DataDir {
		t.Errorf("PrimitivesFile is in %q, want the data root %q", filepath.Dir(p.PrimitivesFile), p.DataDir)
	}
}

// Resolving where a file belongs is not the same as creating it: a missing
// config file must yield defaults with nothing created (acceptance item 2), and
// that starts with the resolver itself writing nothing.
func TestResolvePathsCreatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvDataDir, filepath.Join(t.TempDir(), "data"))

	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() = %v", err)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("reading HOME: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ResolvePaths() created %d entries under HOME, want none", len(entries))
	}
	for _, path := range []string{p.ConfigDir, p.DataDir, p.ConfigFile, p.SaltFile, p.ProjectsFile, p.PrimitivesFile} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("os.Lstat(%q) = %v, want ErrNotExist — ResolvePaths must create nothing", path, err)
		}
	}
}
