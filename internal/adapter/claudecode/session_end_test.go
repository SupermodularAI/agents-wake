package claudecode

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// sessionIdleTimeout is this file's stand-in for config's session.idle_timeout
// default. Nothing here depends on the number: what the tests exercise is which
// side of it a session's silence falls on.
const sessionIdleTimeout = 30 * time.Minute

// finished and live are the two answers the idleness predicate can give for a
// session whose last activity is callInstant. finished is well past the threshold;
// live is well inside it.
var (
	finished = Idleness{Timeout: sessionIdleTimeout, Now: callInstant.Add(2 * time.Hour)}
	live     = Idleness{Timeout: sessionIdleTimeout, Now: callInstant.Add(10 * time.Minute)}
)

// realUsage is the usage block a real Claude Code transcript carries, kept
// verbatim including every sibling key this reader does not model. Using the
// observed shape rather than a trimmed one is the point: the unmodelled siblings
// have to reach nothing, and a fixture that omits them cannot show that.
const realUsage = `{"input_tokens":6,"cache_creation_input_tokens":5273,"cache_read_input_tokens":27455,` +
	`"output_tokens":361,"output_tokens_details":{"thinking_tokens":136},` +
	`"cache_creation":{"ephemeral_5m_input_tokens":5273,"ephemeral_1h_input_tokens":0},` +
	`"iterations":1,"server_tool_use":{"web_search_requests":0},"service_tier":"standard",` +
	`"inference_geo":"us","speed":"fast"}`

// assistantLine builds one assistant transcript line carrying a message id and a
// raw usage block. usage is spliced in as JSON, so a caller passes "null", an
// object, or any other shape it wants the reader to face.
func assistantLine(uuid, session, at, messageID, usage string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":%q,"version":"1.0.0","entrypoint":"cli",`+
			`"message":{"model":"sonnet","id":%q,"usage":%s,"content":[]}}`,
		uuid, session, at, messageID, usage)
}

// toolCallLines is a terminated tool call: the tool_use and the tool_result that
// ends it, so the invocation record exists without the staleness rule running.
func toolCallLines(uuid, session, at, callID, tool, inputJSON string) string {
	return strings.Join([]string{
		fmt.Sprintf(
			`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":%q,"version":"1.0.0","entrypoint":"cli",`+
				`"message":{"model":"sonnet","id":%q,"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`,
			uuid, session, at, uuid+"-msg", callID, tool, inputJSON),
		fmt.Sprintf(
			`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":%q,"entrypoint":"cli",`+
				`"message":{"content":[{"type":"tool_result","tool_use_id":%q,"is_error":false}]}}`,
			uuid+"-result", session, at, callID),
	}, "\n")
}

// sessionEnds filters a read's records down to the session grain, so a test can
// assert about it without counting the invocation records beside it.
func sessionEnds(records []record.Record) []record.Record {
	ends := make([]record.Record, 0, len(records))
	for _, event := range records {
		if event.Kind == record.KindSessionEnd {
			ends = append(ends, event)
		}
	}
	return ends
}

// onlySessionEnd fails unless the read produced exactly one session-grain record,
// and returns it. Most tests below want that record and nothing else about shape.
func onlySessionEnd(t *testing.T, result Result) record.Record {
	t.Helper()
	ends := sessionEnds(result.Records)
	if len(ends) != 1 {
		t.Fatalf("session_end records = %d, want 1 (result = %+v)", len(ends), result)
	}
	return ends[0]
}

func TestReadDerivesOneSessionEndForAFinishedSession(t *testing.T) {
	input := assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", realUsage)

	result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, Staleness{}, finished)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	event := onlySessionEnd(t, result)

	if event.Kind != record.KindSessionEnd {
		t.Errorf("Kind = %q, want %q", event.Kind, record.KindSessionEnd)
	}
	// Wake's own constant for the row, not a value from the transcript: Validate
	// requires a non-empty name and a session has no primitive name.
	if event.Name != "session" {
		t.Errorf("Name = %q, want %q", event.Name, "session")
	}
	// A session_end is a lifecycle event inferred from a threshold — chosen by
	// nobody, which is what plan §2.3 defines auto as.
	if event.Invoker != record.InvokerAuto {
		t.Errorf("Invoker = %q, want %q", event.Invoker, record.InvokerAuto)
	}
	if event.Harness != harness || event.SessionID != "session-1" || event.Repo != repo {
		t.Errorf("identity = %q/%q/%q", event.Harness, event.SessionID, event.Repo)
	}
	if event.Entrypoint != record.EntrypointCLI || event.HarnessVersion != "1.0.0" {
		t.Errorf("entrypoint/version = %q/%q", event.Entrypoint, event.HarnessVersion)
	}
	// A session reports no outcome, and nil is never a synthesised ok (ADR-0005).
	if event.Outcome != nil {
		t.Errorf("Outcome = %v, want nil", event.Outcome)
	}
	// Deliberately absent: a session may use several models, so carrying one entry's
	// model would assert a fact about the session no source states. A duration is
	// impossible to state correctly when the timestamp is fixed at last activity.
	if event.Model != "" {
		t.Errorf("Model = %q, want empty", event.Model)
	}
	if event.DurationMS != nil {
		t.Errorf("DurationMS = %v, want nil", event.DurationMS)
	}
	if err := record.Validate(event); err != nil {
		t.Errorf("Validate() error = %v", err)
	}
}

func TestReadDerivesNoSessionEndForAnOpenSession(t *testing.T) {
	input := assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", realUsage)
	cases := map[string]Idleness{
		"well inside the window": live,
		// Strictly greater than the threshold, matching closed()'s rule: a session
		// silent for exactly the threshold is still open, which errs toward not writing
		// a record that cannot be taken back.
		"exactly at the threshold": {Timeout: sessionIdleTimeout, Now: callInstant.Add(sessionIdleTimeout)},
	}

	for name, idle := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, Staleness{}, idle)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if ends := sessionEnds(result.Records); len(ends) != 0 {
				t.Fatalf("session_end records = %d, want 0", len(ends))
			}
		})
	}
}

// TestReadDerivesNoSessionEndWithoutAThreshold pins the safe default. A caller
// that cannot read session.idle_timeout must derive nothing: a session_end written
// on a guessed threshold is permanent, because ADR-0015 rejects upsert and
// ADR-0004 deduplicates the correction away.
func TestReadDerivesNoSessionEndWithoutAThreshold(t *testing.T) {
	input := assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", realUsage)
	cases := map[string]Idleness{
		"zero value":     {},
		"no clock":       {Timeout: sessionIdleTimeout},
		"no threshold":   {Now: callInstant.Add(2 * time.Hour)},
		"zero threshold": {Timeout: 0, Now: callInstant.Add(2 * time.Hour)},
	}

	for name, idle := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, Staleness{}, idle)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if ends := sessionEnds(result.Records); len(ends) != 0 {
				t.Fatalf("session_end records = %d, want 0", len(ends))
			}
		})
	}
}

// TestReadDerivesASessionEndForASessionWithNoInvocations is the plan §2.7
// baseline: a session that invoked nothing is exactly the row that makes every
// rate above it meaningful, so it has to be observable rather than absent.
func TestReadDerivesASessionEndForASessionWithNoInvocations(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","entrypoint":"cli","message":{"content":[]}}`,
		assistantLine("entry-2", "session-1", "2026-08-13T12:00:05Z", "msg_1", realUsage),
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, Staleness{}, finished)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Records = %d, want only the session_end", len(result.Records))
	}
	event := onlySessionEnd(t, result)
	// Present at zero rather than absent: this scan counted them, and a measured
	// zero is a value. An absent count would read as "unreported" at the receiver.
	assertCount(t, "ToolCalls", event.ToolCalls, 0)
	assertCount(t, "BuiltinToolCalls", event.BuiltinToolCalls, 0)
}

func TestSessionEndTimestampIsLastConsentedActivity(t *testing.T) {
	// Deliberately out of order: nothing in the transcript format promises the
	// entries are sorted, and the timestamp is a fold over values the source
	// supplies rather than the last line read.
	input := strings.Join([]string{
		assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", "null"),
		assistantLine("entry-2", "session-1", "2026-08-13T12:05:00Z", "msg_2", "null"),
		assistantLine("entry-3", "session-1", "2026-08-13T12:02:00Z", "msg_3", "null"),
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, Staleness{}, finished)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	event := onlySessionEnd(t, result)

	want := callInstant.Add(5 * time.Minute)
	if !event.Timestamp.Equal(want) {
		t.Fatalf("Timestamp = %v, want %v", event.Timestamp, want)
	}
	// Never the scan's clock: two scans of one transcript have to compute this
	// identically (ADR-0004), and finished.Now is two hours later than any entry.
	if event.Timestamp.Equal(finished.Now) {
		t.Fatal("Timestamp is the scan clock, not last activity")
	}
}

func TestSessionEndIDIsStableAcrossReads(t *testing.T) {
	input := strings.Join([]string{
		assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", realUsage),
		toolCallLines("entry-2", "session-1", "2026-08-13T12:00:02Z", "call-1", "Bash", `{}`),
	}, "\n")

	first := onlySessionEnd(t, mustRead(t, input, finished))
	second := onlySessionEnd(t, mustRead(t, input, finished))

	if first.EventID != second.EventID {
		t.Fatalf("EventID = %q then %q", first.EventID, second.EventID)
	}
	firstBytes, err := record.Marshal(first)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	secondBytes, err := record.Marshal(second)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("two reads of one transcript disagree:\n%s\n%s", firstBytes, secondBytes)
	}
}

// TestSessionEndIDIsIndependentOfTotalsAndTimestamp is the property the store's
// dedup turns into "one session_end per session id, ever". The id is derived from
// (harness, session id, kind) and from nothing that a later scan can see more of,
// so a re-derivation with different totals is a duplicate rather than a correction.
func TestSessionEndIDIsIndependentOfTotalsAndTimestamp(t *testing.T) {
	quiet := assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", realUsage)
	resumed := strings.Join([]string{
		quiet,
		assistantLine("entry-2", "session-1", "2026-08-13T12:05:00Z", "msg_2", realUsage),
	}, "\n")

	before := onlySessionEnd(t, mustRead(t, quiet, finished))
	after := onlySessionEnd(t, mustRead(t, resumed, finished))

	if before.EventID != after.EventID {
		t.Fatalf("EventID changed with the totals: %q then %q", before.EventID, after.EventID)
	}
	if before.Timestamp.Equal(after.Timestamp) {
		t.Fatal("the fixture did not move the timestamp; the test proves nothing")
	}
	if *before.InputTokens == *after.InputTokens {
		t.Fatal("the fixture did not move the totals; the test proves nothing")
	}
}

// TestSessionEndIDDoesNotCollideWithACallOrATerminalRun exercises the three id
// shapes at once on a transcript built to make them collide: the entry uuid is
// the session id, and the tool_use block id is literally "session_end". The
// separators keep them disjoint — 0x1f for a call, 0x1e for a session, and a bare
// uuid for a Shape-A fallback.
func TestSessionEndIDDoesNotCollideWithACallOrATerminalRun(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"session-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","entrypoint":"cli","message":{"content":[{"type":"tool_use","id":"session_end","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","entrypoint":"cli","message":{"content":[{"type":"tool_result","tool_use_id":"session_end","is_error":false}]}}`,
		`{"uuid":"session-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","entrypoint":"cli","attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, closedSession, finished)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 3 {
		t.Fatalf("Records = %d, want a call, a fallback and a session_end (%+v)", len(result.Records), result)
	}
	ids := map[record.Hash]record.Kind{}
	for _, event := range result.Records {
		if previous, clash := ids[event.EventID]; clash {
			t.Fatalf("event id collision between %q and %q", previous, event.Kind)
		}
		ids[event.EventID] = event.Kind
	}
}

func TestReadCountsToolCallsAndBuiltinToolCallsForASession(t *testing.T) {
	// Two sessions in one file, so the counts are proved per session rather than
	// per transcript.
	input := strings.Join([]string{
		toolCallLines("a1", "session-a", "2026-08-13T12:00:00Z", "call-1", "Bash", `{}`),
		toolCallLines("a2", "session-a", "2026-08-13T12:00:02Z", "call-2", "Skill", `{"skill":"pr-review"}`),
		toolCallLines("a3", "session-a", "2026-08-13T12:00:03Z", "call-3", "mcp__atlassian__search", `{}`),
		toolCallLines("b1", "session-b", "2026-08-13T12:00:04Z", "call-4", "Bash", `{}`),
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, Staleness{}, finished)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	ends := sessionEnds(result.Records)
	if len(ends) != 2 {
		t.Fatalf("session_end records = %d, want 2 (%+v)", len(ends), result)
	}
	want := map[record.Identifier][2]int64{
		"session-a": {3, 1},
		"session-b": {1, 1},
	}
	for _, event := range ends {
		counts, known := want[event.SessionID]
		if !known {
			t.Fatalf("unexpected session %q", event.SessionID)
		}
		assertCount(t, string(event.SessionID)+" ToolCalls", event.ToolCalls, counts[0])
		assertCount(t, string(event.SessionID)+" BuiltinToolCalls", event.BuiltinToolCalls, counts[1])
	}
}

// TestReadCountsAShapeAFallbackAsAToolCall keeps total minus builtin equal to the
// number of spans the session delivers. The count exists so a receiver can recover
// the denominator encodeSpan's built-in-tool omission destroys, and a fallback
// record is a span like any other.
func TestReadCountsAShapeAFallbackAsAToolCall(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","entrypoint":"cli","attributionSkill":"pr-review","message":{"model":"sonnet","stop_reason":"end_turn"}}`

	result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, closedSession, finished)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	event := onlySessionEnd(t, result)
	assertCount(t, "ToolCalls", event.ToolCalls, 1)
	assertCount(t, "BuiltinToolCalls", event.BuiltinToolCalls, 0)
}

// TestReadExcludesAPendingCallFromTheAggregate is ADR-0034 §3's snapshot: the
// aggregate sums what this scan made terminal, and a call still buffered because
// scan.stale_call_timeout has not elapsed is simply not in it. There is no
// waiting, no backfill and no reconciliation pass.
func TestReadExcludesAPendingCallFromTheAggregate(t *testing.T) {
	result, err := Read(strings.NewReader(unterminatedCall), resolver, names, installedPrimitives, Staleness{}, finished)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if result.Pending != 1 {
		t.Fatalf("Pending = %d, want 1", result.Pending)
	}
	event := onlySessionEnd(t, result)
	assertCount(t, "ToolCalls", event.ToolCalls, 0)
	assertCount(t, "BuiltinToolCalls", event.BuiltinToolCalls, 0)
}

// TestReadSumsSessionTokensOnce is the measured shape: Claude Code writes one
// assistant API message across several transcript lines, each repeating the same
// usage block verbatim. Summing per line multiplies a session's totals, so the
// dedup on message id is the whole reason the field is read.
func TestReadSumsSessionTokensOnce(t *testing.T) {
	lines := make([]string, 0, 5)
	for index := range 5 {
		lines = append(lines, assistantLine(
			fmt.Sprintf("entry-%d", index), "session-1",
			fmt.Sprintf("2026-08-13T12:00:0%dZ", index), "msg_1", realUsage))
	}

	event := onlySessionEnd(t, mustRead(t, strings.Join(lines, "\n"), finished))

	assertCount(t, "InputTokens", event.InputTokens, 6)
	assertCount(t, "OutputTokens", event.OutputTokens, 361)
	assertCount(t, "CacheReadTokens", event.CacheReadTokens, 27455)
	assertCount(t, "CacheCreationTokens", event.CacheCreationTokens, 5273)
	assertCount(t, "ThinkingTokens", event.ThinkingTokens, 136)
}

func TestReadSumsTokensAcrossMessages(t *testing.T) {
	input := strings.Join([]string{
		assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", realUsage),
		assistantLine("entry-2", "session-1", "2026-08-13T12:00:01Z", "msg_1", realUsage),
		assistantLine("entry-3", "session-1", "2026-08-13T12:00:02Z", "msg_2", realUsage),
	}, "\n")

	event := onlySessionEnd(t, mustRead(t, input, finished))

	assertCount(t, "InputTokens", event.InputTokens, 12)
	assertCount(t, "OutputTokens", event.OutputTokens, 722)
	assertCount(t, "CacheReadTokens", event.CacheReadTokens, 54910)
	assertCount(t, "CacheCreationTokens", event.CacheCreationTokens, 10546)
	assertCount(t, "ThinkingTokens", event.ThinkingTokens, 272)
}

// TestReadLeavesAnUnreportedTokenCountAbsent keeps "the harness said nothing"
// distinguishable from "the harness said none". A block that is not a JSON object
// carries no structured usage and none is inferred from it (plan §3.3).
func TestReadLeavesAnUnreportedTokenCountAbsent(t *testing.T) {
	cases := map[string]string{
		"absent":       "",
		"null":         "null",
		"a string":     `"many tokens"`,
		"an array":     `[]`,
		"a number":     `7`,
		"empty object": `{}`,
	}

	for name, usage := range cases {
		t.Run(name, func(t *testing.T) {
			// The absent case cannot go through assistantLine, which always writes the key.
			line := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","entrypoint":"cli","message":{"model":"sonnet","id":"msg_1","content":[]}}`
			if usage != "" {
				line = assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", usage)
			}

			event := onlySessionEnd(t, mustRead(t, line, finished))
			assertUnreportedTokens(t, event)
			// The call counts are this scan's own measurement and are unaffected by a
			// missing usage block.
			assertCount(t, "ToolCalls", event.ToolCalls, 0)
		})
	}
}

// TestReadPartiallyDecodesAUsageBlock applies inspectable's rule one level down: a
// field arriving at another type costs that field and not the block, because
// encoding/json records the mismatch and carries on with the remaining keys.
func TestReadPartiallyDecodesAUsageBlock(t *testing.T) {
	line := assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1",
		`{"input_tokens":7,"output_tokens":"many"}`)

	event := onlySessionEnd(t, mustRead(t, line, finished))

	assertCount(t, "InputTokens", event.InputTokens, 7)
	if event.OutputTokens != nil {
		t.Fatalf("OutputTokens = %d, want nil", *event.OutputTokens)
	}
}

// TestReadRefusesANegativeOrOverflowingTokenTotal reports unknown rather than a
// wrapped or partial number. It is the only wrong answer that cannot be mistaken
// for a measurement (ADR-0005), and record.Validate would refuse the negative
// anyway — the record has to be built valid, not repaired at write time.
func TestReadRefusesANegativeOrOverflowingTokenTotal(t *testing.T) {
	overflowing := strconv.FormatInt(math.MaxInt64, 10)
	cases := map[string]string{
		"a negative count": assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1",
			`{"input_tokens":-1,"output_tokens":3}`),
		"an addition past int64": strings.Join([]string{
			assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1",
				`{"input_tokens":`+overflowing+`,"output_tokens":3}`),
			assistantLine("entry-2", "session-1", "2026-08-13T12:00:01Z", "msg_2",
				`{"input_tokens":`+overflowing+`,"output_tokens":4}`),
		}, "\n"),
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			event := onlySessionEnd(t, mustRead(t, input, finished))
			if event.InputTokens != nil {
				t.Errorf("InputTokens = %d, want nil", *event.InputTokens)
			}
			// The refusal is per counter: a total this reader will not carry costs that
			// total and not the record.
			if event.OutputTokens == nil {
				t.Error("OutputTokens = nil, want the reported value")
			}
			if err := record.Validate(event); err != nil {
				t.Errorf("Validate() error = %v", err)
			}
		})
	}
}

// TestReadIgnoresAUsageBlockWithNoUsableMessageID fails closed against the failure
// mode the dedup exists to prevent: a block that cannot be identified cannot be
// recognised as a repeat, so counting it is how a token total gets multiplied.
func TestReadIgnoresAUsageBlockWithNoUsableMessageID(t *testing.T) {
	inputs := map[string]string{
		"absent": `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","entrypoint":"cli","message":{"model":"sonnet","usage":` + realUsage + `,"content":[]}}`,
		"empty":  assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "", realUsage),
	}
	for _, value := range hostileValues {
		inputs["hostile: "+value] = fmt.Sprintf(
			`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","entrypoint":"cli",`+
				`"message":{"model":"sonnet","id":%s,"usage":%s,"content":[]}}`,
			quoted(t, value), realUsage)
	}

	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			event := onlySessionEnd(t, mustRead(t, input, finished))
			assertUnreportedTokens(t, event)
		})
	}
}

func TestReadDerivesNoSessionEndOutsideConsent(t *testing.T) {
	input := assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", realUsage)
	// forwardOnly consents to the directory but only from an instant after every
	// entry: the second dimension of consent (ADR-0025). An entry that predates the
	// boundary contributes nothing — not a timestamp, not a token.
	forwardOnly := func(cwd string, at time.Time) (record.Hash, bool) {
		return repo, cwd == consentedPath && !at.Before(callInstant.Add(time.Hour))
	}
	cases := map[string]Resolver{
		"an unconsented directory": deny,
		"before the boundary":      forwardOnly,
	}

	for name, resolve := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := Read(strings.NewReader(input), resolve, names, installedPrimitives, Staleness{}, finished)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if ends := sessionEnds(result.Records); len(ends) != 0 {
				t.Fatalf("session_end records = %d, want 0", len(ends))
			}
		})
	}
}

// TestSessionEndUsesOnlyConsentedActivity splits the two quantities that look like
// one. Liveness comes from every line that named a session and a time, because a
// session still writing in an unconsented directory is demonstrably alive and
// believing it finished would be permanent. What the record is dated by is the
// last *consented* activity.
func TestSessionEndUsesOnlyConsentedActivity(t *testing.T) {
	input := strings.Join([]string{
		assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", "null"),
		// Same session, a directory the resolver refuses, and much later.
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/elsewhere","timestamp":"2026-08-13T13:50:00Z","entrypoint":"cli","message":{"model":"sonnet","id":"msg_2","usage":` + realUsage + `,"content":[]}}`,
	}, "\n")

	stillWriting := Idleness{Timeout: sessionIdleTimeout, Now: callInstant.Add(2 * time.Hour)}
	result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, Staleness{}, stillWriting)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if ends := sessionEnds(result.Records); len(ends) != 0 {
		t.Fatalf("session_end records = %d, want 0 while the session is still writing", len(ends))
	}

	longQuiet := Idleness{Timeout: sessionIdleTimeout, Now: callInstant.Add(4 * time.Hour)}
	event := onlySessionEnd(t, mustRead(t, input, longQuiet))
	if !event.Timestamp.Equal(callInstant) {
		t.Errorf("Timestamp = %v, want the last consented activity %v", event.Timestamp, callInstant)
	}
	assertUnreportedTokens(t, event)
}

// TestReadDerivesNoSessionEndWhenALineWasUnreadable mirrors the staleness rule's
// own blindness gate. A read that could not rule out one of its lines may not
// conclude any session went silent: the line may be the tool_result that
// terminated a buffered call, and the totals would be understated by whatever it
// held.
func TestReadDerivesNoSessionEndWhenALineWasUnreadable(t *testing.T) {
	cases := map[string]string{
		"a line that did not parse":                   `{"uuid":"entry-2","sessionId":"session-1",`,
		"an unusable identity carrying a tool_result": `{"sessionId":"session-1","cwd":"/repo","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}

	for name, unreadable := range cases {
		t.Run(name, func(t *testing.T) {
			input := assistantLine("entry-1", "session-1", "2026-08-13T12:00:00Z", "msg_1", realUsage) +
				"\n" + unreadable + "\n"

			result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, closedSession, finished)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if ends := sessionEnds(result.Records); len(ends) != 0 {
				t.Fatalf("session_end records = %d, want 0", len(ends))
			}
		})
	}
}

// TestReadDerivesNoSessionEndWithNoMappableEntrypoint is fail closed on a
// dimension this build cannot state. Refused is positive on the same transcript,
// so the blindness is reported rather than silent.
func TestReadDerivesNoSessionEndWithNoMappableEntrypoint(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","entrypoint":"sdk-ts","message":{"model":"sonnet","id":"msg_1","usage":` + realUsage + `,"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`

	result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, Staleness{}, finished)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if ends := sessionEnds(result.Records); len(ends) != 0 {
		t.Fatalf("session_end records = %d, want 0", len(ends))
	}
	if result.Refused == 0 {
		t.Fatal("Refused = 0, want the lost invocation reported")
	}
}

// TestReadCarriesEntrypointAndHarnessVersionFromTheEarliestConsentedEntry pins the
// anchor to a total order over source values rather than to arrival order: a
// first-wins fold would let line order decide which version the one record carries.
func TestReadCarriesEntrypointAndHarnessVersionFromTheEarliestConsentedEntry(t *testing.T) {
	earliest := `{"uuid":"entry-a","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"1.0.0","entrypoint":"cli","message":{"model":"sonnet","id":"msg_1","content":[]}}`
	latest := `{"uuid":"entry-b","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:05:00Z","version":"2.0.0","entrypoint":"sdk-cli","message":{"model":"sonnet","id":"msg_2","content":[]}}`

	for name, input := range map[string]string{
		"in order": earliest + "\n" + latest,
		"reversed": latest + "\n" + earliest,
	} {
		t.Run(name, func(t *testing.T) {
			event := onlySessionEnd(t, mustRead(t, input, finished))
			if event.HarnessVersion != "1.0.0" {
				t.Errorf("HarnessVersion = %q, want %q", event.HarnessVersion, "1.0.0")
			}
			if event.Entrypoint != record.EntrypointCLI {
				t.Errorf("Entrypoint = %q, want %q", event.Entrypoint, record.EntrypointCLI)
			}
		})
	}
}

// TestReadEmitsSessionEndsInADeterministicOrder is ADR-0004's byte-identical
// requirement: map iteration order is randomised, so the emission order is sorted
// by session id, which is a total order because it keys the fold.
func TestReadEmitsSessionEndsInADeterministicOrder(t *testing.T) {
	input := strings.Join([]string{
		assistantLine("entry-1", "session-c", "2026-08-13T12:00:00Z", "msg_1", "null"),
		assistantLine("entry-2", "session-a", "2026-08-13T12:00:01Z", "msg_2", "null"),
		assistantLine("entry-3", "session-b", "2026-08-13T12:00:02Z", "msg_3", "null"),
	}, "\n")

	for range 8 {
		ends := sessionEnds(mustRead(t, input, finished).Records)
		got := make([]string, 0, len(ends))
		for _, event := range ends {
			got = append(got, string(event.SessionID))
		}
		if want := []string{"session-a", "session-b", "session-c"}; !slices.Equal(got, want) {
			t.Fatalf("session_end order = %v, want %v", got, want)
		}
	}
}

// TestReadRetainsNothingFromASessionEnd is this record shape's own hostile-payload
// corpus run. ADR-0007 requires one per adapter and per input shape, and a
// session_end is a new way for a record to be built: no tool_use and no
// tool_result contributes to it, and it is the first record to read a usage block.
func TestReadRetainsNothingFromASessionEnd(t *testing.T) {
	for _, value := range hostileValues {
		// Every hostile value goes in a field no record field is derived from: the
		// working directory (which the resolver turns into a hash and which may never
		// travel as a path), a denial kind, an unmodelled sibling, the assistant message
		// id, and every usage sibling this reader does not model.
		hostileUsage := fmt.Sprintf(
			`{"input_tokens":4,"output_tokens":5,"service_tier":%s,"inference_geo":%s,`+
				`"cache_creation":{"note":%s},"output_tokens_details":{"thinking_tokens":6,"note":%s}}`,
			quoted(t, value), quoted(t, "swordfish-"+value), quoted(t, value), quoted(t, "swordfish-"+value))
		transcript := fmt.Sprintf(
			`{"uuid":"entry-1","sessionId":"session-1","cwd":%s,"timestamp":"2026-08-13T12:00:00Z",`+
				`"entrypoint":"cli","toolDenialKind":%s,"pad":%s,`+
				`"message":{"model":"sonnet","id":%s,"usage":%s,"content":[]}}`,
			quoted(t, consentedPath), quoted(t, value), quoted(t, "swordfish-"+value),
			quoted(t, value), hostileUsage)

		result, err := Read(strings.NewReader(transcript), resolver, names, installedPrimitives, closedSession, finished)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		event := onlySessionEnd(t, result)
		if validateErr := record.Validate(event); validateErr != nil {
			t.Errorf("Read(%q) Validate() error = %v", value, validateErr)
		}
		encoded, err := record.Marshal(event)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		for _, fragment := range []string{"swordfish", consentedPath, value, "/", `\`} {
			if strings.Contains(string(encoded), fragment) {
				t.Fatalf("session_end record retains %q: %s", fragment, encoded)
			}
		}
	}
}

// TestSessionStateFinishedSessionsIsNotClosed proves the two predicates are
// independent. ADR-0023 §3's "no second threshold is introduced" governs the
// session-close check that serves the staleness rule; ADR-0034's session-end
// inference is a different question under a different key, and a session can
// answer the two differently.
func TestSessionStateFinishedSessionsIsNotClosed(t *testing.T) {
	sessions := &SessionState{}
	sessions.Observe(0, "session-1", callInstant, 0)

	// Silent for an hour: past session.idle_timeout's 30m, well inside
	// scan.stale_call_timeout's 24h.
	stale := Staleness{Timeout: 24 * time.Hour, Now: callInstant.Add(time.Hour)}
	idle := Idleness{Timeout: sessionIdleTimeout, Now: callInstant.Add(time.Hour)}

	if sessions.Closed("session-1", stale) {
		t.Error("Closed() = true under a 24h staleness threshold")
	}
	if got := sessions.finishedSessions(idle); len(got) != 1 || got[0] != "session-1" {
		t.Errorf("finishedSessions() = %v, want [session-1]", got)
	}
	// And a session this state never observed is never finished, for closed()'s
	// reason: absence of evidence that a session is alive is not evidence it ended.
	if got := (&SessionState{}).finishedSessions(idle); len(got) != 0 {
		t.Errorf("finishedSessions() on an empty state = %v, want none", got)
	}
}

// TestSessionStateNeverFinishesABlindSession is constraint 11 on the second
// predicate. A source carrying this session held a line the reader could not rule
// out as a terminator, so its last activity is known-understated and its totals
// would be understated by whatever that line held — and a session_end is permanent
// (ADR-0015 rejects upsert, ADR-0004 deduplicates the correction away).
func TestSessionStateNeverFinishesABlindSession(t *testing.T) {
	sessions := &SessionState{}
	sessions.Observe(0, "session-blind", callInstant, 0)
	sessions.Observe(0, "session-clear", callInstant, 100)
	sessions.MarkBlind("session-blind")

	idle := Idleness{Timeout: sessionIdleTimeout, Now: callInstant.Add(time.Hour)}

	got := sessions.finishedSessions(idle)
	if len(got) != 1 || got[0] != "session-clear" {
		t.Fatalf("finishedSessions() = %v, want [session-clear]: one source's blindness is not every session's", got)
	}
}

// mustRead is the session-grain entry point into Read: a real Idleness, no
// staleness rule unless a test asks for one.
func mustRead(t *testing.T, input string, idle Idleness) Result {
	t.Helper()
	result, err := Read(strings.NewReader(input), resolver, names, installedPrimitives, Staleness{}, idle)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	return result
}

func assertCount(t *testing.T, field string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %d", field, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", field, *got, want)
	}
}

// assertUnreportedTokens is the "never 0" half of the nullable contract: an
// unreported total has to stay absent, because a zero would be read as a
// measurement the harness never made.
func assertUnreportedTokens(t *testing.T, event record.Record) {
	t.Helper()
	for field, got := range map[string]*int64{
		"InputTokens":         event.InputTokens,
		"OutputTokens":        event.OutputTokens,
		"CacheReadTokens":     event.CacheReadTokens,
		"CacheCreationTokens": event.CacheCreationTokens,
		"ThinkingTokens":      event.ThinkingTokens,
	} {
		if got != nil {
			t.Errorf("%s = %d, want nil", field, *got)
		}
	}
}

// D8's second intended side effect, pinned deliberately rather than left to surface as
// a mystery diff. Modelling a plain-string message.content means such a line now yields
// an entry, so it reaches observeSessionGrain and a session can date its end from a user
// turn.
//
// That closes a latent inconsistency rather than opening one: SessionState.lastActivity
// already folded those lines, because the observe call runs before the decode gate, so
// the grain's own lastSeen was the value that disagreed with it. It is nonetheless a
// change to the dimensions a session_end carries for an id that does not change, which
// is exactly why this series bumps the schema version and rebuilds (ADR-0004, ADR-0015).
func TestSessionEndDatesItselfFromATrailingUserTurn(t *testing.T) {
	last := "2026-08-13T12:05:00Z"
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"` + last + `","type":"user","message":{"role":"user","content":"carry on"}}`,
	}, "\n")

	event := onlySessionEnd(t, mustRead(t, input, finished))

	want, err := time.Parse(time.RFC3339, last)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !event.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want the trailing user turn's %v", event.Timestamp, want)
	}
}
