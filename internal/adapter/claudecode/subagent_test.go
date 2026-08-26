package claudecode

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// subagentTranscript is one subagent's own transcript: several entries all carrying
// the same agentId and sessionId, the first of them declaring no name — the shape a
// real one has (agentId on 32787/32787 entries, attributionAgent on 0/400 first
// entries). name == "" produces the 2% case that declares none at all.
func subagentTranscript(t *testing.T, agentID, session, name string) string {
	t.Helper()
	return subagentTranscriptIn(t, agentID, session, name, consentedPath)
}

// subagentTranscriptIn is subagentTranscript with the recorded working directory
// under the caller's control, for the tests about the consent boundary.
func subagentTranscriptIn(t *testing.T, agentID, session, name, cwd string) string {
	t.Helper()
	lines := make([]string, 0, 3)
	for index, at := range []string{"2026-08-13T12:00:00Z", "2026-08-13T12:00:01Z", "2026-08-13T12:00:02Z"} {
		// Only the entries after the first declare the name, which is what the fold has
		// to survive: emitting on first sight of the agent id would refuse every real
		// subagent (BC-4).
		attribution := ""
		if index > 0 && name != "" {
			attribution = fmt.Sprintf(`,"attributionAgent":%s`, quoted(t, name))
		}
		lines = append(lines, fmt.Sprintf(
			`{"uuid":%q,"sessionId":%q,"cwd":%s,"timestamp":%q,"version":"1.0.0","entrypoint":"cli",`+
				`"isSidechain":true,"agentId":%s%s,"message":{"model":"sonnet","content":[]}}`,
			fmt.Sprintf("%s-entry-%d", agentID, index), session, quoted(t, cwd), at, quoted(t, agentID), attribution))
	}
	return strings.Join(lines, "\n")
}

// subagentInvocationLines is the invoking side: the parent transcript's Agent
// tool_use and the tool_result that terminated it. It carries no agentId, because a
// parent transcript never does (0/39039 measured).
func subagentInvocationLines(callID, session, subagentType string) string {
	input := ""
	if subagentType != "" {
		input = fmt.Sprintf(`,"input":{"subagent_type":%q}`, subagentType)
	}
	return strings.Join([]string{
		fmt.Sprintf(`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"1.0.0","entrypoint":"cli",`+
			`"message":{"model":"sonnet","content":[{"type":"tool_use","id":%q,"name":"Agent"%s}]}}`,
			callID+"-use", session, callID, input),
		fmt.Sprintf(`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":"2026-08-13T12:00:03Z","version":"1.0.0","entrypoint":"cli",`+
			`"message":{"content":[{"type":"tool_result","tool_use_id":%q,"is_error":false}]}}`,
			callID+"-result", session, callID),
	}, "\n")
}

// subagentRecords selects the subagent records out of a walk's output.
func subagentRecords(records []record.Record) []record.Record {
	selected := []record.Record{}
	for _, event := range records {
		if event.Kind == record.KindSubagent {
			selected = append(selected, event)
		}
	}
	return selected
}

// AC 1: a session invoking three different subagents yields three records carrying
// three distinct names — not one named after the tool that entered them (ADR-0002's
// invocation grain, ADR-0036 §1's precedence table).
func TestScanNamesEachSubagentFromItsOwnTranscript(t *testing.T) {
	records, result := twoSources(t, closedSession, Idleness{},
		subagentTranscript(t, "agent-1", "session-1", "explorer"),
		subagentTranscript(t, "agent-2", "session-1", "code-reviewer"),
		subagentTranscript(t, "agent-3", "session-1", "general-purpose"))

	if len(records) != 3 {
		t.Fatalf("records = %+v, want one per subagent run (result = %+v)", records, result)
	}
	seen := map[record.Identifier]bool{}
	for _, event := range records {
		if event.Kind != record.KindSubagent {
			t.Errorf("record kind = %q, want %q", event.Kind, record.KindSubagent)
		}
		if event.Name == "Agent" || event.Name == "Task" {
			t.Errorf("record is named after the tool that entered the subagent: %+v", event)
		}
		if err := record.Validate(event); err != nil {
			t.Errorf("Validate(%+v) error = %v", event, err)
		}
		seen[event.Name] = true
	}
	for _, want := range []record.Identifier{"explorer", "code-reviewer", "general-purpose"} {
		if !seen[want] {
			t.Errorf("no record named %q; got %v", want, seen)
		}
	}
}

// AC 2: the name comes from the transcript, so an invoking call that named no
// subagent type costs nothing. That call is 18% of real ones (26 of 144 measured).
func TestScanNamesASubagentWhoseInvocationNamedNoType(t *testing.T) {
	records, result := twoSources(t, closedSession, Idleness{},
		subagentInvocationLines("call-1", "session-1", ""),
		subagentTranscript(t, "agent-1", "session-1", "explorer"))

	if len(records) != 1 {
		t.Fatalf("records = %+v, want exactly the subagent record (result = %+v)", records, result)
	}
	if records[0].Kind != record.KindSubagent || records[0].Name != "explorer" {
		t.Errorf("record = %+v, want the explorer subagent named from its own transcript", records[0])
	}
}

// AC 3: a transcript that declares no name is refused and counted, never named by
// inference from the harness's documented default (ADR-0036 §2 and its Alternatives).
func TestScanRefusesAndCountsASubagentTranscriptDeclaringNoName(t *testing.T) {
	records, result := twoSources(t, closedSession, Idleness{},
		subagentTranscript(t, "agent-1", "session-1", ""))

	if len(records) != 0 {
		t.Fatalf("records = %+v, want none: nothing names this run", records)
	}
	if result.Refused != 1 {
		t.Errorf("Refused = %d, want 1: an unnamed subagent is collection Wake lost", result.Refused)
	}
	if result.Malformed != 0 || result.Pending != 0 {
		t.Errorf("result = %+v, want no unusable line and nothing buffered", result)
	}
}

// AC 4: one subagent run is one record, though Claude Code's storage describes it
// twice — the parent's invoking tool_use/tool_result pair, and the subagent's own
// transcript. Only the transcript is the canonical source (ADR-0036 §1-§2).
func TestScanCountsOneSubagentRunAsOneRecord(t *testing.T) {
	records, result := twoSources(t, closedSession, Idleness{},
		subagentInvocationLines("call-1", "session-1", "explorer"),
		subagentTranscript(t, "agent-1", "session-1", "explorer"))

	if len(records) != 1 {
		t.Fatalf("records = %+v, want exactly one for one run (result = %+v)", records, result)
	}
	event := records[0]
	if event.Kind != record.KindSubagent || event.Name != "explorer" {
		t.Fatalf("record = %+v, want the explorer subagent record", event)
	}
	// The completion boundary lives on the invoking side, and ADR-0036 §5 declines
	// that correlation outright — so the outcome is absent rather than synthesized
	// (BC-5, ADR-0005).
	if event.Outcome != nil {
		t.Errorf("Outcome = %v, want nil: a synthesized outcome is forbidden", *event.Outcome)
	}
	summary := metrics.Aggregate(records)
	if len(summary.Primitives) != 1 || summary.Primitives[0].Invocations != 1 {
		t.Errorf("Aggregate() primitives = %+v, want explorer with one invocation", summary.Primitives)
	}
}

// ADR-0002's invocation grain: two runs of one subagent are two rows that aggregate
// to one primitive, so nothing is lost before aggregation collapses them.
func TestScanDerivesTwoRecordsForTwoRunsOfOneSubagent(t *testing.T) {
	records, result := twoSources(t, closedSession, Idleness{},
		subagentTranscript(t, "agent-1", "session-1", "explorer"),
		subagentTranscript(t, "agent-2", "session-1", "explorer"))

	if len(records) != 2 {
		t.Fatalf("records = %+v, want one per run (result = %+v)", records, result)
	}
	if records[0].EventID == records[1].EventID {
		t.Errorf("both runs derived the same event id %q", records[0].EventID)
	}
	summary := metrics.Aggregate(records)
	if len(summary.Primitives) != 1 || summary.Primitives[0].Invocations != 2 {
		t.Errorf("Aggregate() primitives = %+v, want one explorer primitive with two invocations", summary.Primitives)
	}
}

// A subagent record counts toward its session's ToolCalls exactly as a call
// terminated inside a source does, and never toward BuiltinToolCalls — which is what
// the invoking Agent call used to be counted as. The count exists so a receiver can
// recover the denominator encodeSpan's built-in-tool omission destroys (ADR-0006), so
// total minus builtin has to stay the number of spans the session delivers.
//
// It holds because Close resolves the subagent runs before the tally reads them; a
// refactor that moved the resolution after it would silently understate every session
// that ran a subagent.
func TestScanCountsASubagentRecordAsAToolCall(t *testing.T) {
	records, result := twoSources(t, closedSession, finished,
		subagentTranscript(t, "agent-1", "session-1", "explorer"))

	ends := sessionEnds(records)
	if len(ends) != 1 {
		t.Fatalf("session_end records = %d, want 1 (result = %+v)", len(ends), result)
	}
	assertCount(t, "ToolCalls", ends[0].ToolCalls, 1)
	assertCount(t, "BuiltinToolCalls", ends[0].BuiltinToolCalls, 0)
}

// BC-4, asserted rather than assumed: the record resolves at session close and never
// on first sight of the agent id. An eagerly emitted record is permanent — ADR-0015
// rejects upsert and ADR-0004 deduplicates the correction away — so the bug this
// guards cannot be fixed by a later scan.
func TestScanDefersASubagentUntilItsSessionCloses(t *testing.T) {
	source := subagentTranscript(t, "agent-1", "session-1", "explorer")

	open := Staleness{Timeout: time.Hour, Now: callInstant.Add(30 * time.Minute)}
	records, result := twoSources(t, open, Idleness{}, source)
	if len(records) != 0 {
		t.Fatalf("records = %+v, want none while the session is open", records)
	}
	if result.OpenSessions != 1 || result.Pending != 0 || result.Refused != 0 {
		t.Errorf("result = %+v, want one open session, nothing pending and no refusal", result)
	}

	closed, _ := twoSources(t, closedSession, Idleness{}, source)
	if len(closed) != 1 {
		t.Fatalf("records = %+v, want the record once the session closed", closed)
	}
}

// BC-4's teeth: the agent id is on every entry and the name is on none of the first
// ones, so a single fold would refuse a subagent whose name arrives three entries
// later — permanently, and at a far higher rate than ADR-0036's measured 2%.
func TestScanDoesNotRefuseASubagentWhoseNameArrivesLater(t *testing.T) {
	records, result := twoSources(t, closedSession, Idleness{},
		subagentTranscript(t, "agent-1", "session-1", "explorer"))

	if len(records) != 1 || records[0].Name != "explorer" {
		t.Fatalf("records = %+v, want the explorer record (result = %+v)", records, result)
	}
	if result.Refused != 0 {
		t.Errorf("Refused = %d, want 0: the name arrived, just not on the first entry", result.Refused)
	}
}

// ADR-0004 / BC-11: what the walk derives cannot depend on the order it visited the
// sources in. Compared as a set ordered by event id, so the emission sequence — per
// source by construction — is not what is being asserted.
func TestScanDerivesTheSameSubagentRecordInEitherSourceOrder(t *testing.T) {
	first := subagentTranscript(t, "agent-1", "session-1", "explorer")
	second := subagentTranscript(t, "agent-2", "session-1", "code-reviewer")
	third := subagentTranscript(t, "agent-3", "session-1", "general-purpose")

	forward, _ := twoSources(t, closedSession, Idleness{}, first, second, third)
	reverse, _ := twoSources(t, closedSession, Idleness{}, third, second, first)

	if len(forward) != len(reverse) {
		t.Fatalf("record counts differ: %d forward, %d reversed", len(forward), len(reverse))
	}
	byEventID(forward)
	byEventID(reverse)
	if !reflect.DeepEqual(forward, reverse) {
		t.Fatalf("records differ by walk order:\nforward  = %+v\nreversed = %+v", forward, reverse)
	}
}

// AC 6 / BC-11: re-scanning the same sources yields byte-identical store contents,
// which is what makes the cursor an optimisation and re-ingestion a no-op (ADR-0004).
func TestScanDerivesTheSameSubagentRecordOnReingest(t *testing.T) {
	sources := []string{
		subagentTranscript(t, "agent-1", "session-1", "explorer"),
		subagentTranscript(t, "agent-2", "session-1", "apps/web:reviewer"),
	}

	first, _ := twoSources(t, closedSession, finished, sources...)
	second, _ := twoSources(t, closedSession, finished, sources...)

	byEventID(first)
	byEventID(second)
	if len(first) != len(second) {
		t.Fatalf("record counts differ between walks: %d and %d", len(first), len(second))
	}
	for index := range first {
		before, err := record.Marshal(first[index])
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		after, err := record.Marshal(second[index])
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		if string(before) != string(after) {
			t.Fatalf("re-scan derived different bytes:\nfirst  %s\nsecond %s", before, after)
		}
	}
}

// An agent id outside the token domain is a clean zero, not a refusal — call's own
// rule for an unusable block id. Such a value would make the composed source
// identity ambiguous, so nothing is derived from it and nothing is reported lost.
func TestScanSkipsASubagentTranscriptWhoseAgentIDIsOutsideTheTokenDomain(t *testing.T) {
	for _, value := range hostileValues {
		records, result := twoSources(t, closedSession, Idleness{},
			subagentTranscript(t, value, "session-1", "explorer"))

		if len(records) != 0 || result.Refused != 0 {
			t.Errorf("agentId = %q: records = %+v, Refused = %d; want a clean zero", value, records, result.Refused)
		}
	}
}

// Consent is resolved from the subagent transcript's own cwd, as a pure string
// operation (BC-7, ADR-0019 §1). An unconsented directory is outside collection
// rather than lost from it, so it is neither a record nor a refusal.
func TestScanSkipsASubagentTranscriptInAnUnconsentedRepository(t *testing.T) {
	scan := NewScan(deny, names, closedSession, Idleness{})
	if _, err := scan.Read(strings.NewReader(subagentTranscript(t, "agent-1", "session-1", "explorer"))); err != nil {
		t.Fatalf("Scan.Read() error = %v", err)
	}
	result := scan.Close()

	if len(result.Records) != 0 || result.Refused != 0 {
		t.Errorf("result = %+v, want no record and no refusal", result)
	}
}

// BC-3: the store now carries four event-id shapes and they must be structurally
// disjoint. ADR-0035's Context records what a carelessly hashed fourth shape costs —
// "a valid-looking hex id no record in the store has or ever will".
func TestScanDerivesASubagentIDThatCollidesWithNoOtherShape(t *testing.T) {
	// A composed identity carries its own separator and no other, and no token-domain
	// value can carry any of them.
	composed := string(subagentSourceEvent("agent-1"))
	if !strings.Contains(composed, subagentSeparator) {
		t.Errorf("subagentSourceEvent() = %q, want the subagent separator", composed)
	}
	if strings.Contains(composed, callSeparator) || strings.Contains(composed, sessionSeparator) {
		t.Errorf("subagentSourceEvent() = %q, want neither the call nor the session separator", composed)
	}

	// And the four shapes, derived together in one walk: a tool call, a Shape-A skill
	// fallback, a session_end and a subagent run.
	records, result := twoSources(t, closedSession, finished,
		strings.Join([]string{
			toolCallLines("parent-1", "session-1", "2026-08-13T12:00:00Z", "call-1", "Bash", `{}`),
			attributedRun("parent-2", "session-1", "2026-08-13T12:00:01Z", "pr-review"),
		}, "\n"),
		subagentTranscript(t, "agent-1", "session-1", "explorer"))

	if len(records) != 4 {
		t.Fatalf("records = %+v, want one of each shape (result = %+v)", records, result)
	}
	ids := map[record.Hash]record.Kind{}
	for _, event := range records {
		if previous, clash := ids[event.EventID]; clash {
			t.Fatalf("event id collision between a %q record and a %q one", previous, event.Kind)
		}
		ids[event.EventID] = event.Kind
	}
	if len(subagentRecords(records)) != 1 {
		t.Errorf("subagent records = %d, want 1", len(subagentRecords(records)))
	}
}
