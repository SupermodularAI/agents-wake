package claudecode

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

const repo = record.Hash("0123456789abcdef0123456789abcdef")

// hostileValues is the Claude Code adapter's hostile-payload corpus. ADR-0007
// requires one per adapter: each adapter is a new input shape, and the privacy
// guarantee has to hold for every one of them.
var hostileValues = []string{
	"/usr/local/bin", "usr/local/bin", "../secrets", "./relative", "~/.ssh/id_rsa",
	`C:\Windows\System32`, "C:temp", `\\server\share`, "contains space", "tab\there",
	// Already wearing the shape DerivedName produces, so a transcript cannot
	// hand-craft a name that collides with a real scope digest (ADR-0020), and one
	// value past the identifier length bound.
	"scope-0123456789ab:reviewer", strings.Repeat("a", 129),
}

func TestReadDerivesOnlyTerminalRecords(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"1.0.0","attributionMcpServer":"plugin:atlassian:cloud","attributionMcpTool":"search","attributionSkill":"jira-work","message":{"model":"sonnet","content":[{"type":"tool_use","id":"call-1","name":"mcp__atlassian__search"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Pending != 0 || result.Malformed != 0 {
		t.Fatalf("Read() = %+v", result)
	}
	event := result.Records[0]
	if event.Kind != record.KindMCPTool || event.Package != "atlassian" || event.ViaSkill != "jira-work" || event.Outcome == nil || *event.Outcome != record.OutcomeOK {
		t.Fatalf("record = %+v", event)
	}
}

func TestReadDoesNotEmitUnterminatedCall(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill"}]}}`
	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	// The zero Staleness is the pre-T114 contract and stays it: a caller that carries
	// no threshold resolves nothing, and the session is reported open rather than
	// silently closed.
	if len(result.Records) != 0 || result.Pending != 1 || result.Interrupted != 0 || result.OpenSessions != 1 {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadUsesSkillNameInsteadOfToolName(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":"pr-review","args":"never retain this prose"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Records[0].Name != "pr-review" || result.Records[0].Kind != record.KindSkill {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadDerivesTerminalAttributedSkill(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionSkill":"run-sdlc","message":{"model":"sonnet","stop_reason":"end_turn"}}`
	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
	event := result.Records[0]
	if event.Kind != record.KindSkill || event.Name != "run-sdlc" || event.Invoker != record.InvokerUser || event.Outcome != nil {
		t.Fatalf("record = %+v", event)
	}
}

// An attributed end_turn entry is not a second subagent invocation. The Task call
// that entered the subagent is the invocation, and the parent transcript carries it
// as a paired tool_use/tool_result with an outcome; counting the attributed entry
// too would report one run as two.
func TestReadDerivesNoSubagentRecordFromAttributionAlone(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionAgent":"sdlc-check-architecture","message":{"model":"sonnet","stop_reason":"end_turn"}}`
	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 {
		t.Fatalf("Read() records = %+v, want none: the Task call is the invocation", result.Records)
	}
	// It is not a refusal either: the entry is readable and Wake deliberately
	// collects nothing from it, which must not read as lost collection in doctor.
	if result.Refused != 0 || result.Malformed != 0 {
		t.Errorf("Read() = %+v, want no refusal and no malformed line", result)
	}
}

// A subagent's own sidechain turn inherits the parent skill's attributionSkill, so
// it meets the attributed-run condition without being a skill invocation at all —
// 183 of 258 real attributed entries are this shape, all isSidechain: true
// (ADR-0023 Context). It is excluded on sight, before any session-close buffering:
// nothing about it is deferred or uncertain.
func TestReadNeverCountsASidechainTurnAsASkillInvocation(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","isSidechain":true,"attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`

	result, err := Read(strings.NewReader(input), resolver, names, closedSession)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 {
		t.Fatalf("Read() records = %+v, want none: a sidechain turn is not a skill invocation", result.Records)
	}
	// Not a refusal and not an unusable line either: the entry is perfectly readable
	// and Wake deliberately collects nothing from it, which must not read as lost
	// collection in doctor.
	if result.Refused != 0 || result.Malformed != 0 {
		t.Errorf("Read() = %+v, want no refusal and no malformed line", result)
	}
}

func TestReadRecordsAttributingAgentForPrimitiveCalls(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionAgent":"sdlc-implement","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":"commit-message"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Records[0].ViaAgent != "sdlc-implement" {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadDoesNotEmitUnfinishedAttributedRun(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionAgent":"sdlc-check-architecture","message":{"model":"sonnet","stop_reason":"tool_use"}}`
	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
}

func TestReadSkipsUnconsentedRepository(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/outside","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/outside","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), func(string) (record.Hash, bool) { return "", false }, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 {
		t.Fatalf("Read() emitted records for an unconsented repository: %+v", result.Records)
	}
}

func TestReadKeepsUnknownOutcomeNull(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1"}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Records[0].Outcome != nil {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadSkipsMalformedLine(t *testing.T) {
	input := strings.Join([]string{
		`not json`,
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Task","input":{"subagent_type":"explorer"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":true}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if result.Malformed != 1 || len(result.Records) != 1 || result.Records[0].Kind != record.KindSubagent {
		t.Fatalf("Read() = %+v", result)
	}
}

// twoCallsInOneEntry is a transcript whose single tool_use entry carries two
// calls, terminated together by one later entry. Claude Code emits this shape
// whenever the model calls two tools in one assistant turn.
const twoCallsInOneEntry = `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"},{"type":"tool_use","id":"call-2","name":"Task","input":{"subagent_type":"explorer"}}]}}
{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false},{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}`

func TestReadDerivesADistinctIDForEachToolCallInOneEntry(t *testing.T) {
	result, err := Read(strings.NewReader(twoCallsInOneEntry), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("Read() records = %+v, want one per tool call", result.Records)
	}
	if result.Records[0].EventID == result.Records[1].EventID {
		t.Errorf("both tool calls derived the same event id %q", result.Records[0].EventID)
	}
	run := record.DeriveEventID(harness, record.Identifier("entry-1"))
	for index, event := range result.Records {
		if event.EventID == run {
			t.Errorf("record %d derives the id a terminal run of entry-1 would: %q", index, event.EventID)
		}
	}
}

func TestReadDerivesTheSameToolCallIDsOnReingest(t *testing.T) {
	first, err := Read(strings.NewReader(twoCallsInOneEntry), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("first Read() error = %v", err)
	}
	second, err := Read(strings.NewReader(twoCallsInOneEntry), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("second Read() error = %v", err)
	}
	if len(first.Records) != len(second.Records) {
		t.Fatalf("record counts = %d and %d", len(first.Records), len(second.Records))
	}
	for index := range first.Records {
		if first.Records[index].EventID != second.Records[index].EventID {
			t.Errorf("record %d id = %q on re-read, was %q", index, second.Records[index].EventID, first.Records[index].EventID)
		}
	}
}

func TestReadDoesNotCollideAToolCallWithATerminalRun(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionSkill":"pr-review","message":{"stop_reason":"end_turn","content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("Read() records = %+v, want a terminal run and a tool call", result.Records)
	}
	run, call := result.Records[0], result.Records[1]
	if run.Kind != record.KindSkill || run.Outcome != nil {
		t.Errorf("first record is not the terminal skill run: %+v", run)
	}
	if call.Kind != record.KindBuiltinTool {
		t.Errorf("second record is not the tool call: %+v", call)
	}
	if run.EventID == call.EventID {
		t.Errorf("a tool call and a terminal run of the same entry share id %q", run.EventID)
	}
}

func TestReadRejectsAnEntryIDOutsideTheTokenDomain(t *testing.T) {
	input := strings.Join([]string{
		// The first id carries the separator itself and the second is path-shaped.
		// Either would make a composed tool call identity ambiguous, so both are
		// refused rather than escaped.
		fmt.Sprintf(`{"uuid":%s,"sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionSkill":"pr-review","message":{"stop_reason":"end_turn"}}`, quoted(t, "entry"+callSeparator+"1")),
		fmt.Sprintf(`{"uuid":%s,"sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","attributionSkill":"pr-review","message":{"stop_reason":"end_turn"}}`, quoted(t, "../escape")),
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 {
		t.Errorf("Read() records = %+v, want none", result.Records)
	}
	if result.Malformed != 2 {
		t.Errorf("Malformed = %d, want 2", result.Malformed)
	}
}

func TestReadCountsAnOversizedLineWithoutRetainingIt(t *testing.T) {
	oversized := `{"uuid":"entry-x","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-x","name":"Bash"}]},"pad":"swordfish` + strings.Repeat("A", 1024*1024) + `"}`
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
		oversized,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","message":{"content":[{"type":"tool_use","id":"call-2","name":"Task","input":{"subagent_type":"code-reviewer"}}]}}`,
		`{"uuid":"entry-4","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("Read() records = %d, want a record on each side of the oversized line: %+v", len(result.Records), result)
	}
	if result.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1", result.Malformed)
	}
	if result.Pending != 0 {
		t.Errorf("Pending = %d, want 0", result.Pending)
	}
	for _, event := range result.Records {
		encoded, marshalErr := record.Marshal(event)
		if marshalErr != nil {
			t.Fatalf("Marshal() error = %v", marshalErr)
		}
		for _, fragment := range []string{"swordfish", "entry-x"} {
			if strings.Contains(string(encoded), fragment) {
				t.Fatalf("record retains %q from the oversized line: %s", fragment, encoded)
			}
		}
	}
}

func TestReadNeverUsesToolArguments(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"args":"do not retain this secret"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	encoded, err := record.Marshal(result.Records[0])
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), "do not retain") {
		t.Fatalf("record contains source argument: %s", encoded)
	}
}

// forwardOrderPair is the canonical terminated Bash call: the tool_use line, then
// the tool_result line that terminates it. reversedOrderPair is the same two lines
// in the order Claude Code writes them in a small fraction of transcripts — the
// result first. Each line keeps its own uuid and timestamp in both orders, so the
// only difference between the two transcripts is arrival order.
var (
	toolUseLine    = `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"1.0.0","message":{"model":"sonnet","content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`
	toolResultLine = `{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`

	forwardOrderPair  = toolUseLine + "\n" + toolResultLine
	reversedOrderPair = toolResultLine + "\n" + toolUseLine
)

// A tool_result line can precede its own tool_use line. That is an out-of-order
// write, not a malformed one, and the forward-only pairing loop used to drop the
// result and then buffer the call forever — which, once the staleness path is live,
// writes a call that genuinely succeeded as outcome: interrupted, permanently
// (ADR-0004 dedup never upserts, ADR-0015).
func TestReadCompletesACallWhoseResultLinePrecedesIt(t *testing.T) {
	result, err := Read(strings.NewReader(reversedOrderPair), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Pending != 0 || result.Malformed != 0 || result.Refused != 0 {
		t.Fatalf("Read() = %+v, want one record and no counter moved", result)
	}
	event := result.Records[0]
	if event.Outcome == nil || *event.Outcome != record.OutcomeOK {
		t.Errorf("Outcome = %v, want the real outcome the result line carries", event.Outcome)
	}
	if event.Kind != record.KindBuiltinTool || event.Name != "Bash" {
		t.Errorf("record = %+v, want the Bash call", event)
	}
	// The tool_use entry is what the call's own metadata comes from, in either order.
	if event.HarnessVersion != "1.0.0" || event.Model != "sonnet" {
		t.Errorf("HarnessVersion = %q, Model = %q, want them from the tool_use entry", event.HarnessVersion, event.Model)
	}
	// And the result entry is what stamps the timestamp, exactly as complete has
	// always stamped it.
	if want := record.NormalizedTimestamp(parsedTime(t, "2026-08-13T12:00:01Z")); !event.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want the result entry's %v", event.Timestamp, want)
	}
}

// Ordering may not reach the derived identity at all: callSourceEvent composes the
// tool_use entry's uuid with the block's own id, and both halves are the same
// whichever line the reader saw first (ADR-0004). Comparing the marshalled bytes
// covers the id, the timestamp and every other field in one assertion, which is
// what "no change to the forward-order path" actually asks for.
func TestReadDerivesTheSameRecordInEitherLineOrder(t *testing.T) {
	encoded := make([]string, 0, 2)
	for _, transcript := range []string{forwardOrderPair, reversedOrderPair} {
		result, err := Read(strings.NewReader(transcript), resolver, names, Staleness{})
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if len(result.Records) != 1 {
			t.Fatalf("Read() records = %+v, want exactly one", result.Records)
		}
		marshalled, err := record.Marshal(result.Records[0])
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		encoded = append(encoded, string(marshalled))
	}
	if encoded[0] != encoded[1] {
		t.Errorf("record differs by line order:\n forward  = %s\n reversed = %s", encoded[0], encoded[1])
	}
}

// A result whose tool_use never arrives is counted nowhere. It cannot become a
// record — there is no name, kind, invoker or consented repository to build one
// from — and it must not be folded into Pending either: Pending means "accepted
// calls awaiting a result", and the staleness path ages exactly those into
// interrupted. A call that does not exist has nothing to age.
func TestReadEmitsNothingForAResultWhoseCallNeverArrives(t *testing.T) {
	result, err := Read(strings.NewReader(toolResultLine), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 || result.Pending != 0 || result.Malformed != 0 || result.Refused != 0 {
		t.Fatalf("Read() = %+v, want nothing emitted and no counter moved", result)
	}
}

func TestReadDerivesTheRealOutcomeWhenTheResultArrivesFirst(t *testing.T) {
	ok, failed := record.OutcomeOK, record.OutcomeError
	deniedPolicy, deniedUser := record.OutcomeDeniedPolicy, record.OutcomeDeniedUser
	interrupted := record.OutcomeInterrupted

	for _, testCase := range []struct {
		name        string
		entryFields string
		blockFields string
		want        *record.Outcome
	}{
		{name: "ok", blockFields: `,"is_error":false`, want: &ok},
		{name: "error", blockFields: `,"is_error":true`, want: &failed},
		// ADR-0005: absent is first-class null, never coerced to ok.
		{name: "unknown"},
		{name: "denied by policy", entryFields: `"toolDenialKind":"permission-rule",`, blockFields: `,"is_error":true`, want: &deniedPolicy},
		{name: "denied by user", entryFields: `"toolDenialKind":"user-rejected",`, blockFields: `,"is_error":true`, want: &deniedUser},
		// The control: interrupted may only ever come from the source saying so. The
		// five rows above prove arrival order alone never produces it.
		{name: "interrupted", entryFields: `"toolUseResult":{"interrupted":true},`, want: &interrupted},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			transcript := reversedPairWith(testCase.entryFields, testCase.blockFields)
			result, err := Read(strings.NewReader(transcript), resolver, names, Staleness{})
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if len(result.Records) != 1 {
				t.Fatalf("Read() records = %+v, want exactly one", result.Records)
			}
			got := result.Records[0].Outcome
			if (got == nil) != (testCase.want == nil) {
				t.Fatalf("Outcome = %v, want %v", got, testCase.want)
			}
			if got != nil && *got != *testCase.want {
				t.Errorf("Outcome = %q, want %q", *got, *testCase.want)
			}
		})
	}
}

func TestReadDoesNotEmitAnEarlyResultForAnUnconsentedCall(t *testing.T) {
	transcript := strings.Join([]string{
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/outside","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/outside","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(transcript), func(string) (record.Hash, bool) { return "", false }, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	// A clean zero, not a refusal: an unconsented directory is outside collection
	// rather than lost from it, whichever line arrived first.
	if len(result.Records) != 0 || result.Refused != 0 {
		t.Errorf("Read() = %+v, want no record and no refusal", result)
	}
}

func TestReadCountsARefusedNameWhoseResultArrivedFirst(t *testing.T) {
	transcript := strings.Join([]string{
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Task"}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(transcript), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	// A buffered result does not turn a fail-closed refusal into a silent success,
	// and the refusal is still counted exactly once.
	if len(result.Records) != 0 || result.Refused != 1 {
		t.Errorf("Read() = %+v, want the call refused and counted once", result)
	}
}

func TestReadDoesNotEmitAnEarlyResultForAnIDOutsideTheTokenDomain(t *testing.T) {
	unsafe := quoted(t, "call"+callSeparator+"1")
	transcript := strings.Join([]string{
		fmt.Sprintf(`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":%s,"is_error":false}]}}`, unsafe),
		fmt.Sprintf(`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":%s,"name":"Bash"}]}}`, unsafe),
	}, "\n")
	result, err := Read(strings.NewReader(transcript), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	// An id that would make a composed identity ambiguous is skipped, not refused,
	// and the early result may not smuggle it back in.
	if len(result.Records) != 0 || result.Refused != 0 || result.Pending != 0 {
		t.Errorf("Read() = %+v, want the call skipped with no record", result)
	}
}

// The buffering branch now retains a result that used to be discarded, so a second
// result for an already-completed call reaches it. One call is still one record
// (ADR-0002), and the first terminal result is the one that stands.
func TestReadIgnoresARepeatedResultForACompletedCall(t *testing.T) {
	transcript := strings.Join([]string{
		toolUseLine,
		toolResultLine,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":true}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(transcript), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Pending != 0 {
		t.Fatalf("Read() = %+v, want exactly one record for one call", result)
	}
	if event := result.Records[0]; event.Outcome == nil || *event.Outcome != record.OutcomeOK {
		t.Errorf("Outcome = %v, want the first terminal result to stand", event.Outcome)
	}
}

// Two results for one id can both precede the tool_use line. The forward-order
// path has always let the first terminal result stand and discarded the rest, so
// the buffered path must too — otherwise the same transcript would report a
// different outcome depending on which side of the tool_use line the duplicate
// landed on.
func TestReadKeepsTheFirstOfTwoEarlyResultsForOneCall(t *testing.T) {
	transcript := strings.Join([]string{
		toolResultLine,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":true}]}}`,
		toolUseLine,
	}, "\n")
	result, err := Read(strings.NewReader(transcript), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Pending != 0 {
		t.Fatalf("Read() = %+v, want exactly one record for one call", result)
	}
	event := result.Records[0]
	if event.Outcome == nil || *event.Outcome != record.OutcomeOK {
		t.Errorf("Outcome = %v, want the first early result to stand", event.Outcome)
	}
	if want := record.NormalizedTimestamp(parsedTime(t, "2026-08-13T12:00:01Z")); !event.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want the first early result's %v", event.Timestamp, want)
	}
}

// The adapter's hostile-payload assertion for the new retention path. ADR-0007
// requires one per adapter, not one shared corpus, because each input shape is a
// new way for a transcript string to reach a record.
func TestReadRetainsNothingFromAnEarlyResultLine(t *testing.T) {
	for _, value := range hostileValues {
		entryFields := fmt.Sprintf(`"toolDenialKind":%s,"pad":%s,"toolUseResult":{"interrupted":false},`,
			quoted(t, value), quoted(t, "swordfish-"+value))
		result, err := Read(strings.NewReader(reversedPairWith(entryFields, "")), resolver, names, Staleness{})
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if len(result.Records) != 1 {
			t.Fatalf("Read(toolDenialKind=%q) records = %+v, want exactly one", value, result.Records)
		}
		event := result.Records[0]
		// A hostile denial kind is neither permission-rule nor user-rejected, so the
		// source does not say — and unknown is never success (ADR-0005).
		if event.Outcome != nil {
			t.Errorf("Read(toolDenialKind=%q) outcome = %q, want unknown", value, *event.Outcome)
		}
		encoded, err := record.Marshal(event)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		for _, fragment := range []string{"swordfish", value} {
			if strings.Contains(string(encoded), fragment) {
				t.Fatalf("record retains %q from the early result line: %s", fragment, encoded)
			}
		}
	}
}

// reversedPairWith builds a reversed-order pair whose tool_result line carries
// entryFields at the top level (each ending in a comma) and blockFields inside the
// tool_result block (each starting with a comma), followed by the tool_use line
// that the result terminates.
func reversedPairWith(entryFields, blockFields string) string {
	result := fmt.Sprintf(
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z",%s"message":{"content":[{"type":"tool_result","tool_use_id":"call-1"%s}]}}`,
		entryFields, blockFields)
	return result + "\n" + toolUseLine
}

func parsedTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", value, err)
	}
	return parsed
}

// mixedShapeTranscript is one session shaped the way a real one is: three calls
// opened in a single assistant turn, terminated one at a time by results written
// in three different shapes, with an attribution-only bookkeeping entry in
// between. Every payload carries a fragment nothing may retain.
const mixedShapeTranscript = `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"model":"sonnet","content":[{"type":"tool_use","id":"call-1","name":"Bash"},{"type":"tool_use","id":"call-2","name":"Task","input":{"subagent_type":"code-reviewer"}},{"type":"tool_use","id":"call-3","name":"Skill","input":{"skill":"pr-review"}}]}}
{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","toolUseResult":"total 12\nfile.go\n","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}
{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","toolUseResult":[{"type":"text","text":"reviewed"}],"message":{"content":[{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}
{"uuid":"entry-4","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:03Z","message":{"model":"sonnet","stop_reason":"tool_use"}}
{"uuid":"entry-5","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:04Z","toolUseResult":{"stdout":"swordfish","interrupted":true},"message":{"content":[{"type":"tool_result","tool_use_id":"call-3"}]}}`

func TestReadTerminatesEveryCallInATranscriptShapedLikeARealOne(t *testing.T) {
	result, err := Read(strings.NewReader(mixedShapeTranscript), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 3 || result.Pending != 0 || result.Malformed != 0 || result.Refused != 0 {
		t.Fatalf("Read() = records %d, pending %d, malformed %d, refused %d; want one record per call and nothing stranded",
			len(result.Records), result.Pending, result.Malformed, result.Refused)
	}

	// Matched by name rather than by position: which shape terminated which call is
	// the point, and the assertion should not also depend on emission order.
	want := map[record.Identifier]record.Outcome{
		"Bash":          record.OutcomeOK,
		"code-reviewer": record.OutcomeOK,
		"pr-review":     record.OutcomeInterrupted,
	}
	got := map[record.Identifier]record.Outcome{}
	for _, event := range result.Records {
		if err := record.Validate(event); err != nil {
			t.Errorf("Validate(%q) error = %v", event.Name, err)
		}
		if event.Outcome == nil {
			t.Errorf("record %q has no outcome; its terminator carried a verdict", event.Name)
			continue
		}
		got[event.Name] = *event.Outcome
	}
	for name, outcome := range want {
		if got[name] != outcome {
			t.Errorf("record %q outcome = %q, want %q", name, got[name], outcome)
		}
	}
	if len(got) != len(want) {
		t.Errorf("records named %v, want %v", got, want)
	}

	// The reader reads a result payload; it never carries one. "interrupted" is
	// deliberately not a fragment here — it is the outcome enum's own value
	// (ADR-0005), written by the record type rather than copied from the payload.
	for _, event := range result.Records {
		encoded, marshalErr := record.Marshal(event)
		if marshalErr != nil {
			t.Fatalf("Marshal() error = %v", marshalErr)
		}
		for _, fragment := range []string{"total 12", "file.go", "reviewed", "swordfish", "stdout"} {
			if strings.Contains(string(encoded), fragment) {
				t.Fatalf("record %q retains %q from a tool result payload", event.Name, fragment)
			}
		}
	}
}

// consentedPath is the one working directory resolver consents to. It is named so a
// retention test can assert the path itself never reaches a record: only the hash
// does (plan §3.4).
const consentedPath = "/repo"

func resolver(cwd string) (record.Hash, bool) {
	return repo, cwd == consentedPath
}

// names keys the scope digest for this package's tests. Production keys it with a
// subkey of the per-machine salt (config.Repos.NameKey).
var names = record.NewNamer([]byte("test scope key"))

// quoted encodes value as a JSON string so a hostile value can be embedded in a
// transcript line without hand-escaping it.
func quoted(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal(%q) error = %v", value, err)
	}
	return string(encoded)
}

// skillCallTranscript is a terminated Skill call naming skill as its primitive.
func skillCallTranscript(t *testing.T, skill string) string {
	t.Helper()
	return strings.Join([]string{
		fmt.Sprintf(`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":%s}}]}}`, quoted(t, skill)),
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
}

// subagentCallTranscript is a terminated Task call naming subagentType as its
// primitive.
func subagentCallTranscript(t *testing.T, subagentType string) string {
	t.Helper()
	return strings.Join([]string{
		fmt.Sprintf(`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Task","input":{"subagent_type":%s}}]}}`, quoted(t, subagentType)),
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
}

func TestReadNamesASubagentCallByItsSubagentType(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Task","input":{"subagent_type":"explorer"}},{"type":"tool_use","id":"call-2","name":"Task","input":{"subagent_type":"code-reviewer"}},{"type":"tool_use","id":"call-3","name":"Task","input":{"subagent_type":"general-purpose"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false},{"type":"tool_result","tool_use_id":"call-2","is_error":false},{"type":"tool_result","tool_use_id":"call-3","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 3 || result.Malformed != 0 || result.Pending != 0 || result.Refused != 0 {
		t.Fatalf("Read() = %+v, want one record per subagent invocation", result)
	}
	seen := map[record.Identifier]bool{}
	for _, event := range result.Records {
		if event.Name == "Task" {
			t.Errorf("record is named after the tool, not the subagent: %+v", event)
		}
		if event.Kind != record.KindSubagent {
			t.Errorf("record kind = %q, want %q", event.Kind, record.KindSubagent)
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
	if len(seen) != 3 {
		t.Errorf("distinct names = %v, want exactly three", seen)
	}
}

func TestReadCollapsesRepeatedSubagentInvocationsToOnePrimitive(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Task","input":{"subagent_type":"explorer"}},{"type":"tool_use","id":"call-2","name":"Task","input":{"subagent_type":"explorer"}},{"type":"tool_use","id":"call-3","name":"Task","input":{"subagent_type":"code-reviewer"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false},{"type":"tool_result","tool_use_id":"call-2","is_error":false},{"type":"tool_result","tool_use_id":"call-3","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	// The invocation grain first: two calls of one subagent stay two records with
	// two distinct ids, so nothing is lost before aggregation collapses them.
	if len(result.Records) != 3 {
		t.Fatalf("Read() records = %+v, want one per invocation", result.Records)
	}
	var explorers []record.Record
	for _, event := range result.Records {
		if event.Name == "explorer" {
			explorers = append(explorers, event)
		}
	}
	if len(explorers) != 2 {
		t.Fatalf("explorer records = %d, want 2", len(explorers))
	}
	if explorers[0].EventID == explorers[1].EventID {
		t.Errorf("both explorer invocations derived the same event id %q", explorers[0].EventID)
	}

	// Then the primitive grain, through the real aggregation layer rather than by
	// inspection.
	summary := metrics.Aggregate(result.Records)
	if len(summary.Primitives) != 2 {
		t.Fatalf("Aggregate() primitives = %+v, want explorer and code-reviewer", summary.Primitives)
	}
	usage := map[record.Identifier]metrics.PrimitiveUsage{}
	for _, primitive := range summary.Primitives {
		usage[primitive.Name] = primitive
	}
	if got := usage["explorer"]; got.Invocations != 2 || got.Kind != record.KindSubagent {
		t.Errorf("explorer usage = %+v, want 2 invocations of a subagent", got)
	}
	if got := usage["code-reviewer"]; got.Invocations != 1 || got.Kind != record.KindSubagent {
		t.Errorf("code-reviewer usage = %+v, want 1 invocation of a subagent", got)
	}
}

// One subagent run is one invocation, even though Claude Code's own storage
// describes it twice: the parent's Task tool_use/tool_result pair, and the
// attributed end_turn entry the subagent's own turn leaves behind. Both describe
// the same logical event, so only one of them may become a record — the store's
// collapse guarantee, and ADR-0002's invocation grain ("one call to a primitive").
func TestReadCountsOneSubagentRunAsOneInvocation(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"model":"sonnet","content":[{"type":"tool_use","id":"call-1","name":"Task","input":{"subagent_type":"explorer"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","attributionAgent":"explorer","message":{"model":"sonnet","stop_reason":"end_turn"}}`,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v, want exactly one for one subagent run", result.Records)
	}
	// The surviving record is the paired one: it is the only source of the two that
	// carries an outcome at all (ADR-0005), and the only one bounded by a start and
	// an end rather than by a stop reason (ADR-0015).
	event := result.Records[0]
	if event.Kind != record.KindSubagent || event.Name != "explorer" || event.Outcome == nil || *event.Outcome != record.OutcomeOK {
		t.Fatalf("record = %+v, want the terminated explorer call with its outcome", event)
	}

	summary := metrics.Aggregate(result.Records)
	if len(summary.Primitives) != 1 || summary.Primitives[0].Invocations != 1 {
		t.Errorf("Aggregate() primitives = %+v, want explorer with one invocation", summary.Primitives)
	}
}

func TestReadDropsAndCountsASubagentCallWithNoType(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Task"},{"type":"tool_use","id":"call-2","name":"Task","input":{"subagent_type":""}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false},{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 {
		t.Errorf("Read() records = %+v, want none: a nameless Task call is not collected as \"Task\"", result.Records)
	}
	// Nothing was buffered, so the later tool_result synthesises nothing.
	if result.Pending != 0 {
		t.Errorf("Pending = %d, want 0", result.Pending)
	}
	// A refused name is not an unusable line, so doctor's drift signal is untouched.
	if result.Malformed != 0 {
		t.Errorf("Malformed = %d, want 0", result.Malformed)
	}
	if result.Refused != 2 {
		t.Errorf("Refused = %d, want 2", result.Refused)
	}
}

// Refused now drives doctor's "collects nothing", so it must count only calls Wake
// would otherwise have collected. Activity in a directory the user never consented
// to is outside collection altogether: counting it would report lost collection for
// a repository Wake was never asked to read, and would never clear.
func TestReadDoesNotCountARefusalInAnUnconsentedRepository(t *testing.T) {
	deny := func(string) (record.Hash, bool) { return "", false }
	for _, transcript := range []string{
		subagentCallTranscript(t, ""),
		subagentCallTranscript(t, "/usr/local/bin"),
	} {
		result, err := Read(strings.NewReader(transcript), deny, names, Staleness{})
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if len(result.Records) != 0 || result.Refused != 0 {
			t.Errorf("Read() = %+v, want no record and no refusal", result)
		}
	}
}

func TestReadDropsAndCountsSubagentCallsWithAHostileType(t *testing.T) {
	for _, value := range hostileValues {
		result, err := Read(strings.NewReader(subagentCallTranscript(t, value)), resolver, names, Staleness{})
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if len(result.Records) != 0 || result.Pending != 0 || result.Refused != 1 {
			t.Errorf("Read(subagent_type=%q) = %+v", value, result)
		}
	}
}

func TestReadDerivesADirectoryScopedSubagentReference(t *testing.T) {
	result, err := Read(strings.NewReader(subagentCallTranscript(t, "apps/web:reviewer")), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
	event := result.Records[0]
	if event.Kind != record.KindSubagent {
		t.Errorf("record kind = %q, want %q", event.Kind, record.KindSubagent)
	}
	name := string(event.Name)
	if !strings.HasPrefix(name, "scope-") || !strings.HasSuffix(name, ":reviewer") {
		t.Fatalf("record name = %q, want scope-<digest>:reviewer", name)
	}
	if strings.Contains(name, "apps") || strings.Contains(name, "web") {
		t.Fatalf("record name retains a path fragment: %q", name)
	}
	if err := record.Validate(event); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestReadDropsAScopedSubagentCallWithoutAScopeKey(t *testing.T) {
	result, err := Read(strings.NewReader(subagentCallTranscript(t, "apps/web:reviewer")), resolver, record.Namer{}, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 || result.Pending != 0 || result.Refused != 1 {
		t.Errorf("Read() = %+v, want the call dropped and counted, never named unkeyed", result)
	}
}

func TestReadDropsCallsWhosePrimitiveNameIsPathShaped(t *testing.T) {
	for _, value := range hostileValues {
		result, err := Read(strings.NewReader(skillCallTranscript(t, value)), resolver, names, Staleness{})
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if len(result.Records) != 0 || result.Pending != 0 {
			t.Errorf("Read(skill=%q) = %+v", value, result)
		}
	}
}

func TestReadDropsAttributedRunWithPathShapedAttribution(t *testing.T) {
	for _, field := range []string{"attributionSkill", "attributionAgent"} {
		for _, value := range hostileValues {
			input := fmt.Sprintf(`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z",%q:%s,"message":{"model":"sonnet","stop_reason":"end_turn"}}`, field, quoted(t, value))
			result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if len(result.Records) != 0 {
				t.Errorf("Read(%s=%q) records = %+v", field, value, result.Records)
			}
		}
	}
}

func TestReadOmitsUnsafeOptionalFieldsAndKeepsTheEvent(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"../1.0.0","attributionSkill":"/etc/passwd","attributionAgent":"../x","attributionMcpServer":"plugin:../evil:tool","message":{"model":"C:\\Windows","content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
	event := result.Records[0]
	if event.ViaSkill != "" || event.ViaAgent != "" || event.Package != "" || event.HarnessVersion != "" || event.Model != "" {
		t.Fatalf("record retained an unsafe optional field: %+v", event)
	}
	if err := record.Validate(event); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestReadDerivesADirectoryScopedSkillReference(t *testing.T) {
	result, err := Read(strings.NewReader(skillCallTranscript(t, "apps/web:deploy")), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
	event := result.Records[0]
	name := string(event.Name)
	if !strings.HasPrefix(name, "scope-") || !strings.HasSuffix(name, ":deploy") {
		t.Fatalf("record name = %q, want scope-<digest>:deploy", name)
	}
	if strings.Contains(name, "apps") || strings.Contains(name, "web") {
		t.Fatalf("record name retains a path fragment: %q", name)
	}
	if err := record.Validate(event); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestReadPreservesRealClaudeCodeIdentityFormats(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"2.1.3","attributionMcpServer":"plugin:atlassian:cloud","attributionSkill":"pr-review","attributionAgent":"sdlc-implement","message":{"model":"claude-sonnet-4-5-20250929","content":[{"type":"tool_use","id":"call-1","name":"mcp__atlassian__search"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
	event := result.Records[0]
	if event.Package != "atlassian" {
		t.Errorf("Package = %q", event.Package)
	}
	if event.ViaSkill != "pr-review" {
		t.Errorf("ViaSkill = %q", event.ViaSkill)
	}
	if event.ViaAgent != "sdlc-implement" {
		t.Errorf("ViaAgent = %q", event.ViaAgent)
	}
	if event.HarnessVersion != "2.1.3" {
		t.Errorf("HarnessVersion = %q", event.HarnessVersion)
	}
	if event.Model != "claude-sonnet-4-5-20250929" {
		t.Errorf("Model = %q", event.Model)
	}
	if event.Name != "mcp__atlassian__search" {
		t.Errorf("Name = %q", event.Name)
	}
}

// callInstant is when unterminatedCall's tool_use happened, as a value the
// staleness tests can compute a clock from. Every one of them injects its own Now
// rather than reading the wall clock: the threshold's real default errs deliberately
// long (24h) and must never be shortened to make a test easier to write.
var callInstant = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// closedSession is a staleness value under which every session in this file's
// fixtures — all stamped at or just after callInstant — has gone quiet past the
// threshold. ADR-0023 makes session close the terminal boundary for a Shape-A skill
// record, so a test that wants one emitted has to say the session ended; the zero
// Staleness deliberately emits nothing (see Staleness's own comment).
var closedSession = Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

// unterminatedCall is one tool_use with no tool_result anywhere after it — the
// transcript a session killed mid-call leaves behind.
const unterminatedCall = `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`

// resultForUnterminatedCall terminates unterminatedCall's call the ordinary way, so
// a test can compare the two terminal paths' derived identities.
const resultForUnterminatedCall = `{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`

func TestReadEmitsInterruptedForACallWhoseSessionWentStale(t *testing.T) {
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(2 * time.Hour)}

	result, err := Read(strings.NewReader(unterminatedCall), resolver, names, stale)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if len(result.Records) != 1 || result.Interrupted != 1 || result.Pending != 0 || result.OpenSessions != 0 {
		t.Fatalf("Read() = %+v", result)
	}
	event := result.Records[0]
	if event.Outcome == nil || *event.Outcome != record.OutcomeInterrupted {
		t.Fatalf("Outcome = %v, want interrupted", event.Outcome)
	}
	// The call's own tool_use timestamp, not the session's last activity and never
	// scan time: the invocation grain's timestamp is when the call happened, and a
	// timestamp that moved when unrelated later lines were appended would make the
	// same logical event serialise to different bytes on a later scan.
	if !event.Timestamp.Equal(callInstant) {
		t.Errorf("Timestamp = %v, want the tool_use timestamp %v", event.Timestamp, callInstant)
	}
	if err := record.Validate(event); err != nil {
		t.Errorf("Validate() error = %v", err)
	}
}

func TestReadKeepsACallBufferedInsideTheStalenessWindow(t *testing.T) {
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(30 * time.Minute)}

	result, err := Read(strings.NewReader(unterminatedCall), resolver, names, stale)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if len(result.Records) != 0 || result.Pending != 1 || result.Interrupted != 0 || result.OpenSessions != 1 {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadDoesNotInterruptACallInAStillActiveSession(t *testing.T) {
	// The call is three hours old, so a rule that read the call's own timestamp would
	// resolve it. The session wrote something one minute ago, so the rule that reads
	// the session's last activity keeps it buffered — which is the rule ADR-0015 means.
	input := strings.Join([]string{
		unterminatedCall,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T15:00:00Z","message":{"content":[{"type":"text"}]}}`,
	}, "\n")
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(3*time.Hour + time.Minute)}

	result, err := Read(strings.NewReader(input), resolver, names, stale)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if len(result.Records) != 0 || result.Pending != 1 || result.Interrupted != 0 || result.OpenSessions != 1 {
		t.Fatalf("Read() = %+v", result)
	}
}

// oversizedResult is resultForUnterminatedCall padded past maxLineBytes, so
// internal/jsonl discards it whole and the reader learns only that one line was
// unusable. A tool result carrying a megabyte of build output is ordinary rather
// than hostile (internal/jsonl's package comment), which is what makes this the
// common shape and not an edge case.
func oversizedResult(t *testing.T) string {
	t.Helper()
	line := `{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","padding":"` +
		strings.Repeat("p", maxLineBytes) +
		`","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`
	if len(line) <= maxLineBytes {
		t.Fatalf("line is %d bytes, want more than the reader's %d limit", len(line), maxLineBytes)
	}
	return line
}

func TestReadDoesNotInterruptACallWhenALineWasUnusable(t *testing.T) {
	// The unusable line is the tool_result that terminated the call successfully. The
	// reader cannot see that, so it must not conclude the opposite: emitting
	// interrupted here writes a permanent, uncorrectable failure for a call that
	// succeeded (ADR-0004 dedups, ADR-0015 rejects upsert), and inferring a terminal
	// outcome from a line it is blind to is exactly what plan §3.3 forbids.
	cases := map[string]string{
		"oversized": oversizedResult(t),
		"malformed": `{"uuid":"entry-2","sessionId":"session-1",`,
		// Decodes, and the reader can see a tool_result for the buffered call in it —
		// but the entry's own identity is unusable, so the result could not be attributed
		// and the call stayed buffered. That is blindness about this call's fate just as
		// much as a line that never arrived, however well the line parsed.
		"unusable identity carrying a tool_result": `{"sessionId":"session-1","cwd":"/repo","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	for name, unusable := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := Read(strings.NewReader(unterminatedCall+"\n"+unusable+"\n"), resolver, names, stale)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			// Malformed is the blindness doctor reports; Pending is the number that is
			// not final yet. Neither may become a record.
			if result.Malformed != 1 {
				t.Errorf("Malformed = %d, want 1", result.Malformed)
			}
			if len(result.Records) != 0 || result.Interrupted != 0 || result.Pending != 1 {
				t.Fatalf("Read() = %+v", result)
			}
		})
	}
}

func TestReadDoesNotInterruptACallTerminatedByAnyResultShape(t *testing.T) {
	// The commonest terminator on a real machine: the result block is there and
	// toolUseResult carries whatever shape the tool returned — a bare string for Bash,
	// an array of content blocks for Task. The reader reads those (see
	// transcriptEntry.ToolUseResult), so the call terminates normally and the staleness
	// rule has nothing left to resolve.
	//
	// It is pinned here, under a window wide open, because the failure it guards against
	// is silent and permanent: a shape that stopped terminating its call would leave the
	// call buffered, and this rule would then write it as outcome interrupted — a call
	// that in fact succeeded, uncorrectable because ADR-0004 deduplicates and ADR-0015
	// rejects upsert.
	shapes := map[string]string{
		"a bare string":              `"ok: 12 files"`,
		"an array of content blocks": `[{"type":"text","text":"done"}]`,
		"an object":                  `{"interrupted":false}`,
	}
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	for name, payload := range shapes {
		t.Run(name, func(t *testing.T) {
			terminator := `{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","toolUseResult":` +
				payload +
				`,"message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`

			result, err := Read(strings.NewReader(unterminatedCall+"\n"+terminator+"\n"), resolver, names, stale)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}

			if result.Malformed != 0 || result.Interrupted != 0 || result.Pending != 0 {
				t.Fatalf("Read() = %+v", result)
			}
			if len(result.Records) != 1 {
				t.Fatalf("Read() records = %+v, want exactly one", result.Records)
			}
			outcome := result.Records[0].Outcome
			if outcome == nil || *outcome != record.OutcomeOK {
				t.Errorf("Outcome = %v, want ok — the call finished, it was not interrupted", outcome)
			}
		})
	}
}

func TestReadKeepsEverySessionOpenWhenALineWasUnusable(t *testing.T) {
	// An unusable line is never observed as activity, so every session's last activity
	// is known-understated once the reader skipped one — including a session whose only
	// recent line is the one it could not read. Reporting such a session closed would
	// let a future cursor (ADR-0023 §5) advance past a session that is still alive, so
	// the whole read declines to judge: the floor stays at the start of the source.
	input := strings.Join([]string{unterminatedCall, oversizedResult(t)}, "\n")
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	result, err := Read(strings.NewReader(input), resolver, names, stale)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if result.OpenSessions != 1 {
		t.Errorf("OpenSessions = %d, want 1", result.OpenSessions)
	}
	if result.CursorFloor != 0 {
		t.Errorf("CursorFloor = %d, want 0", result.CursorFloor)
	}
}

func TestReadStillResolvesStaleCallsWhenEveryLineWasUsable(t *testing.T) {
	// The gate is "this read was blind somewhere", not "this transcript is long": a
	// transcript the reader read whole still resolves under the staleness rule, so the
	// fix does not disable ADR-0015 in the ordinary case.
	input := strings.Join([]string{
		unterminatedCall,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"text"}]}}`,
	}, "\n")
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	result, err := Read(strings.NewReader(input), resolver, names, stale)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if result.Malformed != 0 || result.Interrupted != 1 || result.Pending != 0 || result.OpenSessions != 0 {
		t.Fatalf("Read() = %+v", result)
	}
}

// bookkeepingLines are the per-session housekeeping shapes a real Claude Code
// transcript carries alongside its entries. They are not transcript entries: they
// decode cleanly and name their session, but they have no uuid and mostly no
// timestamp, so entry.valid() rejects them and they land in Malformed.
//
// They are routine rather than exotic — sampling ~/.claude/projects found all three
// present in every transcript examined, hundreds of lines against hundreds of
// entries. That is what makes them the load-bearing case here: a staleness gate
// keyed on "this read rejected a line" is a gate that is always closed on real
// input, so ADR-0015's rule would never run outside a hand-written fixture.
var bookkeepingLines = map[string]string{
	"ai-title":        `{"type":"ai-title","aiTitle":"reader staleness","sessionId":"session-1"}`,
	"last-prompt":     `{"type":"last-prompt","lastPrompt":"run the tests","leafUuid":"entry-1","sessionId":"session-1"}`,
	"queue-operation": `{"type":"queue-operation","operation":"enqueue","sessionId":"session-1","timestamp":"2026-08-13T12:00:00Z"}`,
}

func TestReadStillResolvesAStaleCallOnATranscriptCarryingBookkeepingLines(t *testing.T) {
	// A line the reader decoded is a line it can rule out: it holds no tool_result, so
	// it cannot be the terminator of a buffered call and nothing about this session's
	// fate is hidden by it. The rule therefore still applies, on each shape alone and
	// on all of them together — which is the shape of every real transcript.
	inputs := map[string]string{}
	all := []string{unterminatedCall}
	for name, line := range bookkeepingLines {
		inputs[name] = unterminatedCall + "\n" + line
		all = append(all, line)
	}
	slices.Sort(all[1:])
	inputs["every shape at once"] = strings.Join(all, "\n")
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			result, err := Read(strings.NewReader(input), resolver, names, stale)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if result.Interrupted != 1 || result.Pending != 0 || result.OpenSessions != 0 || len(result.Records) != 1 {
				t.Fatalf("Read() = %+v", result)
			}
			event := result.Records[0]
			if event.Outcome == nil || *event.Outcome != record.OutcomeInterrupted {
				t.Fatalf("Outcome = %v, want interrupted", event.Outcome)
			}
			if err := record.Validate(event); err != nil {
				t.Errorf("Validate() error = %v", err)
			}
			// These lines are still counted as lines this read had no entry for, which is
			// what doctor's drift signal reports today. Splitting that counter so doctor
			// stops calling a healthy machine "collects nothing" is its own change; what
			// this pins is that the count does not decide whether the rule runs.
			if want := strings.Count(input, "\n"); result.Malformed != want {
				t.Errorf("Malformed = %d, want %d", result.Malformed, want)
			}
		})
	}
}

// partlyDecodedLines are real transcript shapes that do not fit this reader's struct
// in exactly one field. encoding/json reports the mismatch and keeps decoding the
// rest, so the line is still inspectable — and neither of these carries a tool_result
// block, so neither can be the terminator of a buffered call.
//
// They are the common case, not an edge: sampling ~/.claude/projects, message.content
// arrives as a plain string on thousands of user turns, touching almost every
// transcript. A gate keyed on "json.Unmarshal returned an error" would therefore be as
// permanently closed as one keyed on Malformed was.
//
// A non-object toolUseResult used to belong here and no longer does: the field is read
// raw, so such a line decodes whole (see transcriptEntry.ToolUseResult). It is covered
// as an ordinary readable line instead — by
// TestReadDoesNotInterruptACallTerminatedByAnyResultShape when it terminates a call,
// and by the case below when it does not.
var partlyDecodedLines = map[string]string{
	"a user turn whose content is text rather than blocks": `{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","type":"user","message":{"role":"user","content":"carry on"}}`,
}

func TestReadStillResolvesAStaleCallWhenALineOnlyPartlyDecoded(t *testing.T) {
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	for name, line := range partlyDecodedLines {
		t.Run(name, func(t *testing.T) {
			result, err := Read(strings.NewReader(unterminatedCall+"\n"+line), resolver, names, stale)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if result.Interrupted != 1 || result.Pending != 0 || result.OpenSessions != 0 || len(result.Records) != 1 {
				t.Fatalf("Read() = %+v", result)
			}
			if result.Malformed != 1 {
				t.Errorf("Malformed = %d, want 1 — the line still yielded no entry", result.Malformed)
			}
		})
	}
}

func TestReadStillResolvesAStaleCallPastANonObjectToolUseResult(t *testing.T) {
	// An entry whose toolUseResult is an array and which carries no tool_result block:
	// fully readable, so it is not blindness, and not a terminator, so it resolves
	// nothing. The rule must still run, and the line must not be counted as a line the
	// reader had no entry for.
	line := `{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","toolUseResult":["one","two"],"message":{"content":[{"type":"text"}]}}`
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	result, err := Read(strings.NewReader(unterminatedCall+"\n"+line), resolver, names, stale)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if result.Malformed != 0 || result.Interrupted != 1 || result.Pending != 0 || result.OpenSessions != 0 {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadTreatsALineWithNoEntryAsActivityForItsSession(t *testing.T) {
	// The reader has no entry for either line, but both name their session and carry a
	// timestamp — which is the whole of what the staleness rule asks. Reading the
	// session's last activity from the tool_use instead would call a session that wrote
	// something half an hour ago dead, and ADR-0004's dedup makes that permanent.
	//
	// This is what the narrowed gate makes load-bearing: these lines no longer stop the
	// rule, so the rule has to take account of them rather than not see them.
	lines := map[string]string{
		"bookkeeping line":  `{"type":"queue-operation","operation":"enqueue","sessionId":"session-1","timestamp":"2026-08-13T19:30:00Z"}`,
		"partly decoded":    `{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T19:30:00Z","type":"user","message":{"role":"user","content":"carry on"}}`,
		"unusable entry id": `{"uuid":"not/a/token","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T19:30:00Z","message":{"content":[{"type":"text"}]}}`,
	}
	// 20:00 against a 19:30 line: half an hour inside a one-hour window.
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	for name, line := range lines {
		t.Run(name, func(t *testing.T) {
			result, err := Read(strings.NewReader(unterminatedCall+"\n"+line), resolver, names, stale)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if result.Interrupted != 0 || result.Pending != 1 || result.OpenSessions != 1 {
				t.Fatalf("Read() = %+v", result)
			}
			// The floor is the session's earliest line, which is still the tool_use at the
			// start of the source.
			if result.CursorFloor != 0 {
				t.Errorf("CursorFloor = %d, want 0", result.CursorFloor)
			}
		})
	}
}

func TestReadDerivesTheSameEventIDForAStaleCallAsForACompletedOne(t *testing.T) {
	// One buffered call becomes at most one record whichever way it terminates: the id
	// comes from the source event, so no suffix and no second id namespace separates
	// the interrupted record from the completed one (ADR-0004). That is what makes a
	// rescan, a retry and two concurrent scans all a no-op at the store.
	stale, err := Read(strings.NewReader(unterminatedCall), resolver, names, Staleness{Timeout: time.Hour, Now: callInstant.Add(2 * time.Hour)})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	completed, err := Read(strings.NewReader(unterminatedCall+"\n"+resultForUnterminatedCall), resolver, names, Staleness{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(stale.Records) != 1 || len(completed.Records) != 1 {
		t.Fatalf("records = %d stale, %d completed; want 1 each", len(stale.Records), len(completed.Records))
	}
	if stale.Records[0].EventID != completed.Records[0].EventID {
		t.Fatalf("EventID = %q interrupted, %q completed; want identical", stale.Records[0].EventID, completed.Records[0].EventID)
	}

	// And a different clock does not change it either: the id is derived from the
	// source event, never from when the scan happened.
	later, err := Read(strings.NewReader(unterminatedCall), resolver, names, Staleness{Timeout: time.Minute, Now: callInstant.Add(400 * time.Hour)})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(later.Records) != 1 || later.Records[0].EventID != stale.Records[0].EventID {
		t.Fatalf("EventID under a different clock = %+v, want %q", later.Records, stale.Records[0].EventID)
	}
}

func TestReadEmitsStaleCallsInADeterministicOrder(t *testing.T) {
	// The buffer is a map and Go randomises map iteration, while internal/store must
	// produce byte-identical contents for two scans of one source. So the resolved
	// calls are sorted by (tool_use timestamp, block id), and twenty reads have to agree.
	input := strings.Join([]string{
		unterminatedCall,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_use","id":"call-2","name":"Read"}]}}`,
	}, "\n")
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(2 * time.Hour)}

	var want []record.Hash
	for attempt := range 20 {
		result, err := Read(strings.NewReader(input), resolver, names, stale)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if result.Interrupted != 2 || len(result.Records) != 2 {
			t.Fatalf("Read() = %+v", result)
		}
		got := []record.Hash{result.Records[0].EventID, result.Records[1].EventID}
		if attempt == 0 {
			want = got
			continue
		}
		if got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("attempt %d order = %v, want %v", attempt, got, want)
		}
	}
}

func TestReadReportsTheCursorFloorOfTheEarliestOpenSession(t *testing.T) {
	// A future incremental cursor must not advance past the earliest line of a session
	// that has not closed yet (ADR-0023 §5), so the floor is the open session's first
	// line — not the closed session's, which sits before it in the file.
	closedSession := `{"uuid":"entry-1","sessionId":"session-old","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`
	openFirst := `{"uuid":"entry-2","sessionId":"session-live","cwd":"/repo","timestamp":"2026-08-13T20:00:00Z","message":{"content":[{"type":"tool_use","id":"call-2","name":"Read"}]}}`
	openLater := `{"uuid":"entry-3","sessionId":"session-live","cwd":"/repo","timestamp":"2026-08-13T20:00:30Z","message":{"content":[{"type":"text"}]}}`
	input := strings.Join([]string{closedSession, openFirst, openLater}, "\n")
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8*time.Hour + 30*time.Minute)}

	result, err := Read(strings.NewReader(input), resolver, names, stale)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if result.OpenSessions != 1 {
		t.Errorf("OpenSessions = %d, want 1", result.OpenSessions)
	}
	if want := int64(len(closedSession) + 1); result.CursorFloor != want {
		t.Errorf("CursorFloor = %d, want %d", result.CursorFloor, want)
	}
	// The closed session's call resolved; the open session's stayed buffered.
	if result.Interrupted != 1 || result.Pending != 1 {
		t.Errorf("Read() = %+v", result)
	}
}

func TestReadReleasesTheCursorFloorWhenEverySessionClosed(t *testing.T) {
	// Nothing is left unresolved, so a cursor may advance freely and the floor means
	// nothing: OpenSessions at zero is what says so, and CursorFloor is only meaningful
	// while it is positive (see Result.CursorFloor).
	second := `{"uuid":"entry-2","sessionId":"session-2","cwd":"/repo","timestamp":"2026-08-13T12:00:05Z","message":{"content":[{"type":"tool_use","id":"call-2","name":"Read"}]}}`
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	result, err := Read(strings.NewReader(unterminatedCall+"\n"+second), resolver, names, stale)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if result.OpenSessions != 0 || result.CursorFloor != 0 {
		t.Errorf("Read() = %+v, want no open session and no floor", result)
	}
	if result.Interrupted != 2 || result.Pending != 0 {
		t.Errorf("Read() = %+v, want both calls resolved", result)
	}
}

func TestReadPinsTheCursorFloorForAnOpenSessionWithNothingBuffered(t *testing.T) {
	// ADR-0023 §5 pins the floor at the earliest line of any session that has not
	// closed, not at the earliest line holding a buffered call. A session still open may
	// yet write the tool_use whose result arrives later in the same file, so advancing
	// past its first line loses the pairing — while a floor that is too low only costs a
	// re-read, which writes nothing twice (ADR-0004).
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(30 * time.Minute)}

	result, err := Read(strings.NewReader(unterminatedCall+"\n"+resultForUnterminatedCall), resolver, names, stale)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if result.Pending != 0 || result.Interrupted != 0 || len(result.Records) != 1 {
		t.Fatalf("Read() = %+v, want the call terminated the ordinary way", result)
	}
	if result.OpenSessions != 1 || result.CursorFloor != 0 {
		t.Errorf("Read() = %+v, want the open session pinned at its first line", result)
	}
}

func TestReadDoesNotInterruptACallItNeverCollected(t *testing.T) {
	// The staleness rule may only resolve what the reader actually buffered. A call
	// outside collection — an unconsented repository, a session id outside the token
	// domain — was never Wake's to collect, and a call whose name was refused is a
	// fail-closed drop (ADR-0007). None of them may become a record because a clock
	// moved: that would widen consent, or substitute a placeholder name, permanently
	// (ADR-0004 deduplicates, ADR-0015 rejects upsert).
	cases := map[string]struct {
		transcript  string
		wantRefused int
	}{
		"an unconsented repository": {
			transcript: `{"uuid":"entry-1","sessionId":"session-1","cwd":"/elsewhere","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		},
		"a session id outside the token domain": {
			transcript: `{"uuid":"entry-1","sessionId":"` + strings.Repeat("s", 1024) + `","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		},
		"a subagent call naming no subagent": {
			transcript:  `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Task"}]}}`,
			wantRefused: 1,
		},
	}
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := Read(strings.NewReader(test.transcript), resolver, names, stale)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}

			if len(result.Records) != 0 || result.Interrupted != 0 || result.Pending != 0 {
				t.Fatalf("Read() = %+v, want nothing emitted and nothing buffered", result)
			}
			if result.Refused != test.wantRefused {
				t.Errorf("Refused = %d, want %d", result.Refused, test.wantRefused)
			}
		})
	}
}

// The adapter's hostile-payload assertion for the interrupted path. ADR-0007 requires
// one per adapter and per input shape, and this is a new way for a record to be built:
// no result line contributes to it, so every field comes out of the retained call.
func TestReadRetainsNothingFromAnInterruptedCall(t *testing.T) {
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}

	for _, value := range hostileValues {
		// The hostile value goes only in fields no record field is derived from: a denial
		// kind, an unmodelled sibling key, and the working directory — which the resolver
		// turns into a hash and which may never travel as a path (plan §3.4). The
		// allowlisted fields carry their own bounded values, and what they admit is bounded
		// by record's own domains rather than by this test.
		transcript := fmt.Sprintf(
			`{"uuid":"entry-1","sessionId":"session-1","cwd":%s,"timestamp":"2026-08-13T12:00:00Z","toolDenialKind":%s,"pad":%s,"message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
			quoted(t, consentedPath), quoted(t, value), quoted(t, "swordfish-"+value))

		result, err := Read(strings.NewReader(transcript), resolver, names, stale)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if result.Interrupted != 1 || len(result.Records) != 1 {
			t.Fatalf("Read(%q) = %+v, want one interrupted record", value, result)
		}
		event := result.Records[0]
		// The outcome is the rule's, not the transcript's: a hostile toolDenialKind is
		// neither permission-rule nor user-rejected, and nothing about a session going
		// quiet is negotiable from the line.
		if event.Outcome == nil || *event.Outcome != record.OutcomeInterrupted {
			t.Errorf("Read(%q) outcome = %v, want interrupted", value, event.Outcome)
		}
		if validateErr := record.Validate(event); validateErr != nil {
			t.Errorf("Read(%q) Validate() error = %v", value, validateErr)
		}
		encoded, err := record.Marshal(event)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		for _, fragment := range []string{"swordfish", consentedPath, value} {
			if strings.Contains(string(encoded), fragment) {
				t.Fatalf("interrupted record retains %q: %s", fragment, encoded)
			}
		}
	}
}

func TestMarshalledRecordsCarryNoSeparator(t *testing.T) {
	inputs := []string{skillCallTranscript(t, "apps/web:deploy"), subagentCallTranscript(t, "apps/web:reviewer")}
	for _, value := range hostileValues {
		inputs = append(inputs, skillCallTranscript(t, value), subagentCallTranscript(t, value))
	}
	inputs = append(inputs, strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"../1.0.0","attributionSkill":"/etc/passwd","attributionAgent":"../secrets","attributionMcpServer":"plugin:../evil:tool","message":{"model":"~/.ssh/id_rsa","content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n"))

	for _, input := range inputs {
		result, err := Read(strings.NewReader(input), resolver, names, Staleness{})
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		for _, event := range result.Records {
			encoded, err := record.Marshal(event)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			for _, fragment := range []string{"/", `\`, "etc", "Windows", "secrets", ".ssh"} {
				if strings.Contains(string(encoded), fragment) {
					t.Fatalf("record contains %q: %s", fragment, encoded)
				}
			}
		}
	}
}
