package activation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
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

	written, err := Init(paths, root, claudeDir, executable, true)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	// Two: the consented call, and the session_end for the session id that made it
	// (ADR-0034). The unconsented history is what this test is about, and none of it
	// contributes either record.
	if written != 2 {
		t.Fatalf("Init() wrote %d events, want 2", written)
	}
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil || len(entries) != 2 {
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
	second, err := Init(paths, root, claudeDir, executable, true)
	if err != nil || second != 0 {
		t.Fatalf("second Init() = %d, %v; want 0, nil", second, err)
	}
	// The second --full pass re-scans the same source and writes nothing twice.
	// event_id dedup in internal/store is the only mechanism (ADR-0004); no
	// watermark is involved, which is what makes a re-scan safe rather than merely
	// cheap.
	entries, err = store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("after a second full init: entries = %d, error = %v; want 2", len(entries), err)
	}
}

// A plain `wake init` does not walk harness history at all.
//
// Asserted with a call counter rather than an empty result, because the two are the
// same number: a walk over a machine with no transcripts also returns 0. The
// fixture deliberately holds one terminal call, so an accidental walk would be
// visible — and the --full leg at the end is the positive control that proves the
// counter can move, without which the zero above proves nothing.
func TestInitWithoutFullNeverWalksHarnessHistory(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	original := importHistory
	t.Cleanup(func() { importHistory = original })
	walks := 0
	importHistory = func(repos *config.Repos, claudeDir string, destination *store.Store, installed claudecode.Installed, stale claudecode.Staleness, idle claudecode.Idleness, scope collectionScope, discover *boundaryDiscovery) (int, health.Scan, error) {
		walks++
		return original(repos, claudeDir, destination, installed, stale, idle, scope, discover)
	}

	written, err := Init(paths, root, claudeDir, testExecutable(t), false)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if walks != 0 {
		t.Errorf("Init() entered the history walk %d times, want 0", walks)
	}
	if written != 0 {
		t.Errorf("Init() wrote %d events, want 0", written)
	}
	// The spool is never created, which is the same fact from the filesystem's side.
	if _, statErr := os.Stat(filepath.Join(paths.DataDir, eventsFile)); !os.IsNotExist(statErr) {
		t.Errorf("Stat(spool) = %v, want it never created", statErr)
	}
	// Registration, the consented list and the triggers are unconditional (ADR-0016
	// layer 2, ADR-0010): only the import is gated.
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	consented, err := repos.ConsentedRoot(root)
	if err != nil || consented != root {
		t.Errorf("ConsentedRoot() = %q, %v; want %q", consented, err, root)
	}
	identity, err := repos.Identify(root)
	if err != nil {
		t.Fatalf("Identify() error = %v", err)
	}
	settings, err := config.Load(paths)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	list, err := settings.StringList("scan.repos")
	if err != nil || !slices.Contains(list, identity.ID) {
		t.Errorf("scan.repos = %v, %v; want it to hold the registered id", list, err)
	}
	if state, hookErr := HookState(claudeDir); hookErr != nil || state != 2 {
		t.Errorf("HookState() = %d, %v; want 2 installed triggers", state, hookErr)
	}
	// The hook counters are recorded and the scan counters are not: a zero-valued
	// health.Scan would read as "collects zero" for a state nobody measured, and
	// doctor must be able to say "never scanned" instead (ADR-0010).
	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if report.Hooks.Installed != 2 {
		t.Errorf("Hooks.Installed = %d, want 2", report.Hooks.Installed)
	}
	if !report.Scan.At.IsZero() {
		t.Errorf("Scan = %+v, want no scan recorded at all", report.Scan)
	}

	// A second plain init on an already-consented repo is still a no-op, and still
	// does not walk.
	second, err := Init(paths, root, claudeDir, testExecutable(t), false)
	if err != nil || second != 0 || walks != 0 {
		t.Fatalf("second plain Init() = %d, %v with %d walks; want 0, nil, 0", second, err, walks)
	}

	// The positive control: the same counter moves under --full, and the history
	// the default path skipped is still there to be had.
	full, err := Init(paths, root, claudeDir, testExecutable(t), true)
	if err != nil {
		t.Fatalf("Init(full) error = %v", err)
	}
	if walks != 1 || full != 2 {
		t.Fatalf("Init(full) = %d events after %d walks; want 2 events after 1 walk", full, walks)
	}
}

// `init --full` on a repository consented forward-only lifts the boundary for good,
// so the trigger stops holding anything back.
//
// The user has asked for the whole history; a boundary still in force afterwards
// would be a filter nothing can clear, quietly narrowing every later scan for a
// repository whose history is already in the spool. The spool is discarded and
// refilled by a trigger on purpose: the assertion is about what the *unattended*
// scan is willing to import, which is the only thing the boundary governs.
func TestAFullInitLiftsTheBoundaryAnEarlierPlainInitRecorded(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	spool := store.New(filepath.Join(paths.DataDir, eventsFile))

	if _, err := Init(paths, root, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("plain Init() error = %v", err)
	}
	if written, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil || written != 2 {
		t.Fatalf("Init(full) = %d, %v; want the declined history imported", written, err)
	}
	if discardErr := spool.Discard(); discardErr != nil {
		t.Fatalf("Discard() error = %v", discardErr)
	}

	if _, err := Trigger(paths, claudeDir); err != nil {
		t.Fatalf("Trigger() error = %v", err)
	}
	entries, err := spool.Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("Entries() = %+v, want the pre-existing call and its session_end — the boundary outlived the full import", entries)
	}
}

// A plain init after a --full leaves the scan counters where the import put them.
//
// This is the reason the default path records no scan at all: RecordScan replaces the
// counters wholesale, so a zero-valued health.Scan written by a repeat `wake init`
// would erase what an earlier import actually found and doctor would report
// "collects zero" for a machine that had collected.
func TestAPlainInitAfterAFullOneKeepsTheEarlierScanCounters(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init(full) error = %v", err)
	}
	before := scanOf(t, paths)
	if before.At.IsZero() || before.EventsWritten != 2 {
		t.Fatalf("Scan = %+v, want the full import's counters", before)
	}

	if _, err := Init(paths, root, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("plain Init() error = %v", err)
	}

	if after := scanOf(t, paths); after != before {
		t.Errorf("Scan = %+v, want it untouched at %+v", after, before)
	}
}

// A repo consented with a plain init and backfilled afterwards ends up
// byte-identical in the store to one that imported from the start.
//
// It is also where the one deliberate asymmetry is pinned: `wake ingest` is the user
// asking for the history, so it ignores the recorded boundary, while the trigger the
// hooks fire honours it (ADR-0025). The boundary is about what happens when nobody
// asked — if this test ever needs a flag to keep passing, the asymmetry has been lost.
//
// Both halves run against one Paths on purpose. A second config directory would
// carry a different salt, so the repo id — a salted hash of the consented root
// (ADR-0019) — would differ and the bytes could not match however correct the code
// was. Same salt, same source events, ids derived from those events (ADR-0004):
// the spool is reproducible, and record.Record carries no write-time field that
// could make it otherwise.
func TestBackfillMatchesAFullInitByteForByte(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	spool := filepath.Join(paths.DataDir, eventsFile)

	if _, err := Init(paths, root, claudeDir, testExecutable(t), false); err != nil {
		t.Fatalf("plain Init() error = %v", err)
	}
	t.Chdir(root)
	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	viaIngest, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() after the backfill error = %v", err)
	}

	if discardErr := store.New(spool).Discard(); discardErr != nil {
		t.Fatalf("Discard() error = %v", discardErr)
	}
	if _, initErr := Init(paths, root, claudeDir, testExecutable(t), true); initErr != nil {
		t.Fatalf("Init(full) error = %v", initErr)
	}
	viaFull, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() after the full init error = %v", err)
	}

	if string(viaIngest) != string(viaFull) {
		t.Errorf("backfilled spool and full-init spool differ:\n%s\n---\n%s", viaIngest, viaFull)
	}
	if len(viaIngest) == 0 {
		t.Fatal("both spools are empty; the fixture proved nothing")
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
	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
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
	if err != nil || len(entries) != 2 {
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
	if _, err := repos.Register(root, filepath.Base(root), time.Time{}); err != nil {
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
	if _, registerErr := repos.Register(root, filepath.Base(root), time.Time{}); registerErr != nil {
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
	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
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

	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
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
				_, err = Init(paths, root, claudeDir, testExecutable(t), true)
			case "ingest":
				if _, initErr := Init(paths, root, claudeDir, testExecutable(t), true); initErr != nil {
					t.Fatalf("Init() error = %v", initErr)
				}
				writeFixture(t, paths.HealthFile, `{"version":99}`)
				t.Chdir(root)
				_, err = Ingest(paths, claudeDir)
			case "remove":
				if _, initErr := Init(paths, root, claudeDir, testExecutable(t), true); initErr != nil {
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
	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
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

// A subagent transcript declaring no name is refused at the closure boundary, not
// while its own lines are read — the name and the agent id are not on the same entry
// (ADR-0036 §2, ADR-0015). Folding only the per-source halves would therefore report
// that lost collection as a clean zero, which is the silence plan §3.3 and §12 exist
// to prevent: doctor would say "collecting" while an invocation nobody counts goes
// missing.
//
// It is counted on its own counter, and it does not move the integration state. Every
// scan re-reads the whole history — there is no incremental cursor yet (T020, T102) —
// re-resolves the same run and refuses it again, and ADR-0036 §2 refuses to ever name
// it, so a state word driven by that count could never change back: a machine that runs
// subagents would read as "collects nothing" for good. Two scans are what asserts it,
// because one scan cannot witness a pin. The number is the report; the state word is
// about whether the numbers can be trusted.
//
// Every transcript's date is deliberately historical, so each session is closed and
// idle under the defaults on any clock this test runs on.
func TestIngestCountsASubagentRefusedAtSessionCloseAndDoesNotPinTheIntegrationState(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	// Three entries sharing one agentId and declaring no attributionAgent anywhere:
	// the 2% shape ADR-0036 §2 measured, which is refused and counted rather than
	// named from the harness's documented default.
	//
	// Its own source, beside the fixture's consented session rather than over it, so the
	// machine is collecting rather than merely not broken: the state assertion below is
	// about a refusal not blinding a working install.
	lines := make([]string, 0, 3)
	for index, at := range []string{"2026-08-13T12:00:00Z", "2026-08-13T12:00:01Z", "2026-08-13T12:00:02Z"} {
		lines = append(lines, `{"uuid":"agent-entry-`+strconv.Itoa(index)+`","sessionId":"session-1","cwd":"`+root+
			`","timestamp":"`+at+`","version":"1.0.0","entrypoint":"cli","isSidechain":true,"agentId":"agent-1",`+
			`"message":{"model":"sonnet","content":[]}}`)
	}
	writeFixture(t, filepath.Join(claudeDir, "projects", "project", "agent.jsonl"), strings.Join(lines, "\n"))
	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for index, pass := range []string{"the first scan", "the second scan"} {
		// A new session for each pass. Every id is derived from the source event
		// (ADR-0004), so re-reading what is already in the spool writes nothing and
		// EventsWritten counts the work done rather than the window — without a new
		// session the second scan would report an honest zero and the state word below
		// would be about that instead of about the refusal.
		transcriptAt(t, claudeDir, "session-later-"+strconv.Itoa(index), root,
			time.Date(2026, 8, 13, 13, 0, index, 0, time.UTC))
		if _, err := Ingest(paths, claudeDir); err != nil {
			t.Fatalf("Ingest() on %s error = %v", pass, err)
		}

		scan := scanCounters(t, paths)
		if scan.RefusedSubagentRuns != 1 {
			t.Errorf("RefusedSubagentRuns after %s = %d, want 1 — a subagent run this build could not name is collection it lost",
				pass, scan.RefusedSubagentRuns)
		}
		// Its own counter, not the drift one: RefusedCalls is what a harness renaming
		// the field a primitive's identity lives in looks like, and it is the counter
		// that still blinds the state word.
		if scan.RefusedCalls != 0 {
			t.Errorf("RefusedCalls after %s = %d, want 0 — a refused subagent run has a counter of its own", pass, scan.RefusedCalls)
		}
		if scan.EventsWritten == 0 {
			t.Errorf("EventsWritten after %s = 0; the fixture is not collecting and the state assertion below means nothing", pass)
		}
		report, err := health.New(paths.HealthFile).Read()
		if err != nil {
			t.Fatalf("Read() on %s error = %v", pass, err)
		}
		if got := health.Diagnose(report, nil, nil); got.State != health.StateCollecting {
			t.Errorf("Diagnose().State after %s = %q, want %q", pass, got.State, health.StateCollecting)
		}
		// Not an honest zero either: the subagent source's one contribution was a counted
		// refusal, which is the opposite of the clean zero doctor reports as Skipped.
		if scan.Skipped != 0 {
			t.Errorf("Skipped after %s = %d, want 0 — an all-refused transcript is not an honest zero", pass, scan.Skipped)
		}
		if scan.ParseErrors != 0 || scan.Unreadable != 0 {
			t.Errorf("ParseErrors = %d, Unreadable = %d after %s; want 0 and 0 — every line was readable",
				scan.ParseErrors, scan.Unreadable, pass)
		}
	}

	// The session grain still derives its records — the sessions were observed and went
	// idle — but the run itself produces none: nothing names it, and fail closed means
	// no placeholder name is substituted (ADR-0007).
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	for _, event := range entries {
		if event.Record.Kind == record.KindSubagent {
			t.Errorf("a subagent record was written for a run nothing names: %+v", event.Record)
		}
	}
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

	written, err := Init(paths, root, claudeDir, testExecutable(t), true)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	// The stale session yields two records — its interrupted call and its session_end
	// — and the live session yields neither: its call is still buffered and its session
	// id is not silent yet. Both thresholds decline on the same session, which is what
	// makes this the end-to-end criterion for either rule.
	if written != 2 {
		t.Fatalf("Init() wrote %d events, want 2 — the stale session resolved and the live one did not", written)
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
	// The interrupted call and the stale session's session_end. Position 1 is the
	// call: the session grain is derived last.
	if len(entries) != 2 {
		t.Fatalf("Entries() = %+v, want the interrupted call and the session_end", entries)
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

	written, err := Init(paths, root, claudeDir, testExecutable(t), true)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	// Two: the one fallback record, plus the session_end its long-silent session id
	// yields (ADR-0034). The collapse this test is about is the fallback count.
	if written != 2 {
		t.Fatalf("Init() wrote %d events, want 2 — three attributed runs of one skill in one session are one record, beside the session_end", written)
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
	// The one fallback record, and the session_end beside it. Position 1 is the
	// fallback: the session grain is derived last.
	if len(entries) != 2 {
		t.Fatalf("Entries() = %+v, want the fallback record and the session_end", entries)
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
	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
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
		// Two records, and the same two every scan: the interrupted call, and the
		// session_end. Both are derived from their own source identity, so neither
		// accumulates a copy per scan (ADR-0004).
		if len(entries) != 2 {
			t.Fatalf("after scan %d: Entries() = %+v, want exactly two records", scanNumber, entries)
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

// TestEachThresholdGovernsOnlyItsOwnRule is what ADR-0023 §3's "no second
// threshold is introduced" actually requires, tested on behaviour rather than by
// grepping for a key name.
//
// This package used to assert that session.idle_timeout is never read here at all,
// which was the same guarantee while the staleness rule was the only rule there
// was. ADR-0034 adds a second one — when a session id is believed finished, so the
// session grain's one record can be derived — and this package owns config for
// every reader (plan §6.2), so the key has to be read here. What must not happen is
// either threshold reaching the other's rule, and each half below fails if it does.
func TestEachThresholdGovernsOnlyItsOwnRule(t *testing.T) {
	// A call one minute old, under a staleness threshold of 24h and an idle threshold
	// of one second: the session is long finished and the call is nowhere near stale.
	t.Run("session.idle_timeout does not resolve a call", func(t *testing.T) {
		paths, claudeDir := stalenessFixture(t, time.Minute)
		if _, err := config.Set(paths, "session.idle_timeout", "1s"); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		if _, err := Ingest(paths, claudeDir); err != nil {
			t.Fatalf("Ingest() error = %v", err)
		}

		if scan := scanOf(t, paths); scan.PendingCalls != 1 || scan.InterruptedCalls != 0 {
			t.Fatalf("Scan = %+v, want the call still buffered", scan)
		}
		if got := kindsInSpool(t, paths); !slices.Equal(got, []record.Kind{record.KindSessionEnd}) {
			t.Fatalf("spool holds %v, want only the session_end", got)
		}
	})

	// The mirror image: the call resolves and the session is not finished, so the two
	// records the two rules produce never arrive together by accident.
	t.Run("scan.stale_call_timeout does not finish a session", func(t *testing.T) {
		paths, claudeDir := stalenessFixture(t, time.Minute)
		if _, err := config.Set(paths, "scan.stale_call_timeout", "1s"); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		if _, err := config.Set(paths, "session.idle_timeout", "72h"); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		if _, err := Ingest(paths, claudeDir); err != nil {
			t.Fatalf("Ingest() error = %v", err)
		}

		if scan := scanOf(t, paths); scan.InterruptedCalls != 1 || scan.PendingCalls != 0 {
			t.Fatalf("Scan = %+v, want the call resolved", scan)
		}
		if got := kindsInSpool(t, paths); !slices.Equal(got, []record.Kind{record.KindBuiltinTool}) {
			t.Fatalf("spool holds %v, want only the interrupted call", got)
		}
	})
}

// kindsInSpool reads back the kinds the spool holds, in position order. It is what
// a test asserting "this rule produced this record and no other" compares against.
func kindsInSpool(t *testing.T, paths config.Paths) []record.Kind {
	t.Helper()
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	kinds := make([]record.Kind, 0, len(entries))
	for _, entry := range entries {
		kinds = append(kinds, entry.Record.Kind)
	}
	return kinds
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

// TestIngestRebuildsASpoolFromAnotherSchemaVersion is the other half of "a schema
// bump is a rebuild rather than a migration" (ADR-0007, ADR-0015). Refusing an
// earlier version on read is only the drop; without the rebuild every consumer
// reads the spool through Entries and reports the post-upgrade subset as the whole
// truth, and every position over it shifts under the delivery watermark.
//
// The stale line is written by hand because there is no earlier build to write it:
// it is this build's own encoding of a valid record with only the version number
// changed, which is what an additive dimension addition actually leaves on disk.
func TestIngestRebuildsASpoolFromAnotherSchemaVersion(t *testing.T) {
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
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"` + root + `","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"` + root + `","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(transcriptDir, "session.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatalf("WriteFile() transcript error = %v", err)
	}
	paths := testPaths(t)

	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	spoolPath := filepath.Join(paths.DataDir, eventsFile)
	outcome := record.OutcomeOK
	stale := record.Record{
		SchemaVersion: record.SchemaVersion - 1,
		EventID:       record.DeriveEventID("claude-code", "an-event-only-the-store-remembers"),
		Timestamp:     time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC),
		Harness:       "claude-code",
		SessionID:     "session-0",
		Repo:          "0123456789abcdef0123456789abcdef",
		Kind:          record.KindBuiltinTool,
		Name:          "Bash",
		Invoker:       record.InvokerModel,
		Outcome:       &outcome,
	}
	line, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	spool, openErr := os.OpenFile(spoolPath, os.O_APPEND|os.O_WRONLY, 0)
	if openErr != nil {
		t.Fatalf("OpenFile() error = %v", openErr)
	}
	if _, writeErr := spool.Write(append(line, '\n')); writeErr != nil {
		t.Fatalf("Write() error = %v", writeErr)
	}
	if closeErr := spool.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}

	if _, ingestErr := Ingest(paths, claudeDir); ingestErr != nil {
		t.Fatalf("Ingest() error = %v", ingestErr)
	}

	raw, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), string(stale.EventID)) {
		t.Error("the spool still holds the record from an earlier schema version; a bump is a rebuild, not a skip")
	}
	entries, err := store.New(spoolPath).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	// Re-derived from the transcript, from position 1: the rebuild is what keeps a
	// cursor over this spool meaningful. Two records — the call and its session_end —
	// and the point is where they start, not how many there are.
	if len(entries) != 2 || entries[0].Position != 1 {
		t.Fatalf("Entries() = %+v, want the re-derived records from position 1", entries)
	}

	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if report.Scan.StaleRecords != 1 {
		t.Errorf("Scan.StaleRecords = %d, want 1 — a discarded record is lost collection and carries a count", report.Scan.StaleRecords)
	}
	if !report.Scan.StaleRebuilt {
		t.Error("Scan.StaleRebuilt = false; the scan that discarded the spool re-derived it, and doctor says so")
	}
}

// TestIngestLeavesACurrentSpoolAlone is the guard on the other side: the rebuild
// fires on a foreign schema version and on nothing else, so an ordinary scan may
// never discard the history it is adding to.
func TestIngestLeavesACurrentSpoolAlone(t *testing.T) {
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
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"` + root + `","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"` + root + `","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(transcriptDir, "session.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatalf("WriteFile() transcript error = %v", err)
	}
	paths := testPaths(t)

	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	spoolPath := filepath.Join(paths.DataDir, eventsFile)
	before, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	if _, ingestErr := Ingest(paths, claudeDir); ingestErr != nil {
		t.Fatalf("Ingest() error = %v", ingestErr)
	}

	after, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(before) != string(after) {
		t.Error("a second scan rewrote a spool it could read; the rebuild must fire only on a foreign schema version")
	}
	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if report.Scan.StaleRecords != 0 {
		t.Errorf("Scan.StaleRecords = %d, want 0", report.Scan.StaleRecords)
	}
}

// splitSessionTranscripts writes the two files ADR-0036 is about: one session id
// spread over the parent's transcript and a subagent's, in the real on-disk layout
// (`<session>/subagents/<agent>.jsonl` beside `<session>.jsonl`). Each holds one
// terminated call and one assistant usage block under its own message id.
//
// parentAt and agentAt date them, so a caller decides which side of the thresholds
// each file falls on without either threshold being shortened.
func splitSessionTranscripts(t *testing.T, claudeDir, root, parentAt, agentAt string) {
	t.Helper()
	line := func(uuid, at, messageID, callID string) string {
		return `{"uuid":"` + uuid + `","sessionId":"session-1","cwd":"` + root +
			`","timestamp":"` + at + `","entrypoint":"cli","message":{"model":"sonnet","id":"` + messageID +
			`","usage":{"input_tokens":10,"output_tokens":20},"content":[{"type":"tool_use","id":"` + callID + `","name":"Bash"}]}}`
	}
	result := func(uuid, at, callID string) string {
		return `{"uuid":"` + uuid + `","sessionId":"session-1","cwd":"` + root +
			`","timestamp":"` + at + `","entrypoint":"cli","message":{"content":[{"type":"tool_result","tool_use_id":"` + callID + `","is_error":false}]}}`
	}
	writeFixture(t, filepath.Join(claudeDir, "projects", "project", "session.jsonl"),
		strings.Join([]string{line("parent-1", parentAt, "msg_parent", "call-parent"), result("parent-2", parentAt, "call-parent")}, "\n"))
	writeFixture(t, filepath.Join(claudeDir, "projects", "project", "session", "subagents", "agent-1.jsonl"),
		strings.Join([]string{line("agent-1", agentAt, "msg_agent", "call-agent"), result("agent-2", agentAt, "call-agent")}, "\n"))
}

// spoolSessionEnds reads the session-grain records the spool holds after an Init.
func spoolSessionEnds(t *testing.T, paths config.Paths) []record.Record {
	t.Helper()
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	ends := make([]record.Record, 0, len(entries))
	for _, entry := range entries {
		if entry.Record.Kind == record.KindSessionEnd {
			ends = append(ends, entry.Record)
		}
	}
	return ends
}

// TestIngestResolvesOneSessionAcrossAParentAndASubagentTranscript is AC 1 through
// the production walk: the two files are one session, so they yield one session_end
// whose totals cover both.
func TestIngestResolvesOneSessionAcrossAParentAndASubagentTranscript(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	claudeDir := filepath.Join(t.TempDir(), "claude")
	writeFixture(t, filepath.Join(claudeDir, "settings.json"), `{}`)
	// Both historical, so the provisional defaults close the session on any clock.
	splitSessionTranscripts(t, claudeDir, root, "2020-01-01T00:00:00Z", "2020-01-01T00:00:01Z")
	paths := testPaths(t)

	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ends := spoolSessionEnds(t, paths)
	if len(ends) != 1 {
		t.Fatalf("the spool holds %d session_end records, want 1 — the two files are one session", len(ends))
	}
	if ends[0].ToolCalls == nil || *ends[0].ToolCalls != 2 {
		t.Errorf("tool_calls = %v, want 2: one call from each transcript", ends[0].ToolCalls)
	}
	if ends[0].InputTokens == nil || *ends[0].InputTokens != 20 {
		t.Errorf("input_tokens = %v, want 20: both transcripts' usage blocks", ends[0].InputTokens)
	}
}

// TestIngestDoesNotCloseASessionAliveInASiblingTranscript is AC 2 through the
// production walk. The parent transcript is stamped from time.Now, so it is inside
// both windows by construction and neither threshold has to be shortened to make
// the test easy — the pattern TestIngestSurfacesPendingAndInterruptedCalls uses.
func TestIngestDoesNotCloseASessionAliveInASiblingTranscript(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	claudeDir := filepath.Join(t.TempDir(), "claude")
	writeFixture(t, filepath.Join(claudeDir, "settings.json"), `{}`)
	splitSessionTranscripts(t, claudeDir, root, time.Now().UTC().Format(time.RFC3339), "2020-01-01T00:00:00Z")
	// And the subagent file leaves a call unterminated, so the staleness rule has
	// something to decline: the call belongs to a session the parent shows running.
	writeFixture(t, filepath.Join(claudeDir, "projects", "project", "session", "subagents", "agent-2.jsonl"),
		`{"uuid":"agent-3","sessionId":"session-1","cwd":"`+root+
			`","timestamp":"2020-01-01T00:00:02Z","entrypoint":"cli","message":{"model":"sonnet","id":"msg_open","content":[{"type":"tool_use","id":"call-open","name":"Bash"}]}}`)
	paths := testPaths(t)

	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if ends := spoolSessionEnds(t, paths); len(ends) != 0 {
		t.Fatalf("the spool holds %d session_end records, want 0 — the parent transcript shows the session running", len(ends))
	}
	scan := scanCounters(t, paths)
	if scan.PendingCalls != 1 {
		t.Errorf("Scan.PendingCalls = %d, want 1 — the subagent's open call is buffered, not interrupted", scan.PendingCalls)
	}
	if scan.InterruptedCalls != 0 {
		t.Errorf("Scan.InterruptedCalls = %d, want 0 — nothing may be given up on from a partial view", scan.InterruptedCalls)
	}
}

// TestIngestIsIndependentOfWalkOrder is AC 4 asserted through the production walk
// rather than a test harness: filepath.WalkDir visits lexically, so the two trees
// below hold the same two logical sources under names whose lexical order is
// reversed and the real walk therefore visits them in opposite orders.
//
// The comparison is over the record set. The walk persists each source as it visits
// it — requirement 1 keeps one Read per file — so the spool's line order follows the
// walk while the records themselves do not.
func TestIngestIsIndependentOfWalkOrder(t *testing.T) {
	sources := func(t *testing.T, root, first, second string) config.Paths {
		t.Helper()
		claudeDir := filepath.Join(t.TempDir(), "claude")
		writeFixture(t, filepath.Join(claudeDir, "settings.json"), `{}`)
		splitSessionTranscripts(t, claudeDir, root, "2020-01-01T00:00:00Z", "2020-01-01T00:00:01Z")
		// Rename the two written transcripts so the lexical walk order is the caller's.
		parent := filepath.Join(claudeDir, "projects", "project", "session.jsonl")
		agent := filepath.Join(claudeDir, "projects", "project", "session", "subagents", "agent-1.jsonl")
		for from, to := range map[string]string{parent: first, agent: second} {
			target := filepath.Join(claudeDir, "projects", "project", to)
			if err := os.Rename(from, target); err != nil {
				t.Fatalf("Rename(%s) error = %v", from, err)
			}
		}
		paths := testPaths(t)
		if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		return paths
	}
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	forward := spoolEventIDs(t, sources(t, root, "a-parent.jsonl", "b-agent.jsonl"))
	reverse := spoolEventIDs(t, sources(t, root, "b-parent.jsonl", "a-agent.jsonl"))

	if len(forward) == 0 {
		t.Fatal("the walk wrote nothing, so this asserts nothing")
	}
	if !slices.Equal(forward, reverse) {
		t.Fatalf("the two walk orders wrote different records:\nforward  = %v\nreversed = %v", forward, reverse)
	}
}

// spoolEventIDs reads a spool back as the sorted list of its event ids, so two
// walks can be compared as sets. An event id identifies a record (ADR-0004), so the
// order is total.
func spoolEventIDs(t *testing.T, paths config.Paths) []string {
	t.Helper()
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, string(entry.Record.EventID))
	}
	slices.Sort(ids)
	return ids
}

// The end-to-end shape of ADR-0036 §3, on the real wiring: a tag naming a primitive the
// machine has is collected, one naming a typed CLI built-in is counted on its own
// counter, and neither reaches RefusedCalls or moves doctor's state word.
//
// The installed set arrives from the one discovery pass each ingesting command already
// runs, which is what makes the /pr-review half work at all: the fixture installs the
// skill under the temp claudeDir, so nothing outside t.TempDir() is read or written.
//
// Two assertions rather than one, and the negative one is the point. Folding this count
// into RefusedCalls would pin every machine to "collects nothing" on every scan forever,
// because the built-ins are re-skipped on every pass over the whole history.
func TestIngestCountsATypedInvocationTheMachineDoesNotHave(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	// The primitive the admissible tag names, installed globally inside the fixture's
	// own claudeDir so discovery finds it there and nowhere else.
	writeFixture(t, filepath.Join(claudeDir, "skills", "pr-review", "SKILL.md"), "# pr-review")
	// One consented session carrying both shapes: an installed skill typed by a person,
	// and a built-in that was never Wake's to collect.
	lines := []string{
		`{"uuid":"typed-1","sessionId":"session-typed","cwd":"` + root +
			`","timestamp":"2026-08-13T12:00:00Z","version":"1.0.0","entrypoint":"cli",` +
			`"type":"user","message":{"role":"user","content":"<command-name>/pr-review</command-name>"}}`,
		`{"uuid":"typed-2","sessionId":"session-typed","cwd":"` + root +
			`","timestamp":"2026-08-13T12:00:01Z","version":"1.0.0","entrypoint":"cli",` +
			`"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
	}
	writeFixture(t, filepath.Join(claudeDir, "projects", "project", "typed.jsonl"), strings.Join(lines, "\n"))

	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	scan := scanCounters(t, paths)
	if scan.SkippedTypedInvocations != 1 {
		t.Errorf("SkippedTypedInvocations = %d, want 1 — /clear is a skip the count has to report", scan.SkippedTypedInvocations)
	}
	if scan.RefusedCalls != 0 {
		t.Errorf("RefusedCalls = %d, want 0 — a typed built-in is not lost collection (ADR-0036 §3)", scan.RefusedCalls)
	}
	if scan.EventsWritten == 0 {
		t.Fatalf("EventsWritten = 0; the fixture is not collecting and the state assertion below means nothing")
	}
	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got := health.Diagnose(report, nil, nil); got.State == health.StateCollectsNothing {
		t.Errorf("Diagnose().State = %q; a skipped typed invocation must not blind the integration state", got.State)
	}

	// The admissible half really was collected, so the skip above is the count of one
	// case and not of both.
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	typed := 0
	for _, event := range entries {
		if event.Record.Kind == record.KindSkill && event.Record.Name == "pr-review" && event.Record.Invoker == record.InvokerUser {
			typed++
		}
	}
	if typed != 1 {
		t.Errorf("typed pr-review records = %d, want 1", typed)
	}
}
