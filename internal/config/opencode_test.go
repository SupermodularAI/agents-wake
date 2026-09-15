package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenCodeStoreDefaultsToTheXDGDataPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvXDGDataHome, "")

	store, err := OpenCodeStore()
	if err != nil {
		t.Fatalf("OpenCodeStore: %v", err)
	}
	if want := filepath.Join(home, ".local", "share", "opencode", "opencode.db"); store != want {
		t.Fatalf("OpenCodeStore() = %q, want %q", store, want)
	}
}

func TestOpenCodeStoreHonoursXDGDataHome(t *testing.T) {
	data := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvXDGDataHome, data)

	store, err := OpenCodeStore()
	if err != nil {
		t.Fatalf("OpenCodeStore: %v", err)
	}
	if want := filepath.Join(data, "opencode", "opencode.db"); store != want {
		t.Fatalf("OpenCodeStore() = %q, want %q", store, want)
	}
}

func TestOpenCodeStoreCreatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvXDGDataHome, "")

	if _, err := OpenCodeStore(); err != nil {
		t.Fatalf("OpenCodeStore: %v", err)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("home after resolving = %v (err %v), want it untouched", entries, err)
	}
}

func TestOpenCodeConfigFilePrefersJSONC(t *testing.T) {
	config := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvXDGConfigHome, config)
	t.Setenv(EnvOpenCodeConfig, "")
	dir := filepath.Join(config, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating config dir: %v", err)
	}
	for _, name := range []string{"opencode.jsonc", "opencode.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	file, err := OpenCodeConfigFile()
	if err != nil {
		t.Fatalf("OpenCodeConfigFile: %v", err)
	}
	if want := filepath.Join(dir, "opencode.jsonc"); file != want {
		t.Fatalf("OpenCodeConfigFile() = %q, want %q", file, want)
	}
}

func TestOpenCodeConfigFileFallsBackToJSON(t *testing.T) {
	config := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvXDGConfigHome, config)
	t.Setenv(EnvOpenCodeConfig, "")
	dir := filepath.Join(config, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	file, err := OpenCodeConfigFile()
	if err != nil {
		t.Fatalf("OpenCodeConfigFile: %v", err)
	}
	if want := filepath.Join(dir, "opencode.json"); file != want {
		t.Fatalf("OpenCodeConfigFile() = %q, want %q", file, want)
	}
}

func TestOpenCodeConfigFileIsEmptyWhenAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvXDGConfigHome, t.TempDir())
	t.Setenv(EnvOpenCodeConfig, "")

	file, err := OpenCodeConfigFile()
	if err != nil {
		t.Fatalf("OpenCodeConfigFile: %v", err)
	}
	if file != "" {
		t.Fatalf("OpenCodeConfigFile() = %q, want the empty string on a machine with no config", file)
	}
}

func TestOpenCodeConfigFileHonoursTheHarnessOverride(t *testing.T) {
	relocated := filepath.Join(t.TempDir(), "elsewhere.jsonc")
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvOpenCodeConfig, relocated)

	file, err := OpenCodeConfigFile()
	if err != nil {
		t.Fatalf("OpenCodeConfigFile: %v", err)
	}
	if file != relocated {
		t.Fatalf("OpenCodeConfigFile() = %q, want %q", file, relocated)
	}
}

func TestOpenCodeConfigRefusesARelativeOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvOpenCodeConfig, "relative/opencode.jsonc")

	if _, err := OpenCodeConfigFile(); !errors.Is(err, ErrOpenCodeConfigNotAbsolute) {
		t.Fatalf("OpenCodeConfigFile() error = %v, want ErrOpenCodeConfigNotAbsolute", err)
	}
}

func TestOpenCodeErrorsNameNoValue(t *testing.T) {
	// An error message is where this promise leaks (plan §4.2): the refusal names
	// the variable and never the path it held.
	relative := filepath.Join("some", "relative", "path")
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvOpenCodeConfig, relative)

	_, err := OpenCodeConfigFile()
	if err == nil {
		t.Fatal("a relative override was accepted")
	}
	if strings.Contains(err.Error(), relative) || strings.Contains(err.Error(), "/") {
		t.Fatalf("error = %q, which quotes the refused value", err)
	}
}
