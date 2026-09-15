package activation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
	"github.com/SupermodularAI/agents-wake/internal/adapter/opencode"
	"github.com/SupermodularAI/agents-wake/internal/config"
)

// withHarnessKey writes a config file setting scan.harnesses to value.
func withHarnessKey(t *testing.T, paths config.Paths, value string) {
	t.Helper()
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatalf("creating the config dir: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[scan]\nharnesses = "+value+"\n"), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}
}

func TestTheDefaultSelectsBothHarnesses(t *testing.T) {
	enabled := selected(testPaths(t))
	if !enabled[claudecode.Harness()] || !enabled[opencode.Harness()] {
		t.Fatalf("selected() = %v with no config file, want both harnesses", enabled)
	}
}

func TestAKeyThatOmitsAHarnessDeselectsIt(t *testing.T) {
	paths := testPaths(t)
	withHarnessKey(t, paths, `["claude-code"]`)

	enabled := selected(paths)
	if !enabled[claudecode.Harness()] {
		t.Error("the named harness was deselected")
	}
	if enabled[opencode.Harness()] {
		t.Error("a harness the key does not name was selected")
	}
}

func TestAnEmptyKeyMeansNoHarness(t *testing.T) {
	// Empty means none, not all — the rule the sibling scan.repos key already
	// states for its own surface.
	paths := testPaths(t)
	withHarnessKey(t, paths, `[]`)

	if enabled := selected(paths); len(enabled) != 0 {
		t.Fatalf("selected() = %v, want nothing selected", enabled)
	}
}

func TestAnUnreadableConfigFallsBackToTheDefault(t *testing.T) {
	// An unparseable config must not silently stop all collection: that is the
	// failure doctor exists to make impossible.
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatalf("creating the config dir: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("this is not toml = = ["), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}

	enabled := selected(paths)
	if !enabled[claudecode.Harness()] || !enabled[opencode.Harness()] {
		t.Fatalf("selected() = %v, want the default", enabled)
	}
}

func TestAnUnknownHarnessNameIsIgnored(t *testing.T) {
	paths := testPaths(t)
	withHarnessKey(t, paths, `["claude-code", "codex"]`)

	enabled := selected(paths)
	if !enabled[claudecode.Harness()] {
		t.Error("a known harness was dropped alongside an unknown one")
	}
	if enabled[opencode.Harness()] {
		t.Error("opencode was selected by a key that does not name it")
	}
}

func TestAnUnnameableHarnessIsIgnored(t *testing.T) {
	paths := testPaths(t)
	withHarnessKey(t, paths, `["../etc/passwd", "claude-code"]`)

	enabled := selected(paths)
	if len(enabled) != 1 || !enabled[claudecode.Harness()] {
		t.Fatalf("selected() = %v, want only claude-code", enabled)
	}
}

func TestASelectedHarnessWithNoStoreIsNotObserved(t *testing.T) {
	// The key filters and presence detects: naming a harness cannot make a store
	// that is not there readable, and the absence renders "not observed".
	paths := testPaths(t)
	withHarnessKey(t, paths, `["opencode"]`)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)

	_, counters, err := runOpenCode(t, repos, filepath.Join(t.TempDir(), "missing.db"), filepath.Join(t.TempDir(), "events.ndjson"))
	if err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	if !selected(paths)[opencode.Harness()] {
		t.Fatal("the key did not select opencode")
	}
	if counters.Observed {
		t.Fatal("a selected harness with no store was reported as observed")
	}
}
