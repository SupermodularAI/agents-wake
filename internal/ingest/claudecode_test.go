package ingest

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/report"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// names keys the scope digest for this package's tests, standing in for the
// subkey config.Repos.NameKey derives in production.
var names = record.NewNamer([]byte("test scope key"))

// transcriptInstant is when this file's fixture entries happen. Every test that needs
// a session judged closed computes its clock from it rather than reading the wall
// clock: the real threshold errs deliberately long (24h) and must never be shortened
// to make a test easier to write.
var transcriptInstant = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// closingStaleness is a staleness value under which every session in this file's
// fixtures has gone quiet past the threshold. ADR-0023 makes session close the
// terminal boundary for an attributed skill run's fallback record, so a test that
// wants one written has to say the session ended.
var closingStaleness = claudecode.Staleness{Timeout: time.Hour, Now: transcriptInstant.Add(8 * time.Hour)}

// spoolLines counts the records in the spool at path. A missing spool is zero: a scan
// that wrote nothing never creates the file.
func spoolLines(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	lines := 0
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) > 0 {
			lines++
		}
	}
	return lines
}

// reportedCalls returns the CALLS column of the USED PRIMITIVES row naming name — the
// number a person reads out of `wake report`. Anchored from the front, at the field past
// PRIMITIVE, TYPE, HARNESS and LAST USED, rather than a substring search or a from-the-end
// offset: those four columns are each a single token by construction, but ERRORS (last)
// renders as two tokens ("1 (33.3%)") whenever a primitive has failures, which would shift
// a from-the-end offset onto the failure count instead of CALLS.
func reportedCalls(t *testing.T, rendered, name string) string {
	t.Helper()
	const callsIndex = 4 // PRIMITIVE, TYPE, HARNESS, LAST USED, then CALLS
	for line := range strings.Lines(rendered) {
		fields := strings.Fields(line)
		if len(fields) <= callsIndex || fields[0] != name {
			continue
		}
		return fields[callsIndex]
	}
	t.Fatalf("report names no primitive %q:\n%s", name, rendered)
	return ""
}

// invocationsOf refreshes the primitive inventory from the spool and returns what the
// snapshot says about one primitive — the number `wake report` and the dashboard
// render. Both go through inventory.Read over metrics.Aggregate, so one assertion here
// covers all three renderers: ADR-0011's single aggregation layer is what makes the
// reader's collapse inherited rather than re-implemented per output.
func invocationsOf(t *testing.T, spool *store.Store, statePath string, kind record.Kind, name record.Identifier) uint64 {
	t.Helper()
	primitives := inventory.New(statePath)
	discovery := inventory.Discovery{
		Primitives:     []inventory.Primitive{{Harness: "claude-code", Kind: kind, Name: name}},
		ProjectScanned: true,
	}
	if err := primitives.Refresh(spool, discovery); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	snapshot, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	for _, usage := range snapshot {
		if usage.Kind == kind && usage.Name == name {
			return usage.Invocations
		}
	}
	t.Fatalf("inventory holds no %s named %q: %+v", kind, name, snapshot)
	return 0
}

func TestClaudeCodeIsIdempotent(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	destination := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string, _ time.Time) (record.Hash, bool) { return repo, cwd == "/repo" }

	first, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	second, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	if first.Written != 1 || second.Duplicate != 1 {
		t.Fatalf("results = %+v, %+v", first, second)
	}
}

func TestClaudeCodePersistsBothToolCallsFromOneSourceEntry(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"},{"type":"tool_use","id":"call-2","name":"Skill","input":{"skill":"pr-review"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false},{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}`,
	}, "\n")
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string, _ time.Time) (record.Hash, bool) { return repo, cwd == "/repo" }

	first, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	if first.Written != 2 || first.Duplicate != 0 || first.Dropped != 0 {
		t.Fatalf("first ClaudeCode() = %+v, want two written records", first)
	}

	before, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	second, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	if second.Written != 0 || second.Duplicate != 2 {
		t.Fatalf("second ClaudeCode() = %+v, want both records recognised as duplicates", second)
	}
	after, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("re-ingest changed the spool:\nbefore %s\nafter  %s", before, after)
	}

	lines := 0
	for _, line := range bytes.Split(after, []byte("\n")) {
		if len(line) > 0 {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("events.ndjson holds %d lines, want 2: %s", lines, after)
	}
}

func TestClaudeCodeCountsARefusedCallWithoutWritingIt(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":"../secrets"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string, _ time.Time) (record.Hash, bool) { return repo, cwd == "/repo" }

	result, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("ClaudeCode() error = %v", err)
	}
	if result.Refused != 1 {
		t.Errorf("Refused = %d, want 1: the reader's refusal has to reach the caller", result.Refused)
	}
	// The refusal is its own fail-closed point: not a record the store dropped, not
	// an unusable line, and nothing parsed or written.
	if result.Written != 0 || result.Parsed != 0 || result.Malformed != 0 || result.Dropped != 0 {
		t.Errorf("ClaudeCode() = %+v, want nothing parsed, written or dropped", result)
	}
	if _, statErr := os.Stat(spool); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("Stat(spool) error = %v, want the spool never created", statErr)
	}
}

func TestClaudeCodePersistsNoPathShapedValue(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":"usr/local/bin"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","attributionSkill":"a/../secrets","attributionMcpServer":"plugin:a/../evil:tool","message":{"model":"C:/Users/me","content":[{"type":"tool_use","id":"call-2","name":"Bash"}]}}`,
		`{"uuid":"entry-4","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}`,
		// A directory-scoped primitive reference: only the keyed digest of the scope may
		// be persisted, never the path fragment it was derived from (ADR-0020).
		`{"uuid":"entry-5","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:04Z","message":{"content":[{"type":"tool_use","id":"call-3","name":"Skill","input":{"skill":"apps/web:reviewer"}}]}}`,
		`{"uuid":"entry-6","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-3","is_error":false}]}}`,
	}, "\n")
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string, _ time.Time) (record.Hash, bool) { return repo, cwd == "/repo" }

	result, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("ClaudeCode() error = %v", err)
	}
	if result.Written != 2 {
		t.Fatalf("ClaudeCode() = %+v, want 2 written", result)
	}

	raw, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	lines := 0
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) > 0 {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("events.ndjson holds %d lines: %s", lines, raw)
	}
	for _, fragment := range []string{"/", `\`, "usr/local", "secrets", "Users", "evil", "apps", "web"} {
		if bytes.Contains(raw, []byte(fragment)) {
			t.Fatalf("events.ndjson contains %q: %s", fragment, raw)
		}
	}
	if !bytes.Contains(raw, []byte("reviewer")) {
		t.Fatalf("events.ndjson dropped the safe half of a scoped reference: %s", raw)
	}
}

// One skill run is one invocation all the way to what a person reads. Claude Code's
// storage describes the run twice — the Skill tool_use/tool_result pair, and the
// attributed end_turn entry the run's own turn leaves behind — and the two carry
// legitimately distinct event ids, so the store cannot collapse them and the reader has
// to (ADR-0004, ADR-0002's invocation grain).
func TestClaudeCodeReportsOneSkillRunAsOneInvocation(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"model":"sonnet","content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":"pr-review"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string, _ time.Time) (record.Hash, bool) { return repo, cwd == "/repo" }

	result, err := ClaudeCode(strings.NewReader(input), resolve, names, closingStaleness, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("ClaudeCode() error = %v", err)
	}
	if result.Written != 1 || result.AmbiguousSkillRuns != 0 {
		t.Fatalf("ClaudeCode() = %+v, want one record and nothing uncertain", result)
	}
	if lines := spoolLines(t, spool); lines != 1 {
		t.Fatalf("events.ndjson holds %d lines, want 1", lines)
	}

	primitiveState := filepath.Join(t.TempDir(), "primitives.json")
	if got := invocationsOf(t, destination, primitiveState, record.KindSkill, "pr-review"); got != 1 {
		t.Errorf("inventory invocations = %d, want 1", got)
	}

	// The literal `wake report` path, so the number a person reads is asserted and not
	// inferred from the layer beneath it.
	var rendered bytes.Buffer
	if err := report.Print(&rendered, destination, inventory.New(primitiveState), report.Options{Usage: true}); err != nil {
		t.Fatalf("Print() error = %v", err)
	}
	if calls := reportedCalls(t, rendered.String(), "pr-review"); calls != "1" {
		t.Errorf("`wake report` shows %s calls for pr-review, want 1", calls)
	}
}

// A skill invoked as a slash command leaves no Skill tool call at all — the shape
// ADR-0023 §4 keeps a fallback record for. It resolves once the session closes, carries
// no outcome, and re-ingesting the same transcript writes nothing more: its event id
// comes from the source event, so the new record path is idempotent like every other
// (ADR-0004).
func TestClaudeCodeReportsAShapeASkillRunAsOneInvocation(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionSkill":"run-sdlc","message":{"model":"sonnet","stop_reason":"end_turn"}}`
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string, _ time.Time) (record.Hash, bool) { return repo, cwd == "/repo" }

	first, err := ClaudeCode(strings.NewReader(input), resolve, names, closingStaleness, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	if first.Written != 1 {
		t.Fatalf("first ClaudeCode() = %+v, want one record written", first)
	}

	entries, err := destination.Entries(0)
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
	if got := invocationsOf(t, destination, filepath.Join(t.TempDir(), "primitives.json"), record.KindSkill, "run-sdlc"); got != 1 {
		t.Errorf("inventory invocations = %d, want 1", got)
	}

	before, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	second, err := ClaudeCode(strings.NewReader(input), resolve, names, closingStaleness, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	if second.Written != 0 || second.Duplicate != 1 {
		t.Fatalf("second ClaudeCode() = %+v, want the record recognised as a duplicate", second)
	}
	after, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("re-ingest changed the spool:\nbefore %s\nafter  %s", before, after)
	}
}

// A subagent's own sidechain turn inherits the parent skill's attributionSkill, so it
// meets the attributed-run condition without being a skill invocation — the commonest
// attributed shape on a real machine (ADR-0023 §1). Nothing is written for it, and the
// spool is never created.
func TestClaudeCodeNeverReportsASidechainTurnAsASkillInvocation(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","isSidechain":true,"attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string, _ time.Time) (record.Hash, bool) { return repo, cwd == "/repo" }

	result, err := ClaudeCode(strings.NewReader(input), resolve, names, closingStaleness, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("ClaudeCode() error = %v", err)
	}
	// Not a refusal and nothing uncertain: the entry is readable and Wake deliberately
	// collects nothing from it, which must not read as lost collection in doctor.
	if result.Written != 0 || result.Parsed != 0 || result.Refused != 0 || result.AmbiguousSkillRuns != 0 {
		t.Errorf("ClaudeCode() = %+v, want a clean zero", result)
	}
	if _, statErr := os.Stat(spool); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("Stat(spool) error = %v, want the spool never created", statErr)
	}
}

// The accepted limitation ADR-0023 documents, carried to the caller: several
// attributed end_turn entries for one skill in one session are one record and a count
// of what was collapsed. The counter is uncertainty about that number and never a
// second invocation, so the spool holds one line and the inventory reports one.
func TestClaudeCodeReportsTheAmbiguityCounterWithoutASecondInvocation(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`,
	}, "\n")
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string, _ time.Time) (record.Hash, bool) { return repo, cwd == "/repo" }

	result, err := ClaudeCode(strings.NewReader(input), resolve, names, closingStaleness, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("ClaudeCode() error = %v", err)
	}
	if result.Written != 1 {
		t.Fatalf("ClaudeCode() = %+v, want exactly one record written", result)
	}
	if result.AmbiguousSkillRuns != 2 {
		t.Errorf("AmbiguousSkillRuns = %d, want 2 — the reader's count has to reach the caller", result.AmbiguousSkillRuns)
	}
	if lines := spoolLines(t, spool); lines != 1 {
		t.Errorf("events.ndjson holds %d lines, want 1", lines)
	}
	// The renderers' number, not just the store's: what doctor reports as uncertainty
	// may never appear as an invocation.
	if got := invocationsOf(t, destination, filepath.Join(t.TempDir(), "primitives.json"), record.KindSkill, "pr-review"); got != 1 {
		t.Errorf("inventory invocations = %d, want 1", got)
	}
}

func TestClaudeCodeWritesAnInterruptedCallExactlyOnce(t *testing.T) {
	// A session killed mid-call leaves a tool_use with no tool_result. Once the
	// staleness threshold has passed, the reader resolves it to outcome interrupted —
	// and because the id comes from the source event rather than from the scan
	// (ADR-0004), a second scan of the same transcript writes nothing more. That is
	// what makes a retry, a rescan and two concurrent scans all safe.
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`
	destination := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string, _ time.Time) (record.Hash, bool) { return repo, cwd == "/repo" }
	stale := claudecode.Staleness{
		Timeout: time.Hour,
		Now:     time.Date(2026, 8, 13, 14, 0, 0, 0, time.UTC),
	}

	first, err := ClaudeCode(strings.NewReader(input), resolve, names, stale, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	if first.Written != 1 || first.Interrupted != 1 || first.Duplicate != 0 || first.Pending != 0 {
		t.Fatalf("first ClaudeCode() = %+v", first)
	}

	second, err := ClaudeCode(strings.NewReader(input), resolve, names, stale, claudecode.Idleness{}, destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	if second.Written != 0 || second.Duplicate != 1 || second.Interrupted != 1 {
		t.Fatalf("second ClaudeCode() = %+v", second)
	}

	entries, err := destination.Entries(0)
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

// sessionInstant is the transcript instant the session-grain tests below build
// their two clocks around.
var sessionInstant = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// sessionFixture opens a spool and a resolver consenting to /repo, which is what
// every session-grain test below needs and nothing more.
func sessionFixture(t *testing.T) (string, *store.Store, claudecode.Resolver) {
	t.Helper()
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string, _ time.Time) (record.Hash, bool) { return repo, cwd == "/repo" }
	return spool, store.New(spool), resolve
}

// sessionEndsInSpool reads back every session-grain record the spool holds. The
// count is the assertion in every test below: one per session id, ever.
func sessionEndsInSpool(t *testing.T, spool string) []record.Record {
	t.Helper()
	entries, err := store.New(spool).Entries(0)
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

// TestClaudeCodeWritesOneSessionEndAcrossTwoScans is the store half of ADR-0034
// §1: the id is derived from (harness, session id, kind), so the second scan
// re-derives it and the store recognises it rather than appending a second copy.
func TestClaudeCodeWritesOneSessionEndAcrossTwoScans(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"model":"sonnet","id":"msg_1","usage":{"input_tokens":10,"output_tokens":20},"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	spool, destination, resolve := sessionFixture(t)
	idle := claudecode.Idleness{Timeout: 30 * time.Minute, Now: sessionInstant.Add(2 * time.Hour)}

	first, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, idle, destination)
	if err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	if first.Written != 2 {
		t.Fatalf("first ClaudeCode() = %+v, want the call and the session_end", first)
	}
	before, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	second, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, idle, destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	if second.Written != 0 || second.Duplicate != 2 {
		t.Fatalf("second ClaudeCode() = %+v, want both recognised as duplicates", second)
	}
	after, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("re-ingest changed the spool:\nbefore %s\nafter  %s", before, after)
	}
	if ends := sessionEndsInSpool(t, spool); len(ends) != 1 {
		t.Fatalf("the spool holds %d session_end records, want 1", len(ends))
	}
}

// TestClaudeCodeNeverCorrectsAWrittenSessionEnd is ADR-0034 §3's acceptance
// criterion verbatim, and the one only a store can prove.
//
// A session_end is written while one of its calls is still buffered, so its
// tool_calls is 0. A later scan resolves that call as interrupted and re-derives
// the same session_end with a tool_calls of 1. The store recognises the id and
// keeps the first: first write wins, whatever the recomputed totals say, because
// ADR-0015 rejected upsert and a comparison of payloads is what a dedup on
// event_id deliberately is not.
func TestClaudeCodeNeverCorrectsAWrittenSessionEnd(t *testing.T) {
	// One session, one unterminated tool_use.
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`
	spool, destination, resolve := sessionFixture(t)

	// Scan 1: the session is finished under a 30m idle threshold, and the call is
	// nowhere near stale under a 24h one.
	first, err := ClaudeCode(strings.NewReader(input), resolve, names,
		claudecode.Staleness{Timeout: 24 * time.Hour, Now: sessionInstant.Add(time.Hour)},
		claudecode.Idleness{Timeout: 30 * time.Minute, Now: sessionInstant.Add(time.Hour)},
		destination)
	if err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	if first.Written != 1 {
		t.Fatalf("first ClaudeCode() = %+v, want only the session_end", first)
	}
	ends := sessionEndsInSpool(t, spool)
	if len(ends) != 1 || ends[0].ToolCalls == nil || *ends[0].ToolCalls != 0 {
		t.Fatalf("session_end records = %+v, want one with tool_calls 0", ends)
	}
	writtenTimestamp := ends[0].Timestamp

	// Scan 2: the call is now stale too, so it resolves as interrupted and the
	// re-derived session_end would carry tool_calls 1.
	second, err := ClaudeCode(strings.NewReader(input), resolve, names,
		claudecode.Staleness{Timeout: 24 * time.Hour, Now: sessionInstant.Add(48 * time.Hour)},
		claudecode.Idleness{Timeout: 30 * time.Minute, Now: sessionInstant.Add(48 * time.Hour)},
		destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	if second.Written != 1 || second.Duplicate != 1 {
		t.Fatalf("second ClaudeCode() = %+v, want the interrupted call written and the session_end a duplicate", second)
	}

	ends = sessionEndsInSpool(t, spool)
	if len(ends) != 1 {
		t.Fatalf("the spool holds %d session_end records, want 1 — no upsert, no correction", len(ends))
	}
	if ends[0].ToolCalls == nil || *ends[0].ToolCalls != 0 {
		t.Errorf("tool_calls = %v, want the first scan's 0 — the record is never corrected", ends[0].ToolCalls)
	}
	if !ends[0].Timestamp.Equal(writtenTimestamp) {
		t.Errorf("Timestamp = %v, want the first scan's %v", ends[0].Timestamp, writtenTimestamp)
	}
	// The invocation record the second scan did write is there, so the undercount in
	// the session's totals is a stale snapshot and not lost collection.
	entries, err := store.New(spool).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	interrupted := 0
	for _, entry := range entries {
		if entry.Record.Outcome != nil && *entry.Record.Outcome == record.OutcomeInterrupted {
			interrupted++
		}
	}
	if interrupted != 1 {
		t.Errorf("interrupted records = %d, want 1", interrupted)
	}
}

// TestClaudeCodeWritesNoSecondSessionEndAfterResumedActivity is ADR-0034 §2: a
// session id is delimited once for its whole life. Activity resuming after the
// record was written yields its own invocation records and no second session_end,
// and does not move the first one's timestamp or totals.
func TestClaudeCodeWritesNoSecondSessionEndAfterResumedActivity(t *testing.T) {
	quiet := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"model":"sonnet","id":"msg_1","usage":{"input_tokens":10,"output_tokens":20},"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	// The same transcript with later activity appended under the same session id.
	resumed := strings.Join([]string{
		quiet,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T16:00:00Z","message":{"model":"sonnet","id":"msg_2","usage":{"input_tokens":70,"output_tokens":80},"content":[{"type":"tool_use","id":"call-2","name":"Read"}]}}`,
		`{"uuid":"entry-4","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T16:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}`,
	}, "\n")
	spool, destination, resolve := sessionFixture(t)

	if _, err := ClaudeCode(strings.NewReader(quiet), resolve, names, claudecode.Staleness{},
		claudecode.Idleness{Timeout: 30 * time.Minute, Now: sessionInstant.Add(2 * time.Hour)},
		destination); err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	ends := sessionEndsInSpool(t, spool)
	if len(ends) != 1 {
		t.Fatalf("session_end records = %d after the first scan, want 1", len(ends))
	}
	written := ends[0]

	// Finished again, on a clock past the resumed activity.
	second, err := ClaudeCode(strings.NewReader(resumed), resolve, names, claudecode.Staleness{},
		claudecode.Idleness{Timeout: 30 * time.Minute, Now: sessionInstant.Add(8 * time.Hour)},
		destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	// The resumed activity's own invocation record is written; the re-derived
	// session_end is not.
	if second.Written != 1 || second.Duplicate != 2 {
		t.Fatalf("second ClaudeCode() = %+v, want the new call written and the rest duplicates", second)
	}

	ends = sessionEndsInSpool(t, spool)
	if len(ends) != 1 {
		t.Fatalf("the spool holds %d session_end records, want 1 — a session id yields one, ever", len(ends))
	}
	if !ends[0].Timestamp.Equal(written.Timestamp) {
		t.Errorf("Timestamp = %v, want the first scan's %v", ends[0].Timestamp, written.Timestamp)
	}
	if ends[0].InputTokens == nil || *ends[0].InputTokens != *written.InputTokens {
		t.Errorf("input_tokens = %v, want the first scan's %d", ends[0].InputTokens, *written.InputTokens)
	}
}

// splitSessionSources are two transcripts of one session id — the shape ADR-0036
// is about: a parent's file and one subagent's, each with a terminated call and its
// own assistant usage block under a distinct message id.
func splitSessionSources() (parent, subagent string) {
	parent = strings.Join([]string{
		`{"uuid":"parent-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","entrypoint":"cli","message":{"model":"sonnet","id":"msg_parent","usage":{"input_tokens":10,"output_tokens":20},"content":[{"type":"tool_use","id":"call-parent","name":"Bash"}]}}`,
		`{"uuid":"parent-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","entrypoint":"cli","message":{"content":[{"type":"tool_result","tool_use_id":"call-parent","is_error":false}]}}`,
	}, "\n")
	subagent = strings.Join([]string{
		`{"uuid":"agent-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","entrypoint":"cli","message":{"model":"sonnet","id":"msg_agent","usage":{"input_tokens":7,"output_tokens":3},"content":[{"type":"tool_use","id":"call-agent","name":"Bash"}]}}`,
		`{"uuid":"agent-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:03Z","entrypoint":"cli","message":{"content":[{"type":"tool_result","tool_use_id":"call-agent","is_error":false}]}}`,
	}, "\n")
	return parent, subagent
}

// walkSources drives one ClaudeCodeScan over sources in the given order against a
// fresh spool, and returns the spool path and the Close result.
func walkSources(t *testing.T, idle claudecode.Idleness, sources ...string) (string, Result) {
	t.Helper()
	spool, destination, resolve := sessionFixture(t)
	scan := NewClaudeCodeScan(resolve, names, claudecode.Staleness{}, idle, destination)
	for index, source := range sources {
		if _, err := scan.Read(strings.NewReader(source)); err != nil {
			t.Fatalf("Read(source %d) error = %v", index, err)
		}
	}
	final, err := scan.Close()
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return spool, final
}

// One subagent run is one invocation all the way to what a person reads. Claude
// Code's storage describes the run twice — the parent transcript's invoking
// tool_use/tool_result pair, and the subagent's own transcript — and only the
// transcript is the canonical source, so the invoking pair contributes no record at
// all (ADR-0036 §1-§2, ADR-0002's invocation grain).
//
// Driven over two sources rather than one because that is the real shape, and because
// ClaudeCode's own doc comment says a caller reading a set of sources that may share
// a session id has to drive the scan.
func TestClaudeCodeScanReportsOneSubagentRunAsOneInvocation(t *testing.T) {
	parent := strings.Join([]string{
		`{"uuid":"parent-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","entrypoint":"cli","message":{"model":"sonnet","content":[{"type":"tool_use","id":"call-1","name":"Agent","input":{"subagent_type":"explorer"}}]}}`,
		`{"uuid":"parent-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:03Z","entrypoint":"cli","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	// The subagent's own transcript: the agent id on every entry, the name on none of
	// the first ones.
	subagent := strings.Join([]string{
		`{"uuid":"agent-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","entrypoint":"cli","isSidechain":true,"agentId":"agent-1","message":{"model":"sonnet","content":[]}}`,
		`{"uuid":"agent-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","entrypoint":"cli","isSidechain":true,"agentId":"agent-1","attributionAgent":"explorer","message":{"model":"sonnet","content":[]}}`,
	}, "\n")

	_, destination, resolve := sessionFixture(t)
	scan := NewClaudeCodeScan(resolve, names, closingStaleness, claudecode.Idleness{}, destination)
	written := 0
	for index, source := range []string{parent, subagent} {
		result, err := scan.Read(strings.NewReader(source))
		if err != nil {
			t.Fatalf("Read(source %d) error = %v", index, err)
		}
		written += result.Written
	}
	final, err := scan.Close()
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	written += final.Written

	if written != 1 {
		t.Fatalf("written = %d, want one record for one subagent run (final = %+v)", written, final)
	}
	if got := invocationsOf(t, destination, filepath.Join(t.TempDir(), "primitives.json"), record.KindSubagent, "explorer"); got != 1 {
		t.Errorf("inventory invocations = %d, want 1", got)
	}
}

// TestClaudeCodeScanWritesOneSessionEndAcrossTwoTranscripts is AC 1 at the store:
// one record for the session, with totals covering both of its transcripts.
func TestClaudeCodeScanWritesOneSessionEndAcrossTwoTranscripts(t *testing.T) {
	parent, subagent := splitSessionSources()
	idle := claudecode.Idleness{Timeout: 30 * time.Minute, Now: sessionInstant.Add(2 * time.Hour)}

	spool, _ := walkSources(t, idle, parent, subagent)

	ends := sessionEndsInSpool(t, spool)
	if len(ends) != 1 {
		t.Fatalf("the spool holds %d session_end records, want 1", len(ends))
	}
	end := ends[0]
	if end.ToolCalls == nil || *end.ToolCalls != 2 {
		t.Errorf("tool_calls = %v, want 2: one call from each transcript", end.ToolCalls)
	}
	if end.InputTokens == nil || *end.InputTokens != 17 {
		t.Errorf("input_tokens = %v, want 17: both transcripts' usage blocks", end.InputTokens)
	}
}

// TestClaudeCodeScanIsIndependentOfSourceOrder is AC 4 at the store, in both
// halves: what lands does not depend on the order the walk visited the sources in,
// and scanning the same set again is a byte-level no-op (ADR-0004).
//
// The comparison across orders is over the record set rather than the file's line
// order. Each source is persisted as it is read — requirement 1 keeps one Read per
// file and never buffers a whole walk's records to the end — so a reversed walk
// writes the same records in a different sequence, and the store appends in the
// order it is given.
func TestClaudeCodeScanIsIndependentOfSourceOrder(t *testing.T) {
	parent, subagent := splitSessionSources()
	idle := claudecode.Idleness{Timeout: 30 * time.Minute, Now: sessionInstant.Add(2 * time.Hour)}

	forwardSpool, forward := walkSources(t, idle, parent, subagent)
	reverseSpool, reverse := walkSources(t, idle, subagent, parent)

	if forward.Written+reverse.Written == 0 {
		t.Fatalf("neither walk wrote anything: %+v / %+v", forward, reverse)
	}
	if got, want := recordSet(t, reverseSpool), recordSet(t, forwardSpool); !slices.Equal(got, want) {
		t.Fatalf("the two walk orders wrote different records:\nforward  = %v\nreversed = %v", want, got)
	}

	// And the same set again, in the same order, is a no-op: every id re-derives, so
	// the store recognises it and the bytes do not move.
	before, err := os.ReadFile(forwardSpool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	destination := store.New(forwardSpool)
	_, _, resolve := sessionFixture(t)
	again := NewClaudeCodeScan(resolve, names, claudecode.Staleness{}, idle, destination)
	written := 0
	duplicate := 0
	for _, source := range []string{parent, subagent} {
		result, readErr := again.Read(strings.NewReader(source))
		if readErr != nil {
			t.Fatalf("Read() error = %v", readErr)
		}
		written += result.Written
		duplicate += result.Duplicate
	}
	final, err := again.Close()
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	written += final.Written
	duplicate += final.Duplicate
	if written != 0 {
		t.Errorf("re-scan wrote %d records, want 0", written)
	}
	if duplicate == 0 {
		t.Error("re-scan recognised no duplicates, so the ids did not re-derive")
	}
	after, err := os.ReadFile(forwardSpool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("re-scan changed the spool:\nbefore %s\nafter  %s", before, after)
	}
}

// recordSet reads a spool back as the sorted list of its event ids, so two walks
// can be compared as sets. An event id identifies a record (ADR-0004), so the
// order is total.
func recordSet(t *testing.T, spool string) []string {
	t.Helper()
	entries, err := store.New(spool).Entries(0)
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

// TestClaudeCodeScanReportsASourceThatProducedNothing is doctor's Skipped counter
// after the hoist: it is reported by the walk, because a source's contribution can
// now resolve after that source's own read has ended.
func TestClaudeCodeScanReportsASourceThatProducedNothing(t *testing.T) {
	collecting := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","entrypoint":"cli","message":{"model":"sonnet","id":"msg_1","content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}
{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","entrypoint":"cli","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`
	// A working directory belonging to no consented repository: read fine, collected
	// nothing, refused nothing. The clean zero doctor calls skipped.
	unconsented := `{"uuid":"other-1","sessionId":"session-2","cwd":"/elsewhere","timestamp":"2026-08-13T12:00:00Z","entrypoint":"cli","message":{"model":"sonnet","id":"msg_2","content":[{"type":"tool_use","id":"call-2","name":"Bash"}]}}`

	_, final := walkSources(t, claudecode.Idleness{}, collecting, unconsented)

	if final.SkippedSources != 1 {
		t.Fatalf("SkippedSources = %d, want 1: one of the two sources produced nothing", final.SkippedSources)
	}
}
