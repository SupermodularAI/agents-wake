package activation

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// seedSettings writes a settings file holding one Wake-marked group and one the user
// owns, and returns the claude directory and the file's original bytes.
func seedSettings(t *testing.T) (claudeDir, original string) {
	t.Helper()
	claudeDir = filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	original = `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"existing command"}]},{"wake":true,"hooks":[{"type":"command","command":"wake ingest --quiet"}]}]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, settingsFileName), []byte(original), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return claudeDir, original
}

// seedBothRoots fills the two roots with something a removal has to take with it.
func seedBothRoots(t *testing.T, paths config.Paths) {
	t.Helper()
	for dir, name := range map[string]string{paths.DataDir: "events.ndjson", paths.ConfigDir: "config.toml"} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("seeded"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
}

func gone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(%s) error = %v, want fs.ErrNotExist", path, err)
	}
}

// Acceptance: the disclosure names every path that will be deleted. It is resolved
// once, so what gets printed and what gets removed cannot disagree.
func TestPlanUninstallDisclosesTheFourPathsItWillDelete(t *testing.T) {
	paths := testPaths(t)
	claudeDir, _ := seedSettings(t)
	executable := testExecutable(t)

	plan, err := PlanUninstall(paths, claudeDir, executable)

	if err != nil {
		t.Fatalf("PlanUninstall() error = %v", err)
	}
	for _, c := range []struct {
		name string
		got  string
		want string
	}{
		{"SettingsFile", plan.SettingsFile, filepath.Join(claudeDir, "settings.json")},
		{"DataDir", plan.DataDir, paths.DataDir},
		{"ConfigDir", plan.ConfigDir, paths.ConfigDir},
		{"Executable", plan.Executable, executable},
	} {
		if c.got != c.want {
			t.Errorf("plan.%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// A relative self path resolves against a working directory `uninstall` does not
// control, so the file it would unlink is not necessarily this binary.
func TestPlanUninstallRefusesARelativeExecutable(t *testing.T) {
	paths := testPaths(t)
	claudeDir, _ := seedSettings(t)

	for _, executable := range []string{"", "wake", filepath.Join("dist", "wake")} {
		plan, err := PlanUninstall(paths, claudeDir, executable)

		if !errors.Is(err, errExecutableNotAbsolute) {
			t.Errorf("PlanUninstall(%q) error = %v, want errExecutableNotAbsolute", executable, err)
		}
		if plan != (UninstallPlan{}) {
			t.Errorf("PlanUninstall(%q) returned a plan alongside the refusal", executable)
		}
		// The refusal names the requirement, never a path.
		for _, path := range []string{claudeDir, paths.ConfigDir, paths.DataDir} {
			if strings.Contains(err.Error(), path) {
				t.Errorf("refusal = %q, want it to carry no path", err.Error())
			}
		}
	}
}

// An installation reached through a link has the real file recorded in its hook
// command, so unlinking the link would leave the binary the hook named on disk.
func TestPlanUninstallResolvesASymlinkedExecutable(t *testing.T) {
	paths := testPaths(t)
	claudeDir, _ := seedSettings(t)
	target := testExecutable(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temporary directory: %v", err)
	}
	link := filepath.Join(dir, "wake-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	plan, err := PlanUninstall(paths, claudeDir, link)

	if err != nil {
		t.Fatalf("PlanUninstall() error = %v", err)
	}
	if plan.Executable != target {
		t.Errorf("plan.Executable = %q, want the link's target %q", plan.Executable, target)
	}
}

// `uninstall` is the command a user reaches for to get Wake off the machine, so it
// must not be the one command an odd installation cannot run.
func TestPlanUninstallKeepsAnUnresolvableExecutablePath(t *testing.T) {
	paths := testPaths(t)
	claudeDir, _ := seedSettings(t)
	absent := filepath.Join(t.TempDir(), "nowhere", "wake")

	plan, err := PlanUninstall(paths, claudeDir, absent)

	if err != nil {
		t.Fatalf("PlanUninstall() error = %v, want the cleaned path kept", err)
	}
	if plan.Executable != filepath.Clean(absent) {
		t.Errorf("plan.Executable = %q, want %q", plan.Executable, filepath.Clean(absent))
	}
}

// Acceptance: the hook entry, both roots and the binary all go — and a user's own
// hook entry stays.
func TestRemoveDeletesTheHookEntryBothRootsAndTheBinary(t *testing.T) {
	paths := testPaths(t)
	claudeDir, _ := seedSettings(t)
	seedBothRoots(t, paths)
	executable := testExecutable(t)
	plan, err := PlanUninstall(paths, claudeDir, executable)
	if err != nil {
		t.Fatalf("PlanUninstall() error = %v", err)
	}

	removed, err := plan.Remove()

	if err != nil || !removed {
		t.Fatalf("Remove() = %t, %v; want true, nil", removed, err)
	}
	gone(t, paths.DataDir)
	gone(t, paths.ConfigDir)
	gone(t, executable)
	settings, readErr := os.ReadFile(filepath.Join(claudeDir, settingsFileName))
	if readErr != nil {
		t.Fatalf("the settings file itself was removed: %v", readErr)
	}
	if !strings.Contains(string(settings), "existing command") {
		t.Errorf("settings = %s; the user's own hook entry is gone", settings)
	}
	if strings.Contains(string(settings), `"wake"`) {
		t.Errorf("settings = %s; Wake's marked group is still there", settings)
	}
}

// Acceptance: an absent integration is reported, not treated as a fault, and the rest
// of the removal still happens.
func TestRemoveOnAMachineThatWasNeverInitedReportsNoIntegrationAndStillRemovesEverything(t *testing.T) {
	paths := testPaths(t)
	claudeDir := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	seedBothRoots(t, paths)
	executable := testExecutable(t)
	plan, err := PlanUninstall(paths, claudeDir, executable)
	if err != nil {
		t.Fatalf("PlanUninstall() error = %v", err)
	}

	removed, err := plan.Remove()

	if err != nil {
		t.Fatalf("Remove() error = %v, want nil for a machine that was never init'ed", err)
	}
	if removed {
		t.Error("Remove() = true, want false — there was no integration to remove")
	}
	gone(t, paths.ConfigDir)
	gone(t, executable)
}

// A re-run after a partial failure has to converge rather than report a fault for
// work that is already done.
func TestRemoveIsIdempotent(t *testing.T) {
	paths := testPaths(t)
	claudeDir, _ := seedSettings(t)
	seedBothRoots(t, paths)
	plan, err := PlanUninstall(paths, claudeDir, testExecutable(t))
	if err != nil {
		t.Fatalf("PlanUninstall() error = %v", err)
	}
	if _, err := plan.Remove(); err != nil {
		t.Fatalf("first Remove() error = %v", err)
	}

	removed, err := plan.Remove()

	if err != nil {
		t.Errorf("second Remove() error = %v, want nil", err)
	}
	if removed {
		t.Error("second Remove() = true, want false — nothing was left to remove")
	}
}

// Acceptance: the binary is only removed after every other step succeeded, so a
// failure part way through leaves an invokable `wake uninstall` to retry.
func TestRemoveKeepsTheBinaryWhenAnEarlierStepFails(t *testing.T) {
	for _, c := range []struct {
		name string
		// arrange breaks one step and returns the claude directory to plan against.
		arrange func(t *testing.T, paths config.Paths) string
		// dataDirGone says whether the failure came after step 1 finished.
		dataDirGone bool
	}{
		{
			// A settings document this build refuses to edit fails step 1, with
			// nothing removed at all. Permission-free, so it holds whatever user the
			// suite runs as.
			name: "a settings document this build refuses",
			arrange: func(t *testing.T, paths config.Paths) string {
				t.Helper()
				claudeDir := filepath.Join(t.TempDir(), "claude")
				if err := os.MkdirAll(claudeDir, 0o700); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				if err := os.WriteFile(filepath.Join(claudeDir, settingsFileName), []byte("null"), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
				seedBothRoots(t, paths)
				return claudeDir
			},
		},
		{
			// A config root that cannot be unlinked fails step 2, after step 1 has
			// already removed the data root.
			name: "a config root that cannot be removed",
			arrange: func(t *testing.T, paths config.Paths) string {
				t.Helper()
				claudeDir, _ := seedSettings(t)
				seedBothRoots(t, paths)
				parent := filepath.Dir(paths.ConfigDir)
				if err := os.Chmod(parent, 0o500); err != nil {
					t.Fatalf("Chmod(%s) error = %v", parent, err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(parent, 0o700); err != nil {
						t.Errorf("restoring %s: %v", parent, err)
					}
				})
				return claudeDir
			},
			dataDirGone: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			paths := testPaths(t)
			claudeDir := c.arrange(t, paths)
			executable := testExecutable(t)
			plan, err := PlanUninstall(paths, claudeDir, executable)
			if err != nil {
				t.Fatalf("PlanUninstall() error = %v", err)
			}

			_, err = plan.Remove()

			if err == nil {
				t.Fatal("Remove() = nil, want the failure surfaced")
			}
			if _, statErr := os.Stat(executable); statErr != nil {
				t.Errorf("Stat(the binary) error = %v; a failed uninstall must leave it in place", statErr)
			}
			if c.dataDirGone {
				gone(t, paths.DataDir)
			}
		})
	}
}
