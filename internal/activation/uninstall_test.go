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

// claudeDirWith writes a settings file holding content, for the documents this build
// refuses to edit.
func claudeDirWith(t *testing.T, content string) string {
	t.Helper()
	claudeDir := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeSettings(t, claudeDir, content)
	return claudeDir
}

func writeSettings(t *testing.T, claudeDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(claudeDir, settingsFileName), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
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

func stillThere(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Stat(%s) error = %v, want it left in place", path, err)
	}
}

// assertRefusedNothingRemoved checks a refusal says the two things only the removal
// command can say: that nothing has been removed, and which command to run once the
// settings file is repaired. `wake init` is the one answer that must never appear —
// it is the opposite of what the user asked for.
func assertRefusedNothingRemoved(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want the settings document refused")
	}
	message := err.Error()
	if strings.Contains(message, "wake init") {
		t.Errorf("refusal = %q; it sends somebody who asked for removal to the command that installs", message)
	}
	if !strings.Contains(message, "run wake uninstall again") {
		t.Errorf("refusal = %q, want it to name the command to run once the file is repaired", message)
	}
	if !strings.Contains(message, "nothing has been removed") {
		t.Errorf("refusal = %q, want it to say plainly that nothing has been removed", message)
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

// A settings document this build refuses to edit is refused before anything has been
// disclosed, and refused in `uninstall`'s own words: nothing has been removed, and the
// command to run once the file is repaired is the one the user asked for.
func TestPlanUninstallRefusesASettingsDocumentItWillNotEdit(t *testing.T) {
	// Named rather than keyed by content: t.Run puts the subtest's name in
	// t.TempDir()'s path, and testExecutable refuses a path holding a character a
	// hook command cannot carry — which every one of these documents does.
	for _, c := range []struct{ name, content string }{
		{"the document is null", `null`},
		{"the document is an array", `[]`},
		{"the document is a string", `"text"`},
		{"the document is not JSON", `{not json`},
		{"hooks is a number", `{"hooks":5}`},
		{"a hook event is a number", `{"hooks":{"SessionStart":5}}`},
		// Not one of hookEvents: removeHooks sweeps every event key, so the refusal
		// this raises has to be pre-checked over every event key too.
		{"an event this build does not register is a number", `{"hooks":{"SomeOtherEvent":5}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			paths := testPaths(t)
			claudeDir := claudeDirWith(t, c.content)
			seedBothRoots(t, paths)
			executable := testExecutable(t)

			plan, err := PlanUninstall(paths, claudeDir, executable)

			assertRefusedNothingRemoved(t, err)
			if plan != (UninstallPlan{}) {
				t.Error("PlanUninstall() returned a plan alongside the refusal")
			}
			for _, path := range []string{paths.DataDir, paths.ConfigDir, executable} {
				stillThere(t, path)
			}
		})
	}
}

// A broken symlink is the same refusal by another route, and the realistic one: a
// dotfile store whose link is committed and whose target is not checked out yet.
func TestPlanUninstallRefusesABrokenSettingsLink(t *testing.T) {
	paths := testPaths(t)
	claudeDir := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "dotfiles", "settings.json"), filepath.Join(claudeDir, settingsFileName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	seedBothRoots(t, paths)
	executable := testExecutable(t)

	_, err := PlanUninstall(paths, claudeDir, executable)

	assertRefusedNothingRemoved(t, err)
	for _, path := range []string{paths.DataDir, paths.ConfigDir, executable} {
		stillThere(t, path)
	}
}

// The sentinels stopped carrying a command, so `init` has to name its own: a settings
// document it refuses still tells the user to run `wake init` again.
func TestInitNamesInitAsTheStepAfterASettingsRefusal(t *testing.T) {
	paths := testPaths(t)
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	claudeDir := claudeDirWith(t, "null")

	_, err := Init(paths, root, claudeDir, testExecutable(t))

	if err == nil {
		t.Fatal("Init() error = nil, want the settings document refused")
	}
	if !strings.Contains(err.Error(), "run wake init again") {
		t.Errorf("refusal = %q, want it to name the command to run once the file is repaired", err.Error())
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
	if linkErr := os.Symlink(target, link); linkErr != nil {
		t.Fatalf("Symlink() error = %v", linkErr)
	}

	plan, err := PlanUninstall(paths, claudeDir, link)

	if err != nil {
		t.Fatalf("PlanUninstall() error = %v", err)
	}
	if plan.Executable != target {
		t.Errorf("plan.Executable = %q, want the link's target %q", plan.Executable, target)
	}
	if plan.Launcher != link {
		t.Errorf("plan.Launcher = %q, want the link the command was invoked through %q", plan.Launcher, link)
	}
}

// A link left behind is a `wake` on PATH that resolves to nothing — an invocation that
// fails with "No such file or directory" rather than "command not found", which is
// worse than either. The ticket asks for nothing Wake-related left on disk, so the link
// goes too, and it is disclosed because it is deleted.
func TestRemoveAlsoRemovesTheLinkItWasInvokedThrough(t *testing.T) {
	paths := testPaths(t)
	claudeDir, _ := seedSettings(t)
	seedBothRoots(t, paths)
	target := testExecutable(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temporary directory: %v", err)
	}
	link := filepath.Join(dir, "wake-link")
	if linkErr := os.Symlink(target, link); linkErr != nil {
		t.Skipf("symlinks unavailable: %v", linkErr)
	}
	plan, err := PlanUninstall(paths, claudeDir, link)
	if err != nil {
		t.Fatalf("PlanUninstall() error = %v", err)
	}

	if _, removeErr := plan.Remove(); removeErr != nil {
		t.Fatalf("Remove() error = %v", removeErr)
	}

	gone(t, target)
	if _, lstatErr := os.Lstat(link); !errors.Is(lstatErr, fs.ErrNotExist) {
		t.Errorf("Lstat(%s) error = %v; the link is still on PATH pointing at nothing", link, lstatErr)
	}
}

// A plain file on PATH — what both documented installations leave — has no link to
// disclose, so the run prints the same four paths it always did.
func TestPlanUninstallHasNoLauncherForAPlainFile(t *testing.T) {
	paths := testPaths(t)
	claudeDir, _ := seedSettings(t)

	plan, err := PlanUninstall(paths, claudeDir, testExecutable(t))

	if err != nil {
		t.Fatalf("PlanUninstall() error = %v", err)
	}
	if plan.Launcher != "" {
		t.Errorf("plan.Launcher = %q, want empty for a path that is not a link", plan.Launcher)
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
	if _, firstErr := plan.Remove(); firstErr != nil {
		t.Fatalf("first Remove() error = %v", firstErr)
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
		// breakAfterPlanning breaks a step PlanUninstall pre-checks, so the failure
		// lands in Remove rather than in the plan.
		breakAfterPlanning func(t *testing.T, claudeDir string)
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
				claudeDir, _ := seedSettings(t)
				seedBothRoots(t, paths)
				return claudeDir
			},
			// Replaced after the plan resolved: PlanUninstall refuses this document up
			// front, so the only way step 1 still meets it is the race the pre-check
			// cannot close.
			breakAfterPlanning: func(t *testing.T, claudeDir string) {
				t.Helper()
				writeSettings(t, claudeDir, "null")
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
			if c.breakAfterPlanning != nil {
				c.breakAfterPlanning(t, claudeDir)
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

// A settings file that changes shape between the pre-check and the sweep is refused in
// the same words, because it is the same answer: nothing has been removed, and the
// command to run once the file is repaired is `wake uninstall`.
func TestRemoveRestatesALostRaceOnTheSettingsFileAsItsOwnRefusal(t *testing.T) {
	paths := testPaths(t)
	claudeDir, _ := seedSettings(t)
	seedBothRoots(t, paths)
	executable := testExecutable(t)
	plan, err := PlanUninstall(paths, claudeDir, executable)
	if err != nil {
		t.Fatalf("PlanUninstall() error = %v", err)
	}
	writeSettings(t, claudeDir, "null")

	_, err = plan.Remove()

	assertRefusedNothingRemoved(t, err)
	for _, path := range []string{paths.DataDir, paths.ConfigDir, executable} {
		stillThere(t, path)
	}
}
