package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// pointSelfPathAtAThrowawayBinary replaces the self-path resolution with a temp file,
// so the test deletes that rather than the binary running the suite.
//
// A variable for the same reason ingest.go's hookChild is one: this seam is the only
// way to exercise the unlink at all — letting it resolve for real would delete the
// test binary.
func pointSelfPathAtAThrowawayBinary(t *testing.T) string {
	t.Helper()
	// Already-resolved, because PlanUninstall symlink-resolves what it is given and on
	// macOS t.TempDir() sits under /var, itself a link to /private/var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temporary directory: %v", err)
	}
	path := filepath.Join(dir, "wake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	original := selfPath
	selfPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { selfPath = original })
	return path
}

// isolateUnderOneHome points HOME and the data root inside a single temporary tree, so
// a walk of that tree sees the config root, the data root and the harness directory at
// once. That is what makes "nothing outside the disclosed paths changed" checkable —
// isolate() puts the data root in a separate TempDir, where no single walk covers both.
func isolateUnderOneHome(t *testing.T) (config.Paths, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(config.EnvDataDir, filepath.Join(home, "state"))
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() error = %v", err)
	}
	return paths, home
}

// writeFixture creates path's parent and writes content, so a seed is one line.
func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

// The two hook groups every settings fixture here starts from: one Wake owns and one
// the user does.
const (
	userHookGroup = `{"hooks":[{"type":"command","command":"existing command"}]}`
	wakeHookGroup = `{"wake":true,"hooks":[{"type":"command","command":"wake ingest --quiet"}]}`
)

func settingsWith(groups ...string) string {
	return `{"hooks":{"SessionStart":[` + strings.Join(groups, ",") + `]}}`
}

// snapshot records every path under root with its kind, mode and contents, so two
// snapshots can be compared for anything that changed.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	seen := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		if d.IsDir() {
			seen[path] = fmt.Sprintf("dir mode=%v", info.Mode().Perm())
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		seen[path] = fmt.Sprintf("file mode=%v body=%s", info.Mode().Perm(), body)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return seen
}

func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(os.PathSeparator))
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(%s) error = %v, want fs.ErrNotExist", path, err)
	}
}

// Acceptance: the disclosure names every path that will be deleted, before anything is
// touched. Ordering is asserted by index, because a disclosure printed after the
// removal is not a consent step.
func TestUninstallDisclosesEveryPathBeforeRemovingAnything(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	binary := pointSelfPathAtAThrowawayBinary(t)
	settings := filepath.Join(claudeHome(t), "settings.json")
	writeFixture(t, settings, settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")

	out, err := run(t, "uninstall")

	if err != nil {
		t.Fatalf("uninstall returned an error: %v\n%s", err, out)
	}
	result := strings.Index(out, "are gone")
	if result < 0 {
		t.Fatalf("output has no result line:\n%s", out)
	}
	for _, path := range []string{settings, paths.DataDir, paths.ConfigDir, binary} {
		at := strings.Index(out, path)
		if at < 0 {
			t.Errorf("the disclosure never names %q; got:\n%s", path, out)
			continue
		}
		if at > result {
			t.Errorf("%q is disclosed after the removal was reported; got:\n%s", path, out)
		}
	}
}

// Acceptance: nothing Wake-related remains on disk afterward.
func TestUninstallRemovesTheIntegrationBothRootsAndTheBinary(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	binary := pointSelfPathAtAThrowawayBinary(t)
	settings := filepath.Join(claudeHome(t), "settings.json")
	writeFixture(t, settings, settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")

	out, err := run(t, "uninstall")

	if err != nil {
		t.Fatalf("uninstall returned an error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Removed Wake's Claude Code integration.") {
		t.Errorf("output does not report the integration was removed:\n%s", out)
	}
	absent(t, paths.DataDir)
	absent(t, paths.ConfigDir)
	absent(t, binary)
	body, readErr := os.ReadFile(settings)
	if readErr != nil {
		t.Fatalf("the harness's own settings file was removed: %v", readErr)
	}
	if bytes.Contains(body, []byte(`"wake"`)) {
		t.Errorf("settings = %s; Wake's marked group is still there", body)
	}
}

// Acceptance: a user's own hook entries stay byte-identical. removeHooks republishes
// only when it removed something, so a file with nothing of Wake's in it must come out
// untouched rather than reformatted.
func TestUninstallKeepsAUsersOwnHookEntriesByteIdentical(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	pointSelfPathAtAThrowawayBinary(t)
	settings := filepath.Join(claudeHome(t), "settings.json")
	original := settingsWith(userHookGroup)
	writeFixture(t, settings, original)
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")

	if out, err := run(t, "uninstall"); err != nil {
		t.Fatalf("uninstall returned an error: %v\n%s", err, out)
	}

	body, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(body) != original {
		t.Errorf("settings = %s, want the seeded bytes unchanged:\n%s", body, original)
	}
}

// Acceptance: run on a system that was never `init`ed, it still removes the config root
// and the binary, reports plainly that no integration existed, and does not error out.
func TestUninstallOnASystemThatWasNeverInitedStillRemovesConfigAndTheBinary(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	binary := pointSelfPathAtAThrowawayBinary(t)
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")

	out, err := run(t, "uninstall")

	if err != nil {
		t.Fatalf("uninstall returned an error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Wake's Claude Code integration was not installed.") {
		t.Errorf("output does not report the absent integration plainly:\n%s", out)
	}
	absent(t, paths.ConfigDir)
	absent(t, binary)
}

// Acceptance: a filesystem diff proves nothing outside the disclosed paths changed.
func TestUninstallChangesNothingOutsideTheDisclosedPaths(t *testing.T) {
	paths, home := isolateUnderOneHome(t)
	pointSelfPathAtAThrowawayBinary(t)
	claudeDir := claudeHome(t)
	settings := filepath.Join(claudeDir, "settings.json")
	writeFixture(t, settings, settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(claudeDir, "some-other-setting.json"), `{"kept":true}`)
	writeFixture(t, filepath.Join(claudeDir, "projects", "p", "session.jsonl"), "{}\n")
	writeFixture(t, filepath.Join(home, ".bashrc"), "export PS1=$\n")
	writeFixture(t, filepath.Join(home, ".config", "another-tool", "config.toml"), "keep = true\n")
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")
	before := snapshot(t, home)
	// Asserted rather than assumed: an empty or shallow snapshot would let the diff
	// below pass with the command deleting anything it liked.
	for _, seeded := range []string{settings, filepath.Join(home, ".bashrc"), filepath.Join(claudeDir, "projects", "p", "session.jsonl"), paths.ConfigFile} {
		if _, ok := before[seeded]; !ok {
			t.Fatalf("the snapshot never saw %s; the diff would be vacuous", seeded)
		}
	}

	if out, err := run(t, "uninstall"); err != nil {
		t.Fatalf("uninstall returned an error: %v\n%s", err, out)
	}

	after := snapshot(t, home)
	for path, was := range before {
		now, still := after[path]
		if still && now == was {
			continue
		}
		if within(path, paths.DataDir) || within(path, paths.ConfigDir) || path == settings {
			continue
		}
		t.Errorf("%s changed outside the disclosed paths: was %q, now %q (present=%t)", path, was, now, still)
	}
	for path := range after {
		if _, existed := before[path]; existed {
			continue
		}
		if within(path, paths.DataDir) || within(path, paths.ConfigDir) {
			continue
		}
		t.Errorf("%s was created outside the disclosed paths", path)
	}
}

// The removal sequence, the config-root deletion and the self-path resolution all live
// below this layer (ADR-0001, plan §6.2). The test reads the source because "no logic
// lives here" is a property of the file, not of an output.
func TestUninstallCommandHoldsNoRemovalLogic(t *testing.T) {
	raw, err := os.ReadFile("uninstall.go")
	if err != nil {
		t.Fatalf("reading uninstall.go: %v", err)
	}
	for _, forbidden := range []string{"RemoveAll", "os.Remove(", "EvalSymlinks"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("uninstall.go names %q; what gets deleted is decided below this layer, which only parses and prints", forbidden)
		}
	}
}

// Acceptance: a failure part way through surfaces and leaves the binary in place, so
// the user can retry `wake uninstall` rather than being left with an uninvokable tool.
//
// A settings document this build refuses to edit is refused before the disclosure, not
// after: the disclosure is worded as a deletion that is about to happen, and printing it
// ahead of a run that removes nothing is what left the user unable to tell whether part
// of it happened. So this asserts the opposite of what a disclosure test does — the
// promise was never made, and the refusal says so.
func TestUninstallRefusesASettingsFileItWillNotEditWithoutPromisingADeletion(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	binary := pointSelfPathAtAThrowawayBinary(t)
	writeFixture(t, filepath.Join(claudeHome(t), "settings.json"), "null")
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")

	out, err := run(t, "uninstall")

	if err == nil {
		t.Fatalf("uninstall returned nil, want the failure surfaced; output:\n%s", out)
	}
	message := err.Error()
	if strings.Contains(message, "wake init") {
		t.Errorf("refusal = %q; the removal command must not send the user to the command that installs", message)
	}
	if !strings.Contains(message, "nothing has been removed") {
		t.Errorf("refusal = %q, want it to say plainly that nothing has been removed", message)
	}
	if strings.Contains(out, "Wake will permanently delete") {
		t.Errorf("the disclosure promised a deletion that never happened:\n%s", out)
	}
	for _, path := range []string{paths.DataDir, paths.ConfigDir, binary} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("Stat(%s) error = %v; the refusal must leave everything in place", path, statErr)
		}
	}
}

// A failure after the disclosure is a different case: something may already be gone, so
// the run reports what it managed before it stopped rather than leaving the reader to
// guess which of the four disclosed paths survived.
func TestUninstallReportsWhatItManagedBeforeAFailure(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	binary := pointSelfPathAtAThrowawayBinary(t)
	writeFixture(t, filepath.Join(claudeHome(t), "settings.json"), settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")
	// The config root cannot be unlinked, so the removal fails at its second step —
	// after the hook entry and the data root have already gone.
	parent := filepath.Dir(paths.ConfigDir)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("Chmod(%s) error = %v", parent, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Errorf("restoring %s: %v", parent, err)
		}
	})

	out, err := run(t, "uninstall")

	if err == nil {
		t.Fatalf("uninstall returned nil, want the failure surfaced; output:\n%s", out)
	}
	if !strings.Contains(out, "Removed Wake's Claude Code integration") {
		t.Errorf("output does not report the step that did succeed:\n%s", out)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("output does not say the removal stopped part way:\n%s", out)
	}
	if !strings.Contains(out, "run `wake uninstall` again") {
		t.Errorf("output does not name the retry:\n%s", out)
	}
	if _, statErr := os.Stat(binary); statErr != nil {
		t.Errorf("Stat(the binary) error = %v; a failed uninstall must leave it in place", statErr)
	}
}

// The success line reports the state it leaves behind rather than asserting four
// removals, because a machine that was never `init`ed has less than four to remove and
// "Removed …" would be claiming work that never happened.
func TestUninstallSuccessLineClaimsNoRemovalThatDidNotHappen(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	pointSelfPathAtAThrowawayBinary(t)

	out, err := run(t, "uninstall")

	if err != nil {
		t.Fatalf("uninstall returned an error: %v\n%s", err, out)
	}
	if strings.Contains(out, "Removed Wake's local data") {
		t.Errorf("output claims removals that did not happen on a machine with neither root:\n%s", out)
	}
	if !strings.Contains(out, "are gone") {
		t.Errorf("output does not report the state it leaves behind:\n%s", out)
	}
	absent(t, paths.DataDir)
}

// The `Short` string is one row of `wake --help`, so it has to fit the table every other
// row fits.
func TestUninstallShortFitsTheHelpTable(t *testing.T) {
	for _, command := range commands {
		cmd := command()
		if cmd.Name() != "uninstall" {
			continue
		}
		// 80 columns minus the width `wake --help` spends on the command name and its
		// padding, which cobra sets from the longest name in the table.
		if limit := 60; len(cmd.Short) > limit {
			t.Errorf("Short is %d characters, want at most %d so the help table stays aligned: %q", len(cmd.Short), limit, cmd.Short)
		}
		return
	}
	t.Fatal("no uninstall command is registered")
}

// An installation reached through a link on PATH: the link is disclosed as a fifth path
// and removed with the file it points at, because a link left behind is a `wake` on PATH
// that resolves to nothing.
func TestUninstallDisclosesAndRemovesALinkOnPath(t *testing.T) {
	isolateUnderOneHome(t)
	binary := pointSelfPathAtAThrowawayBinary(t)
	link := filepath.Join(filepath.Dir(binary), "wake-on-path")
	if err := os.Symlink(binary, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	selfPath = func() (string, error) { return link, nil }

	out, err := run(t, "uninstall")

	if err != nil {
		t.Fatalf("uninstall returned an error: %v\n%s", err, out)
	}
	if !strings.Contains(out, link) {
		t.Errorf("the disclosure never names the link it will remove:\n%s", out)
	}
	absent(t, binary)
	if _, lstatErr := os.Lstat(link); !errors.Is(lstatErr, fs.ErrNotExist) {
		t.Errorf("Lstat(%s) error = %v; the link is still on PATH pointing at nothing", link, lstatErr)
	}
}
