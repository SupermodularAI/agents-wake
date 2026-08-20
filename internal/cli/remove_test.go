package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is a regression pin, not new coverage. `wake uninstall` arrived beside
// `remove` and shares activation.Uninstall with it, so the thing most likely to break
// is `remove`'s scope — widening `--purge` to take the config root too would make
// `uninstall` redundant and would silently re-identify every repository on the next
// `init` (ADR-0019 §3). These three tests fail if that happens.

// Acceptance: `remove` takes the hooks and keeps both roots.
func TestRemoveRemovesWakeHooksAndKeepsBothRoots(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	settings := filepath.Join(claudeHome(t), "settings.json")
	writeFixture(t, settings, settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")

	out, err := run(t, "remove")

	if err != nil {
		t.Fatalf("remove returned an error: %v\n%s", err, out)
	}
	want := "Removed Wake's Claude Code integration.\n" +
		fmt.Sprintf("Local data was kept at %s. Remove it with `wake remove --purge`.\n", paths.DataDir)
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	for _, kept := range []string{filepath.Join(paths.DataDir, "events.ndjson"), paths.ConfigFile} {
		if _, statErr := os.Stat(kept); statErr != nil {
			t.Errorf("Stat(%s) error = %v; `remove` without --purge keeps local state", kept, statErr)
		}
	}
	body, readErr := os.ReadFile(settings)
	if readErr != nil {
		t.Fatalf("ReadFile(settings) error = %v", readErr)
	}
	if !strings.Contains(string(body), "existing command") {
		t.Errorf("settings = %s; the user's own hook entry is gone", body)
	}
	if strings.Contains(string(body), `"wake"`) {
		t.Errorf("settings = %s; Wake's marked group is still there", body)
	}
}

// Acceptance: `--purge` takes the data root and only the data root. The config-root
// assertion is the one that fails if anyone widens `--purge` while implementing
// `uninstall`.
func TestRemovePurgeRemovesOnlyTheDataRoot(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	writeFixture(t, filepath.Join(claudeHome(t), "settings.json"), settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")

	out, err := run(t, "remove", "--purge")

	if err != nil {
		t.Fatalf("remove --purge returned an error: %v\n%s", err, out)
	}
	want := "Removed Wake's Claude Code integration.\n" +
		fmt.Sprintf("Removed local data at %s.\n", paths.DataDir)
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	absent(t, paths.DataDir)
	if _, statErr := os.Stat(paths.ConfigFile); statErr != nil {
		t.Errorf("Stat(config.toml) error = %v; --purge keeps the config root, which is what makes `uninstall` a separate command", statErr)
	}
}

// Acceptance: an absent integration is reported plainly rather than treated as a fault.
func TestRemoveOnASystemThatWasNeverInitedReportsItWasNotInstalled(t *testing.T) {
	isolateUnderOneHome(t)

	out, err := run(t, "remove")

	if err != nil {
		t.Fatalf("remove returned an error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Wake's Claude Code integration was not installed.") {
		t.Errorf("output does not report the absent integration plainly:\n%s", out)
	}
}
