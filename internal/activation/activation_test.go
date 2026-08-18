package activation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/record"
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

// TestIngestSurfacesPendingAndInterruptedCalls is the ticket's end-to-end
// criterion: a call left unterminated by a dead session eventually reaches the
// store as outcome interrupted, and a call whose session may still be running stays
// buffered and is reported as pending instead of vanishing.
//
// The two transcripts pick their timestamps rather than fixing them. The stale one
// is deliberately historical, so the provisional 24h default resolves it on any sane
// clock; the live one is generated from time.Now, so it is inside the window by
// construction. A fixed near-today date would make this test's meaning depend on
// when it runs, and the threshold itself must never be shortened to make the
// feature easier to demonstrate — an interrupted record cannot be taken back.
func TestIngestSurfacesPendingAndInterruptedCalls(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() root error = %v", err)
	}
	claudeDir := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() Claude dir error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile() settings error = %v", err)
	}
	transcriptDir := filepath.Join(claudeDir, "projects", "project")
	if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() transcript error = %v", err)
	}
	// The stale transcript carries the per-session bookkeeping lines a real Claude Code
	// transcript is full of — ai-title, last-prompt, queue-operation, none of them a
	// transcript entry and none carrying a uuid. They are here because the reader
	// counts them as lines it had no entry for, and a staleness rule gated on that
	// count would resolve nothing on any real machine while a fixture of clean entries
	// stayed green. This test is the end-to-end criterion, so it reads like real input.
	stale := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-stale","cwd":"` + root + `","timestamp":"2020-01-01T00:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"type":"ai-title","aiTitle":"an old session","sessionId":"session-stale"}`,
		`{"type":"last-prompt","lastPrompt":"run it","leafUuid":"entry-1","sessionId":"session-stale"}`,
		`{"type":"queue-operation","operation":"enqueue","sessionId":"session-stale","timestamp":"2020-01-01T00:00:00Z"}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(transcriptDir, "stale.jsonl"), []byte(stale), 0o600); err != nil {
		t.Fatalf("WriteFile() stale transcript error = %v", err)
	}
	live := `{"uuid":"entry-2","sessionId":"session-live","cwd":"` + root + `","timestamp":"` + time.Now().UTC().Format(time.RFC3339) + `","message":{"content":[{"type":"tool_use","id":"call-2","name":"Read"}]}}`
	if err := os.WriteFile(filepath.Join(transcriptDir, "live.jsonl"), []byte(live), 0o600); err != nil {
		t.Fatalf("WriteFile() live transcript error = %v", err)
	}
	paths := testPaths(t)

	written, err := Init(paths, root, claudeDir, testExecutable(t))
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if written != 1 {
		t.Fatalf("Init() wrote %d events, want 1 — the stale call resolved and the live one did not", written)
	}

	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if report.Scan.InterruptedCalls != 1 {
		t.Errorf("Scan.InterruptedCalls = %d, want 1", report.Scan.InterruptedCalls)
	}
	if report.Scan.PendingCalls != 1 {
		t.Errorf("Scan.PendingCalls = %d, want 1 — a call whose session may still be running is not lost", report.Scan.PendingCalls)
	}

	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Entries() = %+v, want exactly one record", entries)
	}
	if outcome := entries[0].Record.Outcome; outcome == nil || *outcome != record.OutcomeInterrupted {
		t.Fatalf("Outcome = %v, want interrupted", outcome)
	}
}

// TestIngestSurfacesAmbiguousSkillRuns is the other half of the same criterion at the
// session grain: several attributed end_turn entries for one skill in one session are
// one invocation in the store and a count of what was collapsed in the health report.
// The counter is uncertainty about that number and never a second invocation
// (ADR-0023's accepted limitation).
//
// The transcript is deliberately historical, so the provisional 24h default closes its
// session on any sane clock — the threshold itself must never be shortened to make the
// feature easier to demonstrate.
func TestIngestSurfacesAmbiguousSkillRuns(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() root error = %v", err)
	}
	claudeDir := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() Claude dir error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile() settings error = %v", err)
	}
	transcriptDir := filepath.Join(claudeDir, "projects", "project")
	if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() transcript error = %v", err)
	}
	transcript := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"` + root + `","timestamp":"2020-01-01T00:00:00Z","attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"` + root + `","timestamp":"2020-01-01T00:00:01Z","attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"` + root + `","timestamp":"2020-01-01T00:00:02Z","attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(transcriptDir, "slash-commands.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatalf("WriteFile() transcript error = %v", err)
	}
	paths := testPaths(t)

	written, err := Init(paths, root, claudeDir, testExecutable(t))
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if written != 1 {
		t.Fatalf("Init() wrote %d events, want 1 — three attributed runs of one skill in one session are one record", written)
	}

	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if report.Scan.AmbiguousSkillRuns != 2 {
		t.Errorf("Scan.AmbiguousSkillRuns = %d, want 2 — every candidate collapsed after the first", report.Scan.AmbiguousSkillRuns)
	}

	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Entries() = %+v, want exactly one record", entries)
	}
	// An end_turn entry describes no result, and unknown is never success (ADR-0005).
	if outcome := entries[0].Record.Outcome; outcome != nil {
		t.Errorf("Outcome = %v, want none", outcome)
	}
}

// stalenessFixture activates paths on root and leaves one unterminated call, aged
// `age` before now, as the only thing in Claude Code's history. Every test below turns
// on whether that one call resolves.
func stalenessFixture(t *testing.T, age time.Duration) (config.Paths, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() root error = %v", err)
	}
	claudeDir := filepath.Join(t.TempDir(), "claude")
	transcriptDir := filepath.Join(claudeDir, "projects", "project")
	if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() transcript error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile() settings error = %v", err)
	}
	line := `{"uuid":"entry-1","sessionId":"session-1","cwd":"` + root + `","timestamp":"` +
		time.Now().UTC().Add(-age).Format(time.RFC3339) +
		`","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`
	if err := os.WriteFile(filepath.Join(transcriptDir, "session-1.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatalf("WriteFile() transcript error = %v", err)
	}

	paths := testPaths(t)
	if _, err := Init(paths, root, claudeDir, testExecutable(t)); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	return paths, claudeDir
}

// scanOf reads back what the last scan recorded, which is where doctor reads it from.
func scanOf(t *testing.T, paths config.Paths) health.Scan {
	t.Helper()
	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	return report.Scan
}

func TestIngestResolvesAStaleCallUnderTheConfiguredThreshold(t *testing.T) {
	// The threshold is the config value, not a constant in the reader (ADR-0015: it
	// "belongs in config, not in constants"). A call one minute old is well inside the
	// 24h default and stays buffered; the same call resolves once the user shortens the
	// key. If the reader ever hard-coded a threshold, one of these two halves would fail.
	paths, claudeDir := stalenessFixture(t, time.Minute)

	if scan := scanOf(t, paths); scan.PendingCalls != 1 || scan.InterruptedCalls != 0 {
		t.Fatalf("under the default threshold: Scan = %+v, want the call pending", scan)
	}

	if _, err := config.Set(paths, "scan.stale_call_timeout", "1s"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	scan := scanOf(t, paths)
	if scan.InterruptedCalls != 1 || scan.PendingCalls != 0 {
		t.Fatalf("under a 1s threshold: Scan = %+v, want the call resolved", scan)
	}
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Entries() = %+v, want exactly one record", entries)
	}
	if outcome := entries[0].Record.Outcome; outcome == nil || *outcome != record.OutcomeInterrupted {
		t.Fatalf("Outcome = %v, want interrupted", outcome)
	}
}

func TestIngestResolvesAStaleCallOnlyOnce(t *testing.T) {
	// Re-scanning re-derives the same event id from the same source event, so the store
	// deduplicates the second copy (ADR-0004). That is what makes the rule safe to be
	// final: it cannot accumulate a record per scan.
	paths, claudeDir := stalenessFixture(t, 72*time.Hour)

	for scanNumber := range 3 {
		if _, err := Ingest(paths, claudeDir); err != nil {
			t.Fatalf("Ingest() %d error = %v", scanNumber, err)
		}
		entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
		if err != nil {
			t.Fatalf("Entries() error = %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("after scan %d: Entries() = %+v, want exactly one record", scanNumber, entries)
		}
	}
}

func TestIngestKeepsCollectingWhenTheConfigFileCannotBeRead(t *testing.T) {
	// A scan that cannot read its own threshold buffers rather than guessing one:
	// "could not read" means "collects nothing", never an error that breaks a command
	// (plan §4.3), and an interrupted record written under a threshold the user never
	// chose is permanent while a buffered call is resolved by any later scan.
	paths, claudeDir := stalenessFixture(t, 72*time.Hour)
	if err := os.WriteFile(paths.ConfigFile, []byte("{not toml"), 0o600); err != nil {
		t.Fatalf("WriteFile() config error = %v", err)
	}

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v, want the scan to keep collecting", err)
	}

	scan := scanOf(t, paths)
	if scan.PendingCalls != 1 || scan.InterruptedCalls != 0 {
		t.Fatalf("Scan = %+v, want the call left buffered", scan)
	}
}

func TestActivationNeverReadsTheSessionIdleTimeout(t *testing.T) {
	// ADR-0014 scopes session.idle_timeout to session-end inference — the session
	// grain's ended_at — which is a different question from whether an unresolved call
	// may be resolved. ADR-0023 §3 allows exactly one threshold for that,
	// scan.stale_call_timeout, so reading the other one here would be the "second
	// threshold" it forbids. A grep is the cheapest guard, in the same spirit as
	// internal/config's walk for its salt name.
	//
	// The quoted form is what it looks for, not the bare name: a key can only reach
	// config as a string literal, so the quotes are the difference between reading the
	// tunable and documenting that this package does not.
	const readingTheKey = `"session.idle_timeout"`
	sources, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range sources {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || entry.Name() == "activation_test.go" {
			continue
		}
		body, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", entry.Name(), err)
		}
		if strings.Contains(string(body), readingTheKey) {
			t.Errorf("%s reads %s; the staleness rule reads scan.stale_call_timeout only", entry.Name(), readingTheKey)
		}
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
