package activation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

func TestInitPreservesHooksAndImportsOnlyConsentedHistory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() root error = %v", err)
	}
	claudeDir := filepath.Join(t.TempDir(), "claude")
	settings := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"existing command"}]}]}}`
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() Claude dir error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatalf("WriteFile() settings error = %v", err)
	}
	transcriptDir := filepath.Join(claudeDir, "projects", "project")
	if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() transcript error = %v", err)
	}
	transcript := `{"uuid":"entry-1","sessionId":"session-1","cwd":"` + root + `","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}
{"uuid":"entry-2","sessionId":"session-1","cwd":"` + root + `","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`
	if err := os.WriteFile(filepath.Join(transcriptDir, "session.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatalf("WriteFile() transcript error = %v", err)
	}
	paths := testPaths(t)
	executable := testExecutable(t)
	command, err := hookCommandFor(executable)
	if err != nil {
		t.Fatalf("hookCommandFor() error = %v", err)
	}

	written, err := Init(paths, root, claudeDir, executable)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if written != 1 {
		t.Fatalf("Init() wrote %d events, want 1", written)
	}
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("store entries = %d, error = %v", len(entries), err)
	}
	var persisted map[string]any
	raw, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil || json.Unmarshal(raw, &persisted) != nil {
		t.Fatalf("reading settings failed: %v", err)
	}
	if string(raw) == settings || !containsCommand(raw, "existing command") || !containsCommand(raw, command) {
		t.Fatalf("settings lost existing or Wake hook: %s", raw)
	}
	second, err := Init(paths, root, claudeDir, executable)
	if err != nil || second != 0 {
		t.Fatalf("second Init() = %d, %v; want 0, nil", second, err)
	}
}

func TestRebuildReplacesOnlyTheDerivedEventStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() root error = %v", err)
	}
	claudeDir := filepath.Join(t.TempDir(), "claude")
	transcriptDir := filepath.Join(claudeDir, "projects", "project")
	if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() transcript error = %v", err)
	}
	transcript := `{"uuid":"entry-1","sessionId":"session-1","cwd":"` + root + `","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}
{"uuid":"entry-2","sessionId":"session-1","cwd":"` + root + `","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`
	if err := os.WriteFile(filepath.Join(transcriptDir, "session.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatalf("WriteFile() transcript error = %v", err)
	}
	paths := testPaths(t)
	if _, err := Init(paths, root, claudeDir, testExecutable(t)); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	projectsBefore, err := os.ReadFile(paths.ProjectsFile)
	if err != nil {
		t.Fatalf("ReadFile() projects error = %v", err)
	}
	if _, rebuildErr := Rebuild(paths, claudeDir); rebuildErr != nil {
		t.Fatalf("Rebuild() error = %v", rebuildErr)
	}
	projectsAfter, err := os.ReadFile(paths.ProjectsFile)
	if err != nil || string(projectsBefore) != string(projectsAfter) {
		t.Fatalf("Rebuild() changed consented projects: %v", err)
	}
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("rebuilt entries = %d, error = %v", len(entries), err)
	}
}

func TestUninstallRemovesOnlyWakeHooksAndKeepsData(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), "claude")
	// The SessionStart marked group carries a command this build would never write,
	// and it is still removed: ownership is the marker plus the group's shape, never
	// the command string, so a group an earlier build wrote stays recognisable.
	//
	// The SessionEnd group carries Wake's exact command with no marker, and it is
	// preserved: command equality is not proof of ownership, and a user who copied
	// that command into their own group owns it. That is the mixed-group case the
	// ticket names, and hooks_test.go covers it on its own.
	command, commandErr := hookCommandFor(testExecutable(t))
	if commandErr != nil {
		t.Fatalf("hookCommandFor() error = %v", commandErr)
	}
	settings := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"existing command"}]},{"wake":true,"hooks":[{"type":"command","command":"changed wake command"}]}],"SessionEnd":[{"hooks":[{"type":"command","command":` + quote(command) + `}]}]}}`
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	paths := testPaths(t)
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() data error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(paths.DataDir, "events.ndjson"), []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile() data error = %v", err)
	}

	removed, uninstallErr := Uninstall(paths, claudeDir, false)
	if uninstallErr != nil || !removed {
		t.Fatalf("Uninstall() = %t, %v", removed, uninstallErr)
	}
	raw, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil || !containsCommand(raw, "existing command") || !containsCommand(raw, command) || contains(string(raw), `"wake": true`) {
		t.Fatalf("settings after uninstall = %s, error = %v", raw, err)
	}
	if _, statErr := os.Stat(filepath.Join(paths.DataDir, "events.ndjson")); statErr != nil {
		t.Fatalf("Uninstall() removed data: %v", statErr)
	}
	second, err := Uninstall(paths, claudeDir, false)
	if err != nil || second {
		t.Fatalf("second Uninstall() = %t, %v", second, err)
	}
}

func TestUninstallPurgesOnlyDataRoot(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(paths.DataDir, "events.ndjson"), []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := Uninstall(paths, filepath.Join(t.TempDir(), "claude"), true); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if _, err := os.Stat(paths.DataDir); !os.IsNotExist(err) {
		t.Fatalf("data root still exists: %v", err)
	}
}

func TestDiscoveryScopeWithholdsProjectDiscoveryOutsideAConsentedRepository(t *testing.T) {
	paths := testPaths(t)
	claudeDir := filepath.Join(t.TempDir(), "claude")
	root := t.TempDir()

	got, _ := DiscoveryScope(paths, claudeDir, root)
	want := inventory.Scope{ClaudeDir: claudeDir, Project: inventory.ProjectUnconsented}
	if got != want {
		t.Fatalf("DiscoveryScope() = %+v, want %+v", got, want)
	}
}

func TestDiscoveryScopeGrantsProjectDiscoveryInsideAConsentedRepository(t *testing.T) {
	paths := testPaths(t)
	claudeDir := filepath.Join(t.TempDir(), "claude")
	root := t.TempDir()
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	if _, err := repos.Register(root, filepath.Base(root)); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	got, _ := DiscoveryScope(paths, claudeDir, root)
	want := inventory.Scope{ClaudeDir: claudeDir, Root: root, Project: inventory.ProjectConsented}
	if got != want {
		t.Fatalf("DiscoveryScope() = %+v, want %+v", got, want)
	}
}

func TestDiscoveryScopeResolvesTheConsentedRootFromASubdirectory(t *testing.T) {
	paths := testPaths(t)
	claudeDir := filepath.Join(t.TempDir(), "claude")
	root := t.TempDir()
	inside := filepath.Join(root, "packages", "api")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	if _, registerErr := repos.Register(root, filepath.Base(root)); registerErr != nil {
		t.Fatalf("Register() error = %v", registerErr)
	}

	// Consent was given for the repository, not for the directory the command
	// happens to run in. Scoping discovery to the subdirectory would read only
	// part of the repository's primitives and then, being a complete pass, replace
	// the snapshot with that part.
	got, _ := DiscoveryScope(paths, claudeDir, inside)
	want := inventory.Scope{ClaudeDir: claudeDir, Root: root, Project: inventory.ProjectConsented}
	if got != want {
		t.Fatalf("DiscoveryScope() = %+v, want %+v", got, want)
	}
}

func TestIngestFromASubdirectoryInventoriesTheWholeRepository(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	inside := filepath.Join(root, "packages", "api")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if _, err := Init(paths, root, claudeDir, testExecutable(t)); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Chdir(inside)

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	raw, err := os.ReadFile(paths.PrimitivesFile)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !contains(string(raw), "local-skill") || !contains(string(raw), "global-skill") {
		t.Fatalf("a scan from inside the repository lost its primitives: %s", raw)
	}
}

func TestDiscoveryScopeFailsClosedWhenConsentCannotBeResolved(t *testing.T) {
	paths := testPaths(t)
	claudeDir := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(filepath.Dir(paths.ProjectsFile), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(paths.ProjectsFile, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, _ := DiscoveryScope(paths, claudeDir, t.TempDir())
	want := inventory.Scope{ClaudeDir: claudeDir, Project: inventory.ProjectUnresolved}
	if got != want {
		t.Fatalf("DiscoveryScope() = %+v, want %+v", got, want)
	}
}

func TestDiscoveryScopeWithholdsProjectDiscoveryForARelativeDirectory(t *testing.T) {
	paths := testPaths(t)
	claudeDir := filepath.Join(t.TempDir(), "claude")

	got, _ := DiscoveryScope(paths, claudeDir, "relative/dir")
	want := inventory.Scope{ClaudeDir: claudeDir, Project: inventory.ProjectUnresolved}
	if got != want {
		t.Fatalf("DiscoveryScope() = %+v, want %+v", got, want)
	}
}

func TestIngestDoesNotInventoryAnUnconsentedProject(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	t.Chdir(root)

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	raw, err := os.ReadFile(paths.PrimitivesFile)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !contains(string(raw), "global-skill") {
		t.Fatalf("primitives.json lost global discovery: %s", raw)
	}
	if contains(string(raw), "local-skill") {
		t.Fatalf("primitives.json inventoried an unconsented project: %s", raw)
	}
	if contains(string(raw), root) {
		t.Fatalf("primitives.json contains the working directory: %s", raw)
	}
}

func TestInitInventoriesTheProjectItJustConsented(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)

	if _, err := Init(paths, root, claudeDir, testExecutable(t)); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	raw, err := os.ReadFile(paths.PrimitivesFile)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !contains(string(raw), "global-skill") || !contains(string(raw), "local-skill") {
		t.Fatalf("primitives.json = %s", raw)
	}
}

// The counter file is derived and non-precious (ADR-0014), and doctor already
// renders an unreadable one as a state rather than an error. A command that writes
// counters must agree: a health.json this build cannot read is not a reason to refuse
// to import history, install a trigger, or uninstall one — and the recovery, deleting
// a file nothing tells the user about, would be undiscoverable.
func TestAnUnreadableCounterFileDoesNotStopAnyCommand(t *testing.T) {
	for _, name := range []string{"init", "ingest", "remove"} {
		t.Run(name, func(t *testing.T) {
			paths := testPaths(t)
			claudeDir, root := inventoryFixture(t)
			writeFixture(t, paths.HealthFile, `{"version":99}`)

			var err error
			switch name {
			case "init":
				_, err = Init(paths, root, claudeDir, testExecutable(t))
			case "ingest":
				if _, initErr := Init(paths, root, claudeDir, testExecutable(t)); initErr != nil {
					t.Fatalf("Init() error = %v", initErr)
				}
				writeFixture(t, paths.HealthFile, `{"version":99}`)
				t.Chdir(root)
				_, err = Ingest(paths, claudeDir)
			case "remove":
				if _, initErr := Init(paths, root, claudeDir, testExecutable(t)); initErr != nil {
					t.Fatalf("Init() error = %v", initErr)
				}
				writeFixture(t, paths.HealthFile, `{"version":99}`)
				var removed bool
				removed, err = Uninstall(paths, claudeDir, false)
				if err == nil && !removed {
					t.Error("Uninstall() = false, want true — the hooks were installed")
				}
			}
			if err != nil {
				t.Fatalf("%s error = %v, want the command to run and the counters to be replaced", name, err)
			}

			if _, readErr := health.New(paths.HealthFile).Read(); readErr != nil {
				t.Errorf("the counter file is still unreadable after %s: %v", name, readErr)
			}
		})
	}
}

// A counter write that fails for a reason of its own — an unwritable data root, a
// file this user does not own — happens after the hooks are already gone. It may
// surface, since a local layout this build cannot write to is worth knowing about,
// but it must not turn what did happen into a report that nothing did.
func TestUninstallStillReportsTheHooksItRemovedWhenTheCountersCannotBeWritten(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	if _, err := Init(paths, root, claudeDir, testExecutable(t)); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	// Read-only, so creating the counter file's lock fails. Restored afterwards, or
	// t.TempDir's own cleanup cannot remove it.
	if err := os.Chmod(paths.DataDir, 0o500); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(paths.DataDir, 0o700); err != nil {
			t.Errorf("restoring the data root: %v", err)
		}
	})

	// The error is not asserted: running as root, or on a filesystem that ignores
	// the mode, the write succeeds and there is nothing to report. What is asserted
	// holds either way.
	removed, err := Uninstall(paths, claudeDir, false)

	if !removed {
		t.Errorf("Uninstall() = false, error = %v — the hooks were removed and the report says they were not", err)
	}
	state, hookErr := HookState(claudeDir)
	if hookErr != nil || state != 0 {
		t.Errorf("HookState() = %d, %v — want no Wake group left", state, hookErr)
	}
}

// inventoryFixture writes a Claude directory holding one global skill and a
// project directory holding one project-local skill, so a snapshot names which
// discovery path produced it.
func inventoryFixture(t *testing.T) (claudeDir, root string) {
	t.Helper()
	claudeDir = filepath.Join(t.TempDir(), "claude")
	root = filepath.Join(t.TempDir(), "project")
	writeFixture(t, filepath.Join(claudeDir, "skills", "global-skill", "SKILL.md"), "# global")
	writeFixture(t, filepath.Join(root, ".claude", "skills", "local-skill", "SKILL.md"), "# local")
	transcript := `{"uuid":"entry-1","sessionId":"session-1","cwd":"` + root + `","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}
{"uuid":"entry-2","sessionId":"session-1","cwd":"` + root + `","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`
	writeFixture(t, filepath.Join(claudeDir, "projects", "project", "session.jsonl"), transcript)
	return claudeDir, root
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	paths := config.Paths{ConfigDir: filepath.Join(t.TempDir(), "config"), DataDir: filepath.Join(t.TempDir(), "data")}
	paths.ConfigFile = filepath.Join(paths.ConfigDir, "config.toml")
	paths.SaltFile = filepath.Join(paths.ConfigDir, "salt.bin")
	paths.ProjectsFile = filepath.Join(paths.DataDir, "projects.bin")
	paths.PrimitivesFile = filepath.Join(paths.DataDir, "primitives.json")
	paths.HealthFile = filepath.Join(paths.DataDir, "health.json")
	return paths
}

func containsCommand(raw []byte, command string) bool {
	return string(raw) != "" && string(raw) != command && contains(string(raw), command)
}

func contains(value, want string) bool {
	for index := 0; index+len(want) <= len(value); index++ {
		if value[index:index+len(want)] == want {
			return true
		}
	}
	return false
}
