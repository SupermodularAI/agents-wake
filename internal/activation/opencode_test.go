package activation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/adapter/opencode"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/sqlitex"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// openCodeInstant is when this file's fixture rows happen.
var openCodeInstant = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// openCodeFixture builds a store shaped like opencode's own: a session table and a
// part table whose data column is the JSON blob the harness writes.
//
// It is built through sqlitex.CreateFixture, which is the only write path this
// module has for a SQLite file — a test opening its own handle would be the second
// way to touch SQLite that ADR-0009's confinement forbids, and
// internal/sqlitex/imports_test.go fails the build if one appears.
func openCodeFixture(t *testing.T, directory string, parts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	statements := []string{
		`create table session (id text primary key, directory text, version text,
			tokens_input integer, tokens_output integer, tokens_reasoning integer,
			tokens_cache_read integer, tokens_cache_write integer, cost real, time_updated integer)`,
		`create table part (id text primary key, message_id text, session_id text,
			time_created integer, time_updated integer, data text)`,
		fmt.Sprintf(`insert into session values ('ses_abc', %s, '1.18.30', 11, 13, 17, 19, 23, 0.42, %d)`,
			sqlQuote(directory), openCodeInstant.UnixMilli()),
	}
	return writeFixtureStore(t, path, append(statements, parts...))
}

func writeFixtureStore(t *testing.T, path string, statements []string) string {
	t.Helper()
	if err := sqlitex.CreateFixture(path, statements...); err != nil {
		t.Fatalf("creating the opencode fixture: %v", err)
	}
	return path
}

// toolPartRow is one `part` row whose data is a tool part, written the way
// opencode writes it — including the free-text keys this walk must never select.
func toolPartRow(id, tool, status string) string {
	data := fmt.Sprintf(`{"type":"tool","tool":%s,"callID":"bash:1","state":{"status":%s,`+
		`"input":{"command":"echo secret"},"output":"s3cret output","title":"a title",`+
		`"metadata":{"exit":0},"time":{"start":%d,"end":%d}},"metadata":{"x":1}}`,
		sqlQuote(tool), sqlQuote(status), openCodeInstant.UnixMilli(), openCodeInstant.UnixMilli()+250)
	return fmt.Sprintf(`insert into part values (%s, 'msg_1', 'ses_abc', %d, %d, %s)`,
		sqlQuote(id), openCodeInstant.UnixMilli(), openCodeInstant.UnixMilli(), sqlQuote(data))
}

// sqlQuote is a SQL string literal for a fixture statement. It is not quote()
// from hooks_test.go, which quotes for JSON.
func sqlQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

// consentedRepos registers root and returns the table the walk resolves against.
func consentedRepos(t *testing.T, paths config.Paths, root string) *config.Repos {
	t.Helper()
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	if _, err := repos.Register(root, "project", time.Time{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return repos
}

// runOpenCode drives one opencode walk against a fresh spool.
func runOpenCode(t *testing.T, repos *config.Repos, storePath, spool string) (int, openCodeCounters, error) {
	t.Helper()
	return ingestOpenCode(repos, storePath, opencode.NewServers(nil), store.New(spool),
		adapter.Staleness{}, adapter.Idleness{}, wholeHistory, nil)
}

func TestAnAbsentStoreCollectsNothingAndIsNotObserved(t *testing.T) {
	paths := testPaths(t)
	repos := consentedRepos(t, paths, t.TempDir())
	spool := filepath.Join(t.TempDir(), "events.ndjson")

	written, counters, err := runOpenCode(t, repos, filepath.Join(t.TempDir(), "missing.db"), spool)
	if err != nil {
		t.Fatalf("an absent store returned an error: %v", err)
	}
	if counters.Observed {
		t.Error("an absent store was reported as observed: nobody looked, so nothing may read as zero")
	}
	if written != 0 {
		t.Errorf("written = %d, want 0", written)
	}
}

func TestAnUnreadableStoreIsObservedAndBlind(t *testing.T) {
	paths := testPaths(t)
	repos := consentedRepos(t, paths, t.TempDir())
	corrupt := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(corrupt, []byte(strings.Repeat("not a database", 64)), 0o600); err != nil {
		t.Fatalf("writing the corrupt store: %v", err)
	}

	written, counters, err := runOpenCode(t, repos, corrupt, filepath.Join(t.TempDir(), "events.ndjson"))
	if err != nil {
		t.Fatalf("an unreadable store broke the command: %v", err)
	}
	if !counters.Observed || counters.Unreadable != 1 {
		t.Fatalf("observed = %t, unreadable = %d, want true and 1", counters.Observed, counters.Unreadable)
	}
	if written != 0 {
		t.Errorf("written = %d, want 0", written)
	}
}

func TestALockedStoreBreaksNoCommand(t *testing.T) {
	// A store held by a running harness, standing in here as a file this build
	// cannot read at all: whichever way the refusal arrives, it is blindness
	// reported through counters and never an error that breaks a command.
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)
	locked := openCodeFixture(t, root, toolPartRow("prt_1", "bash", "completed"))
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("cannot make the store unreadable on this system: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o600) })

	written, counters, err := runOpenCode(t, repos, locked, filepath.Join(t.TempDir(), "events.ndjson"))
	if err != nil {
		t.Fatalf("a locked store broke the command: %v", err)
	}
	if !counters.Observed || counters.Unreadable == 0 {
		t.Fatalf("observed = %t, unreadable = %d, want true and at least 1", counters.Observed, counters.Unreadable)
	}
	if written != 0 {
		t.Errorf("written = %d, want 0", written)
	}
}

func TestAConsentedDirectoryCollects(t *testing.T) {
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)
	storePath := openCodeFixture(t, root,
		toolPartRow("prt_1", "bash", "completed"),
		toolPartRow("prt_2", "read", "completed"),
	)
	spool := filepath.Join(t.TempDir(), "events.ndjson")

	written, counters, err := runOpenCode(t, repos, storePath, spool)
	if err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	if written != 2 || counters.EventsWritten != 2 {
		t.Fatalf("written = %d (counter %d), want 2", written, counters.EventsWritten)
	}
	if counters.Sessions != 1 || counters.Parts != 2 {
		t.Fatalf("sessions = %d, parts = %d, want 1 and 2", counters.Sessions, counters.Parts)
	}
	spooled, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	// The free text the fixture's blob carries is in the store and must be in
	// nothing this walk wrote.
	for _, secret := range []string{"echo secret", "s3cret output", "a title", "bash:1"} {
		if strings.Contains(string(spooled), secret) {
			t.Errorf("the spool carries %q from the source blob", secret)
		}
	}
}

func TestAnUnconsentedDirectoryCollectsNothing(t *testing.T) {
	paths := testPaths(t)
	repos := consentedRepos(t, paths, t.TempDir())
	// The store's session names a directory belonging to no consented repository.
	storePath := openCodeFixture(t, filepath.Join(t.TempDir(), "elsewhere"), toolPartRow("prt_1", "bash", "completed"))

	written, counters, err := runOpenCode(t, repos, storePath, filepath.Join(t.TempDir(), "events.ndjson"))
	if err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	if written != 0 {
		t.Fatalf("written = %d, want 0: consent is the caller's answer and it said no", written)
	}
	if counters.RefusedCalls != 0 || counters.Skipped != 1 {
		t.Fatalf("refused = %d, skipped = %d, want 0 and 1: an unconsented directory is an honest zero", counters.RefusedCalls, counters.Skipped)
	}
}

func TestRescanningTheSameStoreIsByteIdentical(t *testing.T) {
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)
	storePath := openCodeFixture(t, root,
		toolPartRow("prt_1", "bash", "completed"),
		toolPartRow("prt_2", "atlassian_search", "error"),
	)
	spool := filepath.Join(t.TempDir(), "events.ndjson")

	if _, _, err := runOpenCode(t, repos, storePath, spool); err != nil {
		t.Fatalf("first walk: %v", err)
	}
	before, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	written, _, err := runOpenCode(t, repos, storePath, spool)
	if err != nil {
		t.Fatalf("second walk: %v", err)
	}
	if written != 0 {
		t.Errorf("the second walk wrote %d records, want 0", written)
	}
	after, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("scanning the same store twice changed the store's contents")
	}
}

func TestAnUnterminatedPartIsNotWritten(t *testing.T) {
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)
	storePath := openCodeFixture(t, root,
		toolPartRow("prt_1", "bash", "running"),
		toolPartRow("prt_2", "read", "cancelled"),
	)

	written, counters, err := runOpenCode(t, repos, storePath, filepath.Join(t.TempDir(), "events.ndjson"))
	if err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	if written != 0 {
		t.Fatalf("written = %d, want 0: only terminal events are emitted", written)
	}
	if counters.PendingCalls != 1 || counters.UnknownOutcomes != 1 {
		t.Fatalf("pending = %d, unknown outcomes = %d, want 1 and 1", counters.PendingCalls, counters.UnknownOutcomes)
	}
}

// TestTheWalkNeverOrdersOnAnUnindexedColumn and TestTheWalkSelectsNoFreeTextColumn
// are source-level assertions, because what they forbid is a query somebody writes
// later rather than a behaviour today's fixture can exhibit.
func TestTheWalkNeverOrdersOnAnUnindexedColumn(t *testing.T) {
	source := readSource(t, "opencode.go")
	for _, unindexed := range []string{"ORDER BY time_created", "ORDER BY p.time_created", "ORDER BY time_updated", "ORDER BY p.time_updated"} {
		if strings.Contains(source, unindexed) {
			t.Errorf("a query orders on %q, which has no index (ADR-0009)", unindexed)
		}
	}
	for _, query := range []string{sessionQuery, partQuery} {
		if !strings.Contains(query, "LIMIT ?") {
			t.Errorf("a query has no row cap:\n%s", query)
		}
		if !strings.Contains(query, "ORDER BY") {
			t.Errorf("a query pages without an order, so its cursor means nothing:\n%s", query)
		}
	}
}

func TestTheWalkSelectsNoFreeTextColumn(t *testing.T) {
	for _, key := range []string{"callID", "$.state.input", "$.state.output", "$.state.title", "$.state.error", "$.state.raw", "$.metadata", "$.state.metadata", "cost"} {
		for _, query := range []string{sessionQuery, partQuery} {
			if strings.Contains(query, key) {
				t.Errorf("a query selects %q: free text must not enter this process at all (ADR-0007)", key)
			}
		}
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	source, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(source)
}

// The acceptance criteria for autodetection, driven through Init and Ingest rather
// than through the walk: what the ticket promises is that normal use collects from
// both harnesses with no configuration command, and that a machine with only
// Claude Code behaves exactly as it did.

// bothHarnesses builds a machine with a Claude Code transcript and an opencode
// store, both in the same consented repository.
func bothHarnesses(t *testing.T) (paths config.Paths, claudeDir, root, openCodeStore string) {
	t.Helper()
	paths = testPaths(t)
	claudeDir, root = inventoryFixture(t)
	openCodeStore = openCodeFixture(t, root, toolPartRow("prt_1", "atlassian_search", "completed"))
	return paths, claudeDir, root, openCodeStore
}

// withOpenCodeStore points the resolvers at a store built for a test, through
// opencode's own environment variables rather than through a wake config key.
func withOpenCodeStore(t *testing.T, storePath string) {
	t.Helper()
	// XDG_DATA_HOME/opencode/opencode.db is where the resolver looks, so the
	// fixture is linked into that shape.
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o700); err != nil {
		t.Fatalf("creating the data dir: %v", err)
	}
	source, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("reading the fixture store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "opencode.db"), source, 0o600); err != nil {
		t.Fatalf("placing the fixture store: %v", err)
	}
	t.Setenv(config.EnvXDGDataHome, data)
	t.Setenv(config.EnvXDGConfigHome, t.TempDir())
	t.Setenv(config.EnvOpenCodeConfig, "")
}

func harnessesInSpool(t *testing.T, paths config.Paths) map[record.Identifier]int {
	t.Helper()
	entries, err := store.New(filepath.Join(paths.DataDir, eventsFile)).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	counts := map[record.Identifier]int{}
	for _, entry := range entries {
		counts[entry.Record.Harness]++
	}
	return counts
}

func TestBothHarnessesCollectFromOneScan(t *testing.T) {
	paths, claudeDir, root, openCodeStore := bothHarnesses(t)
	withOpenCodeStore(t, openCodeStore)

	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	counts := harnessesInSpool(t, paths)
	if counts["claude-code"] == 0 || counts["opencode"] == 0 {
		t.Fatalf("records per harness = %v, want both: autodetection means no config command was needed", counts)
	}
}

func TestOnlyClaudeCodeInstalledIsUnchanged(t *testing.T) {
	// A machine with no opencode store collects exactly what it collected before,
	// and opencode reports "not observed" rather than a zero.
	paths, claudeDir, root, _ := bothHarnesses(t)
	t.Setenv(config.EnvXDGDataHome, t.TempDir())
	t.Setenv(config.EnvXDGConfigHome, t.TempDir())
	t.Setenv(config.EnvOpenCodeConfig, "")

	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !report.Scan.ClaudeCode.Observed || report.Scan.ClaudeCode.EventsWritten == 0 {
		t.Errorf("claude code section = %+v, want it collecting", report.Scan.ClaudeCode)
	}

	// The rescan writes nothing twice and leaves the spool byte-identical, which is
	// the "behaviour is unchanged" half of the criterion.
	spool := filepath.Join(paths.DataDir, eventsFile)
	before, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	written, err := Ingest(paths, claudeDir)
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if written != 0 {
		t.Fatalf("a second scan wrote %d records, want 0", written)
	}
	after, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("a rescan on a Claude-Code-only machine changed the spool")
	}

	counts := harnessesInSpool(t, paths)
	if counts["opencode"] != 0 {
		t.Fatalf("opencode records = %d on a machine with no opencode", counts["opencode"])
	}
	if counts["claude-code"] == 0 {
		t.Fatal("Claude Code collected nothing")
	}

	rescanned, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if rescanned.Scan.OpenCode.Observed {
		t.Error("an absent opencode store was reported as observed")
	}
	if health.DiagnoseHarness(rescanned.Scan.OpenCode) != health.StateNotObserved {
		t.Error("an absent harness did not read as not observed")
	}
}

func TestAnOpenCodeFailureDoesNotFailIngest(t *testing.T) {
	paths, claudeDir, root, _ := bothHarnesses(t)
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o700); err != nil {
		t.Fatalf("creating the data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "opencode.db"), []byte(strings.Repeat("not a database", 64)), 0o600); err != nil {
		t.Fatalf("writing the corrupt store: %v", err)
	}
	t.Setenv(config.EnvXDGDataHome, data)
	t.Setenv(config.EnvXDGConfigHome, t.TempDir())
	t.Setenv(config.EnvOpenCodeConfig, "")

	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() with a corrupt opencode store failed: %v", err)
	}
	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() with a corrupt opencode store failed: %v", err)
	}
	if counts := harnessesInSpool(t, paths); counts["claude-code"] == 0 {
		t.Fatal("a blind opencode store cost Claude Code its records")
	}

	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if health.DiagnoseHarness(report.Scan.OpenCode) != health.StateCollectsNothing {
		t.Errorf("opencode state = %q, want %q", health.DiagnoseHarness(report.Scan.OpenCode), health.StateCollectsNothing)
	}
	// The machine-wide word stays what Claude Code earned: adapters fail
	// independently and soft, so a blind opencode store must not blind the machine.
	// It is asserted as "not collects nothing" rather than as one word, because the
	// word a rescan earns depends on what that rescan found and this test is not
	// about that.
	if got := health.Diagnose(report, nil, nil).State; got == health.StateCollectsNothing {
		t.Errorf("machine-wide state = %q: one harness's blindness moved the machine-wide word", got)
	}
}

func TestOpenCodeUsesTheSameConsentPath(t *testing.T) {
	// No opencode-specific consent exists: a directory consented for Claude Code
	// collects opencode with no further registration, and an unconsented one
	// collects neither.
	paths, claudeDir, root, openCodeStore := bothHarnesses(t)
	withOpenCodeStore(t, openCodeStore)

	if _, err := Ingest(paths, claudeDir); err != nil {
		t.Fatalf("Ingest() before any consent failed: %v", err)
	}
	if counts := harnessesInSpool(t, paths); len(counts) != 0 {
		t.Fatalf("records = %v before any repository was consented", counts)
	}

	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if counts := harnessesInSpool(t, paths); counts["opencode"] == 0 {
		t.Fatalf("records per harness = %v, want opencode collected under the same consent", counts)
	}
}

func TestTheScanRecordsWhichHarnessesItRead(t *testing.T) {
	paths, claudeDir, root, openCodeStore := bothHarnesses(t)
	withOpenCodeStore(t, openCodeStore)
	if _, err := Init(paths, root, claudeDir, testExecutable(t), true); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	snapshot, err := inventory.New(paths.PrimitivesFile).Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	observed := map[record.Identifier]bool{}
	for _, harness := range snapshot.Harnesses {
		observed[harness.Harness] = harness.Observed
	}
	if len(observed) != 2 || !observed["claude-code"] || !observed["opencode"] {
		t.Fatalf("harness observations = %v, want both read", snapshot.Harnesses)
	}
}
