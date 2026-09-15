package activation

import (
	"errors"
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
			tokens_cache_read integer, tokens_cache_write integer, cost real, time_updated integer,
			parent_id text, agent text, time_created integer)`,
		`create table part (id text primary key, message_id text, session_id text,
			time_created integer, time_updated integer, data text)`,
		sessionRow("ses_abc", directory, "", "",
			fmt.Sprint(openCodeInstant.UnixMilli()), fmt.Sprint(openCodeInstant.UnixMilli())),
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

// sessionRow is one `session` row written by column name rather than by position,
// so a column added to the fixture's schema cannot silently shift another's value.
//
// created and updated are raw SQL fragments — a millisecond literal or NULL —
// because two tests here need an absent instant, which is the one thing a typed
// parameter could not express.
func sessionRow(id, directory, agent, parentID, created, updated string) string {
	return fmt.Sprintf(`insert into session
		(id, directory, version, tokens_input, tokens_output, tokens_reasoning,
		 tokens_cache_read, tokens_cache_write, cost, time_updated, parent_id, agent, time_created)
		values (%s, %s, '1.18.30', 11, 13, 17, 19, 23, 0.42, %s, %s, %s, %s)`,
		sqlQuote(id), sqlQuote(directory), updated, sqlQuote(parentID), sqlQuote(agent), created)
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

// toolPartRowWithoutStart is the same row with no state.time.start key: the shape
// a harness that stopped writing the instant, or never wrote it for this state,
// leaves behind.
func toolPartRowWithoutStart(id, tool, status string) string {
	data := fmt.Sprintf(`{"type":"tool","tool":%s,"callID":"bash:1","state":{"status":%s,`+
		`"time":{"end":%d}}}`,
		sqlQuote(tool), sqlQuote(status), openCodeInstant.UnixMilli()+250)
	return fmt.Sprintf(`insert into part values (%s, 'msg_1', 'ses_abc', %d, %d, %s)`,
		sqlQuote(id), openCodeInstant.UnixMilli(), openCodeInstant.UnixMilli(), sqlQuote(data))
}

// skillPartRow is a tool part opencode wrote for a skill invocation, carrying the
// skill's own declared name beside the free text the walk must never select.
func skillPartRow(id, name string) string {
	data := fmt.Sprintf(`{"type":"tool","tool":"skill","callID":"skill:1","state":{"status":"completed",`+
		`"input":{"name":%s,"prompt":"s3cret skill prompt"},"output":"s3cret output",`+
		`"time":{"start":%d,"end":%d}},"metadata":{"x":1}}`,
		sqlQuote(name), openCodeInstant.UnixMilli(), openCodeInstant.UnixMilli()+250)
	return fmt.Sprintf(`insert into part values (%s, 'msg_1', 'ses_abc', %d, %d, %s)`,
		sqlQuote(id), openCodeInstant.UnixMilli(), openCodeInstant.UnixMilli(), sqlQuote(data))
}

// taskPartRow is the invoking part for a subagent run. It produces no record at
// all: the child session row is the canonical source event (ADR-0036 §2), and the
// subagent_type argument this row carries is never read.
func taskPartRow(id, subagentType string) string {
	data := fmt.Sprintf(`{"type":"tool","tool":"task","callID":"task:1","state":{"status":"completed",`+
		`"input":{"subagent_type":%s,"prompt":"s3cret task prompt"},"output":"s3cret output",`+
		`"time":{"start":%d,"end":%d}},"metadata":{"x":1}}`,
		sqlQuote(subagentType), openCodeInstant.UnixMilli(), openCodeInstant.UnixMilli()+250)
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

// runOpenCodeClosed is runOpenCode with the idle threshold elapsed, which is the
// only boundary either session-derived grain is emitted at. runOpenCode leaves
// both thresholds disabled, so it derives no session grain and therefore no
// subagent grain either.
func runOpenCodeClosed(t *testing.T, repos *config.Repos, storePath, spool string) (int, openCodeCounters, error) {
	t.Helper()
	return ingestOpenCode(repos, storePath, opencode.NewServers(nil), store.New(spool),
		adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: openCodeInstant.Add(2 * time.Hour)},
		wholeHistory, nil)
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
	// Every grain this reader derives is in the fixture, so the byte-identity claim
	// covers the two the skill and subagent derivations added.
	storePath := openCodeFixture(t, root,
		toolPartRow("prt_1", "bash", "completed"),
		toolPartRow("prt_2", "atlassian_search", "error"),
		skillPartRow("prt_3", "run-sdlc"),
		taskPartRow("prt_4", "sdlc-plan"),
		sessionRow("ses_child", root, "explore", "ses_abc",
			fmt.Sprint(openCodeInstant.UnixMilli()), fmt.Sprint(openCodeInstant.UnixMilli())),
	)
	spool := filepath.Join(t.TempDir(), "events.ndjson")

	if _, _, err := runOpenCodeClosed(t, repos, storePath, spool); err != nil {
		t.Fatalf("first walk: %v", err)
	}
	before, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	written, _, err := runOpenCodeClosed(t, repos, storePath, spool)
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
	// $.state.input is not on this list any more, because the query legitimately
	// carries exactly one key beneath it — the skill's own declared name. That one
	// key is pinned by the closed enumeration below rather than by an absence here.
	for _, key := range []string{"callID", "$.state.output", "$.state.title", "$.state.error", "$.state.raw", "$.metadata", "$.state.metadata", "cost"} {
		for _, query := range []string{sessionQuery, partQuery} {
			if strings.Contains(query, key) {
				t.Errorf("a query selects %q: free text must not enter this process at all (ADR-0007)", key)
			}
		}
	}
}

// allowedInputPath is the one $.state.input key any query may project: a skill's
// own declared name, which ADR-0007's Consequences license reading in order to
// derive a name.  Everything else under that object is free text.
const allowedInputPath = `$.state.input.name`

// TestTheOnlyProjectedInputKeyIsTheSkillName is the privacy guarantee of this
// change, pinned rather than assumed. The named forbidden paths are the ones a
// reader would reach for first; the closed enumeration below them is what catches
// a path this list never imagined — including $.state.input.subagent_type, which
// is not read at all, because an opencode subagent's canonical source event is the
// child session row and not the invoking part (ADR-0036 §1-§2).
func TestTheOnlyProjectedInputKeyIsTheSkillName(t *testing.T) {
	for _, forbidden := range []string{
		`$.state.input.prompt`, `$.state.input.description`, `$.state.input.command`,
		`$.state.input.subagent_type`, `$.state.input.arguments`, `$.state.input.filePath`,
	} {
		for _, query := range []string{sessionQuery, partQuery} {
			if strings.Contains(query, forbidden) {
				t.Errorf("a query projects %q: free text must not enter this process at all (ADR-0007)", forbidden)
			}
		}
	}
	for _, query := range []string{sessionQuery, partQuery} {
		for rest := query; ; {
			at := strings.Index(rest, `$.state.input`)
			if at < 0 {
				break
			}
			if !strings.HasPrefix(rest[at:], allowedInputPath+`'`) {
				t.Errorf("a query projects a $.state.input key other than %q", allowedInputPath)
				break
			}
			rest = rest[at+len(allowedInputPath):]
		}
	}
}

// TestNoInstantIsCoalesced is a source-level assertion, for the reason the two
// above it are: what it forbids is a query somebody writes later. Coalescing an
// instant substitutes the epoch for an absence, and the epoch is the one
// substitution that passes every validator and renders as a real measurement.
func TestNoInstantIsCoalesced(t *testing.T) {
	for _, query := range []string{sessionQuery, partQuery} {
		for _, argument := range coalesced(query) {
			for _, instant := range []string{"time_updated", "time_created", "$.state.time.start", "$.state.time.end"} {
				if strings.Contains(argument, instant) {
					t.Errorf("a query coalesces %q: an absent instant must stay distinguishable from the epoch", instant)
				}
			}
		}
	}
}

// coalesced is every argument list a query hands coalesce(), matched by counting
// parentheses rather than by line, because one line carries several calls and a
// coalesced instant can nest a json_extract inside itself.
func coalesced(query string) []string {
	arguments := []string{}
	for rest := query; ; {
		open := strings.Index(rest, "coalesce(")
		if open < 0 {
			return arguments
		}
		rest = rest[open+len("coalesce("):]
		depth, end := 1, -1
		for i, r := range rest {
			switch r {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				end = i
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return append(arguments, rest)
		}
		arguments = append(arguments, rest[:end])
		rest = rest[end:]
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

// unscannablePartRow is a tool part whose start instant is a JSON string where the
// walk projects a number. It is well-formed JSON opencode could plausibly write
// after retyping a field, and it is exactly the shape row.Scan refuses — the
// format drift plan §12 says must fail visibly per harness rather than quietly
// stop the walk.
func unscannablePartRow(id string) string {
	data := fmt.Sprintf(`{"type":"tool","tool":"bash","callID":"bash:1","state":{"status":"completed",`+
		`"time":{"start":"not-a-number","end":%d}}}`, openCodeInstant.UnixMilli()+250)
	return fmt.Sprintf(`insert into part values (%s, 'msg_1', 'ses_abc', %d, %d, %s)`,
		sqlQuote(id), openCodeInstant.UnixMilli(), openCodeInstant.UnixMilli(), sqlQuote(data))
}

// unscannableSessionRow is a session whose token count is text where the walk
// projects a number, on the same terms.
func unscannableSessionRow(id string) string {
	return fmt.Sprintf(`insert into session values (%s, '/nowhere', '1.18.30', 'lots', 13, 17, 19, 23, 0.42, %d, '', '', %d)`,
		sqlQuote(id), openCodeInstant.UnixMilli(), openCodeInstant.UnixMilli())
}

// TestAPageOfUnscannablePartsDoesNotStallTheWalk pins the paging key's only job:
// it steps past a row this build cannot decode. A whole page of such rows used to
// leave the key where it was, so the identical query came back with the identical
// rows until the scan budget ran out — and every healthy row after that page was
// orphaned on every future scan, because this walk keeps no cursor and starts over
// each time.
func TestAPageOfUnscannablePartsDoesNotStallTheWalk(t *testing.T) {
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)

	rows := make([]string, 0, sqlitex.MaxRowsPerPage+1)
	for i := range sqlitex.MaxRowsPerPage {
		rows = append(rows, unscannablePartRow(fmt.Sprintf("prt_%04d", i)))
	}
	rows = append(rows, toolPartRow(fmt.Sprintf("prt_%04d", sqlitex.MaxRowsPerPage), "bash", "completed"))
	storePath := openCodeFixture(t, root, rows...)

	written, counters, err := runOpenCode(t, repos, storePath, filepath.Join(t.TempDir(), "events.ndjson"))
	if err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	if written != 1 || counters.Parts != 1 {
		t.Fatalf("written = %d, parts = %d, want 1 and 1: the healthy row sits after the unscannable page",
			written, counters.Parts)
	}
	if counters.ParseErrors != sqlitex.MaxRowsPerPage {
		t.Fatalf("parseErrors = %d, want %d: the unscannable page is read once, not until the budget runs out",
			counters.ParseErrors, sqlitex.MaxRowsPerPage)
	}
}

// TestAPageOfUnscannableSessionsDoesNotStallTheWalk is the same rule for the other
// pager. A session that never registers is a part refused for want of consent, so
// a stalled session page silently empties the whole harness.
func TestAPageOfUnscannableSessionsDoesNotStallTheWalk(t *testing.T) {
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)

	// ids sort before the fixture's own 'ses_abc', so the first page is entirely
	// unscannable and the consented session is only reachable past it.
	rows := make([]string, 0, sqlitex.MaxRowsPerPage+1)
	for i := range sqlitex.MaxRowsPerPage {
		rows = append(rows, unscannableSessionRow(fmt.Sprintf("ses_%04d", i)))
	}
	rows = append(rows, toolPartRow("prt_1", "bash", "completed"))
	storePath := openCodeFixture(t, root, rows...)

	written, counters, err := runOpenCode(t, repos, storePath, filepath.Join(t.TempDir(), "events.ndjson"))
	if err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	if written != 1 || counters.Sessions != 1 {
		t.Fatalf("written = %d, sessions = %d, want 1 and 1: the consented session sits after the unscannable page",
			written, counters.Sessions)
	}
	if counters.ParseErrors != sqlitex.MaxRowsPerPage {
		t.Fatalf("parseErrors = %d, want %d: the unscannable page is read once, not until the budget runs out",
			counters.ParseErrors, sqlitex.MaxRowsPerPage)
	}
}

// TestAPageThatNeverAdvancesEndsTheWalk covers the guard under the key: whatever
// the reason a full page leaves the key untouched, re-issuing the identical query
// would read the same rows forever, so the walk stops and reports blindness
// instead of spending 250,000 reads on it.
func TestAPageThatNeverAdvancesEndsTheWalk(t *testing.T) {
	root := t.TempDir()
	rows := make([]string, 0, sqlitex.MaxRowsPerPage)
	for i := range sqlitex.MaxRowsPerPage {
		rows = append(rows, unscannableSessionRow(fmt.Sprintf("ses_%04d", i)))
	}
	db, err := sqlitex.Open(openCodeFixture(t, root, rows...))
	if err != nil {
		t.Fatalf("sqlitex.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	visited := 0
	err = page(db, sessionQuery, func(sqlitex.Row) string {
		visited++
		return ""
	})
	if !errors.Is(err, errPageStalled) {
		t.Fatalf("page() error = %v, want errPageStalled", err)
	}
	if visited != sqlitex.MaxRowsPerPage {
		t.Fatalf("visited = %d rows, want %d: the stalled page is read once", visited, sqlitex.MaxRowsPerPage)
	}
}

// TestAToolPartWithNoStartInstantIsRefusedAndVisible drives the defect end to end,
// on the whole-history path wake ingest and wake init --full both take. A start
// instant coalesced to 0 would write a record stamped 1970-01-01T00:00:00Z — which
// record.Validate admits, because it is not the zero time — carrying a duration of
// roughly 56 years, while every counter stayed 0 and doctor called the harness
// healthy. Inferring the instant and counting on is exactly what plan §3.3 and §12
// forbid: the loss has to fail visibly per harness.
func TestAToolPartWithNoStartInstantIsRefusedAndVisible(t *testing.T) {
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)
	storePath := openCodeFixture(t, root, toolPartRowWithoutStart("prt_1", "bash", "completed"))
	spool := filepath.Join(t.TempDir(), "events.ndjson")

	written, counters, err := runOpenCode(t, repos, storePath, spool)
	if err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	if written != 0 {
		t.Fatalf("written = %d, want 0: no record carries an instant nothing recorded", written)
	}
	if counters.RefusedCalls != 1 {
		t.Fatalf("refused calls = %d, want 1", counters.RefusedCalls)
	}
	state := health.DiagnoseHarness(health.HarnessScan{
		Observed: counters.Observed, Unreadable: counters.Unreadable, ParseErrors: counters.ParseErrors,
		RefusedCalls: counters.RefusedCalls, UnknownOutcomes: counters.UnknownOutcomes,
		EventsWritten: counters.EventsWritten,
	})
	if state != health.StateCollectsNothing {
		t.Fatalf("state = %q, want %q: blindness has to reach doctor", state, health.StateCollectsNothing)
	}
	if body, readErr := os.ReadFile(spool); readErr == nil && strings.Contains(string(body), "1970-01-01") {
		t.Fatal("a record was written stamped at the epoch")
	}
}

// TestASessionWithNoLastActivityInstantIsRefusedAndVisible is the same rule one
// column over: session.time_updated is the session grain's whole timestamp and the
// idleness comparison's whole input, so a coalesced 0 made a session with no
// recorded activity both instantly finished and stamped at 1970.
func TestASessionWithNoLastActivityInstantIsRefusedAndVisible(t *testing.T) {
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)
	path := filepath.Join(t.TempDir(), "opencode.db")
	storePath := writeFixtureStore(t, path, []string{
		`create table session (id text primary key, directory text, version text,
			tokens_input integer, tokens_output integer, tokens_reasoning integer,
			tokens_cache_read integer, tokens_cache_write integer, cost real, time_updated integer,
			parent_id text, agent text, time_created integer)`,
		`create table part (id text primary key, message_id text, session_id text,
			time_created integer, time_updated integer, data text)`,
		sessionRow("ses_abc", root, "", "", fmt.Sprint(openCodeInstant.UnixMilli()), "NULL"),
	})
	spool := filepath.Join(t.TempDir(), "events.ndjson")

	written, counters, err := ingestOpenCode(repos, storePath, opencode.NewServers(nil), store.New(spool),
		adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: openCodeInstant.Add(2 * time.Hour)},
		wholeHistory, nil)
	if err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	if written != 0 {
		t.Fatalf("written = %d, want 0: no session_end carries an instant nothing recorded", written)
	}
	if counters.RefusedCalls != 1 {
		t.Fatalf("refused calls = %d, want 1", counters.RefusedCalls)
	}
	if body, readErr := os.ReadFile(spool); readErr == nil && strings.Contains(string(body), "1970-01-01") {
		t.Fatal("a session_end was written stamped at the epoch")
	}
}

// TestASkillPartCollectsItsOwnName is the CI-reproducible half of the claim that
// report shows opencode skills by name: the name arrives through the real query,
// and none of the free text sitting beside it in the same blob does.
func TestASkillPartCollectsItsOwnName(t *testing.T) {
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)
	storePath := openCodeFixture(t, root, skillPartRow("prt_1", "run-sdlc"))
	spool := filepath.Join(t.TempDir(), "events.ndjson")

	written, _, err := runOpenCode(t, repos, storePath, spool)
	if err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	if written != 1 {
		t.Fatalf("written = %d, want 1", written)
	}
	body, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, want := range []string{`"kind":"skill"`, `"name":"run-sdlc"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the spool does not carry %s", want)
		}
	}
	for _, secret := range []string{"s3cret skill prompt", "s3cret output", "skill:1"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("the spool carries %q from the source blob", secret)
		}
	}
}

// TestATaskPartCollectsNothing pins the skip end to end, including that the
// invoking argument is not read at all: subagent_type appears nowhere, because the
// query never projects it.
func TestATaskPartCollectsNothing(t *testing.T) {
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)
	storePath := openCodeFixture(t, root, taskPartRow("prt_1", "sdlc-plan"))
	spool := filepath.Join(t.TempDir(), "events.ndjson")

	written, counters, err := runOpenCode(t, repos, storePath, spool)
	if err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	if written != 0 {
		t.Fatalf("written = %d, want 0: the invoking part produces no record", written)
	}
	if counters.RefusedCalls != 0 || counters.PendingCalls != 0 {
		t.Fatalf("refused = %d, pending = %d, want 0 and 0: skipped is not lost collection",
			counters.RefusedCalls, counters.PendingCalls)
	}
	if body, readErr := os.ReadFile(spool); readErr == nil && strings.Contains(string(body), "sdlc-plan") {
		t.Fatal("the spool carries the invoking subagent_type argument")
	}
}

// TestAChildSessionCollectsASubagentNamedByItsAgent is the other half: the name
// comes from the child session's own agent declaration, and the invoking part's
// subagent_type — a different value on purpose — reaches nothing.
func TestAChildSessionCollectsASubagentNamedByItsAgent(t *testing.T) {
	paths := testPaths(t)
	root := t.TempDir()
	repos := consentedRepos(t, paths, root)
	storePath := openCodeFixture(t, root,
		taskPartRow("prt_1", "sdlc-plan"),
		sessionRow("ses_child", root, "explore", "ses_abc",
			fmt.Sprint(openCodeInstant.UnixMilli()), fmt.Sprint(openCodeInstant.UnixMilli())),
	)
	spool := filepath.Join(t.TempDir(), "events.ndjson")

	if _, _, err := runOpenCodeClosed(t, repos, storePath, spool); err != nil {
		t.Fatalf("ingestOpenCode() error = %v", err)
	}
	body, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, want := range []string{`"kind":"subagent"`, `"name":"explore"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the spool does not carry %s", want)
		}
	}
	if strings.Contains(string(body), `"name":"sdlc-plan"`) {
		t.Error("the spool names the subagent by the caller's argument rather than by the harness's own declaration")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.Contains(line, `"kind":"subagent"`) && !strings.Contains(line, `"session_id":"ses_child"`) {
			t.Error("the subagent invocation does not carry the child's own session id")
		}
	}
}
