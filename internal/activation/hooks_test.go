package activation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// settingsFixture returns a paths value and a Claude Code directory holding the
// given settings file, or none when content is empty.
func settingsFixture(t *testing.T, content string) (config.Paths, string) {
	t.Helper()
	paths := testPaths(t)
	claudeDir := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if content != "" {
		if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	return paths, claudeDir
}

func settingsPath(claudeDir string) string { return filepath.Join(claudeDir, "settings.json") }

func readSettingsFile(t *testing.T, claudeDir string) string {
	t.Helper()
	raw, err := os.ReadFile(settingsPath(claudeDir))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	return string(raw)
}

// testExecutable writes a file that satisfies hookCommandFor, so a test exercises
// installation rather than the rejection path.
func testExecutable(t *testing.T) string {
	t.Helper()
	// An already-resolved directory: on macOS t.TempDir() sits under /var, which is
	// a symlink to /private/var, so hookCommandFor would correctly return a path
	// spelled differently from the one the test built. The tests that are about
	// symlink resolution create the link themselves.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temporary directory: %v", err)
	}
	path := filepath.Join(dir, "wake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	// Asserted rather than assumed: a temporary directory whose path carried a
	// character the allowlist rejects would fail every test in this file for a
	// reason none of them is about.
	if _, err := hookCommandFor(path); err != nil {
		t.Fatalf("hookCommandFor(%q) = %v; this platform's temporary paths cannot host the fixture", path, err)
	}
	return path
}

func testCommand(t *testing.T) string {
	t.Helper()
	command, err := hookCommandFor(testExecutable(t))
	if err != nil {
		t.Fatalf("hookCommandFor() error = %v", err)
	}
	return command
}

// A group with no marker is not Wake's, whatever command it holds. Command
// equality is not proof of ownership: a user who copied Wake's command into their
// own group owns that group.
func TestRemovePreservesAGroupThatMerelyContainsWakesCommand(t *testing.T) {
	command := testCommand(t)
	settings := `{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":` + quote(command) + `}]}]}}`
	paths, claudeDir := settingsFixture(t, settings)

	removed, keptOwned, err := removeHooks(paths, claudeDir)
	if err != nil {
		t.Fatalf("removeHooks() error = %v", err)
	}

	if removed != 0 {
		t.Errorf("removed = %d, want 0 — a group with no marker is not Wake's", removed)
	}
	if keptOwned != 0 {
		t.Errorf("keptOwned = %d, want 0 — the group carries no marker", keptOwned)
	}
	if got := readSettingsFile(t, claudeDir); !strings.Contains(got, command) {
		t.Errorf("settings = %s, want the user's group preserved", got)
	}
}

// A marked group the user added a second hook to is a group they have edited, and
// removing it would take their hook with it. Failing to remove is the safe
// direction; failing to preserve is not.
func TestRemovePreservesAMarkedGroupCarryingAThirdPartyHook(t *testing.T) {
	command := testCommand(t)
	settings := `{"hooks":{"SessionEnd":[{"wake":true,"hooks":[{"type":"command","command":` + quote(command) + `},{"type":"command","command":"notify-send done"}]}]}}`
	paths, claudeDir := settingsFixture(t, settings)

	removed, keptOwned, err := removeHooks(paths, claudeDir)
	if err != nil {
		t.Fatalf("removeHooks() error = %v", err)
	}

	if removed != 0 {
		t.Errorf("removed = %d, want 0 — the group's definition no longer matches", removed)
	}
	if keptOwned != 1 {
		t.Errorf("keptOwned = %d, want 1 — a marked group left in place has to be visible", keptOwned)
	}
	got := readSettingsFile(t, claudeDir)
	for _, want := range []string{command, "notify-send done"} {
		if !strings.Contains(got, want) {
			t.Errorf("settings = %s, missing %q", got, want)
		}
	}
}

func TestRemovePreservesAMarkedGroupWithAnExtraKey(t *testing.T) {
	command := testCommand(t)
	settings := `{"hooks":{"SessionEnd":[{"wake":true,"matcher":"*","hooks":[{"type":"command","command":` + quote(command) + `}]}]}}`
	paths, claudeDir := settingsFixture(t, settings)

	removed, keptOwned, err := removeHooks(paths, claudeDir)
	if err != nil {
		t.Fatalf("removeHooks() error = %v", err)
	}

	if removed != 0 {
		t.Errorf("removed = %d, want 0 — a key Wake never writes means the group was edited", removed)
	}
	if keptOwned != 1 {
		t.Errorf("keptOwned = %d, want 1", keptOwned)
	}
	if got := readSettingsFile(t, claudeDir); !strings.Contains(got, `"matcher"`) {
		t.Errorf("settings = %s, want the user's key preserved", got)
	}
}

// The marker is the JSON boolean true and nothing else. A string, a number or
// false is a value Wake never writes, so the group is not one Wake wrote.
func TestRemoveRejectsAFalseOrStringMarker(t *testing.T) {
	command := testCommand(t)
	for _, marker := range []string{`false`, `"true"`, `1`, `null`} {
		t.Run(marker, func(t *testing.T) {
			settings := `{"hooks":{"SessionEnd":[{"wake":` + marker + `,"hooks":[{"type":"command","command":` + quote(command) + `}]}]}}`
			paths, claudeDir := settingsFixture(t, settings)

			removed, _, err := removeHooks(paths, claudeDir)
			if err != nil {
				t.Fatalf("removeHooks() error = %v", err)
			}

			if removed != 0 {
				t.Errorf("removed = %d, want 0", removed)
			}
			if got := readSettingsFile(t, claudeDir); !strings.Contains(got, command) {
				t.Errorf("settings = %s, want the group preserved", got)
			}
		})
	}
}

func TestRemoveRemovesTheGroupInstallWrote(t *testing.T) {
	command := testCommand(t)
	paths, claudeDir := settingsFixture(t, "")
	if _, err := installHooks(paths, claudeDir, command); err != nil {
		t.Fatalf("installHooks() error = %v", err)
	}

	removed, keptOwned, err := removeHooks(paths, claudeDir)
	if err != nil {
		t.Fatalf("removeHooks() error = %v", err)
	}

	if removed != len(hookEvents) {
		t.Errorf("removed = %d, want %d — one group per event", removed, len(hookEvents))
	}
	if keptOwned != 0 {
		t.Errorf("keptOwned = %d, want 0", keptOwned)
	}
	if got := readSettingsFile(t, claudeDir); strings.Contains(got, `"hooks"`) {
		t.Errorf("settings = %s, want hooks gone when it held nothing else", got)
	}
}

// Every setting Wake does not own round-trips untouched, including ones this build
// has never heard of. Installing a hook must not be a way to lose a user's model
// choice or their own hooks.
func TestInstallPreservesUnknownSettingsAndThirdPartyHooks(t *testing.T) {
	command := testCommand(t)
	settings := `{"model":"opus","permissions":{"allow":["Bash"]},"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"lint"}]}]}}`
	paths, claudeDir := settingsFixture(t, settings)

	written, err := installHooks(paths, claudeDir, command)
	if err != nil {
		t.Fatalf("installHooks() error = %v", err)
	}
	if written != len(hookEvents) {
		t.Errorf("written = %d, want %d", written, len(hookEvents))
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(readSettingsFile(t, claudeDir)), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(settings), &want); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got["model"] != want["model"] {
		t.Errorf("model = %v, want %v", got["model"], want["model"])
	}
	if !sameJSON(t, got["permissions"], want["permissions"]) {
		t.Errorf("permissions = %v, want %v", got["permissions"], want["permissions"])
	}
	gotHooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks = %v, want an object", got["hooks"])
	}
	wantHooks, _ := want["hooks"].(map[string]any)
	if !sameJSON(t, gotHooks["PreToolUse"], wantHooks["PreToolUse"]) {
		t.Errorf("PreToolUse = %v, want %v", gotHooks["PreToolUse"], wantHooks["PreToolUse"])
	}
	for _, event := range hookEvents {
		if gotHooks[event] == nil {
			t.Errorf("hooks[%q] is absent; Wake's group was not installed", event)
		}
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	command := testCommand(t)
	paths, claudeDir := settingsFixture(t, "")
	if _, err := installHooks(paths, claudeDir, command); err != nil {
		t.Fatalf("installHooks() error = %v", err)
	}
	before := readSettingsFile(t, claudeDir)

	written, err := installHooks(paths, claudeDir, command)
	if err != nil {
		t.Fatalf("installHooks() error = %v", err)
	}

	if written != 0 {
		t.Errorf("written = %d, want 0 on a re-run", written)
	}
	if after := readSettingsFile(t, claudeDir); after != before {
		t.Errorf("settings changed on a re-run:\ngot:  %s\nwant: %s", after, before)
	}
}

// Re-running `wake init` after moving or reinstalling the binary is what repoints
// the hook. The old group is Wake's own, so it is replaced rather than added
// beside — two Wake groups would run two scans per session.
func TestInstallCorrectsItsOwnGroupWhenTheBinaryMoved(t *testing.T) {
	first, second := testCommand(t), testCommand(t)
	if first == second {
		t.Fatal("the two fixtures produced the same command; the test cannot tell them apart")
	}
	paths, claudeDir := settingsFixture(t, "")
	if _, err := installHooks(paths, claudeDir, first); err != nil {
		t.Fatalf("installHooks() error = %v", err)
	}

	if _, err := installHooks(paths, claudeDir, second); err != nil {
		t.Fatalf("installHooks() error = %v", err)
	}

	got := readSettingsFile(t, claudeDir)
	if strings.Contains(got, first) {
		t.Errorf("settings = %s, still carries the old command", got)
	}
	if strings.Count(got, second) != len(hookEvents) {
		t.Errorf("settings = %s, want exactly one group per event carrying the new command", got)
	}
	installed, err := HookState(claudeDir)
	if err != nil {
		t.Fatalf("HookState() error = %v", err)
	}
	if installed != len(hookEvents) {
		t.Errorf("HookState() = %d, want %d", installed, len(hookEvents))
	}
}

// A settings file this build cannot edit is refused with a message that says what
// was expected, and the file is left exactly as it was. The failure that matters
// is the nil map: `null` unmarshals into a map without error and leaves it nil,
// and a write into a nil map panics.
func TestInstallAndRemoveReturnAnActionableErrorForNullSettings(t *testing.T) {
	command := testCommand(t)
	for _, content := range []string{
		`null`,
		`{"hooks":null}`,
		`{"hooks":{"SessionEnd":null}}`,
		`{"hooks":{"SessionEnd":{}}}`,
		`{"hooks":5}`,
		`[]`,
		`"text"`,
		`{not json`,
	} {
		t.Run(content, func(t *testing.T) {
			paths, claudeDir := settingsFixture(t, content)

			_, installErr := installHooks(paths, claudeDir, command)
			_, _, removeErr := removeHooks(paths, claudeDir)

			for name, err := range map[string]error{"installHooks": installErr, "removeHooks": removeErr} {
				if err == nil {
					t.Errorf("%s() error = nil, want a refusal", name)
					continue
				}
				message := err.Error()
				if !strings.Contains(message, "JSON") && !strings.Contains(message, "object") && !strings.Contains(message, "array") {
					t.Errorf("%s() error = %q, want a message naming what was expected", name, message)
				}
			}
			if got := readSettingsFile(t, claudeDir); got != content {
				t.Errorf("settings = %s, want the file left as it was (%s)", got, content)
			}
		})
	}
}

func TestHookCommandForAcceptsADocumentedInstallation(t *testing.T) {
	path := testExecutable(t)

	got, err := hookCommandFor(path)
	if err != nil {
		t.Fatalf("hookCommandFor() error = %v", err)
	}

	if want := path + " ingest --quiet"; got != want {
		t.Errorf("hookCommandFor() = %q, want %q", got, want)
	}
}

// The resolved target, not the link: a hook command that is a symlink stops
// working the day the link moves, silently.
func TestHookCommandForResolvesASymlinkedInstallation(t *testing.T) {
	target := testExecutable(t)
	link := filepath.Join(t.TempDir(), "wake-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := hookCommandFor(link)
	if err != nil {
		t.Fatalf("hookCommandFor() error = %v", err)
	}

	if !strings.HasPrefix(got, target+" ") {
		t.Errorf("hookCommandFor() = %q, want the resolved target %q", got, target)
	}
}

// An installation whose path cannot be written as an unquoted hook command is
// unsupported, and saying so is more useful than writing a hook that will not run.
//
// The message names the requirement and never the path it refused. Each fixture
// below carries a distinctive marker in its name so the check is real: the word
// "wake" would appear in any honest instruction about where to install wake, and
// asserting the base name is absent would only be asserting that the instruction is
// missing.
func TestHookCommandForRejectsAnUnsupportedInstallation(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temporary directory: %v", err)
	}
	const marker = "unmistakable"
	notExecutable := filepath.Join(dir, marker+"-not-executable")
	if err := os.WriteFile(notExecutable, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	withSpace := filepath.Join(dir, marker+" binary")
	withQuote := filepath.Join(dir, marker+"'binary")
	withNewline := filepath.Join(dir, marker+"\nbinary")
	for _, path := range []string{withSpace, withQuote, withNewline} {
		if err := os.WriteFile(path, []byte("x"), 0o700); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", path, err)
		}
	}

	for name, path := range map[string]string{
		"empty":           "",
		"relative":        marker + "/wake",
		"dot relative":    "./" + marker,
		"parent relative": "../" + marker,
		"home relative":   "~/" + marker,
		"directory":       dir,
		"absent":          filepath.Join(dir, marker+"-absent"),
		"not executable":  notExecutable,
		"space":           withSpace,
		"single quote":    withQuote,
		"newline":         withNewline,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := hookCommandFor(path)
			if err == nil {
				t.Fatalf("hookCommandFor(%q) = %q, want a refusal", path, got)
			}
			message := err.Error()
			if path != "" && strings.Contains(message, path) {
				t.Errorf("error %q names the path it refused", message)
			}
			if strings.Contains(message, marker) {
				t.Errorf("error %q carries a fragment of the path it refused", message)
			}
		})
	}
}

func quote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("encoding a test fixture: " + err.Error())
	}
	return string(encoded)
}

func sameJSON(t *testing.T, left, right any) bool {
	t.Helper()
	encodedLeft, err := json.Marshal(left)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	encodedRight, err := json.Marshal(right)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return string(encodedLeft) == string(encodedRight)
}
