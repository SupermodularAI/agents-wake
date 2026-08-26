package claudecode

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// twoSources drives one Scan over sources in the given order and returns every
// record it derived, in derivation order, plus the walk's final Result.
//
// Multi-source cases are built inline exactly as reader_test.go builds
// single-source ones. A fixture under testdata/ is a captured transcript and is
// never hand-written (AGENTS.md § Off-limits paths), and what these tests need is
// not a real transcript but two of them sharing one session id.
func twoSources(t *testing.T, stale Staleness, idle Idleness, sources ...string) ([]record.Record, Result) {
	t.Helper()
	scan := NewScan(resolver, names, stale, idle)
	records := []record.Record{}
	for index, source := range sources {
		result, err := scan.Read(strings.NewReader(source))
		if err != nil {
			t.Fatalf("Scan.Read(source %d) error = %v", index, err)
		}
		records = append(records, result.Records...)
	}
	final := scan.Close()
	return append(records, final.Records...), final
}

// openCall is one tool_use with no tool_result: a call the staleness rule
// is the only thing that can resolve.
func openCall(uuid, session, at, callID string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":%q,"version":"1.0.0","entrypoint":"cli",`+
			`"message":{"model":"sonnet","id":%q,"content":[{"type":"tool_use","id":%q,"name":"Bash"}]}}`,
		uuid, session, at, uuid+"-msg", callID)
}

// attributedRun is a non-sidechain end_turn entry carrying a skill attribution:
// the Shape-A candidate ADR-0023 defers until its session closes.
func attributedRun(uuid, session, at, skill string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":%q,"version":"1.0.0","entrypoint":"cli",`+
			`"attributionSkill":%q,"message":{"model":"sonnet","id":%q,"stop_reason":"end_turn","content":[]}}`,
		uuid, session, at, skill, uuid+"-msg")
}

// skillCall is a Skill tool_use naming skill, terminated by its result.
func skillCall(uuid, session, at, callID, skill string) string {
	return toolCallLines(uuid, session, at, callID, "Skill", fmt.Sprintf(`{"skill":%q}`, skill))
}

// splitSession is the shape ADR-0036 is about: one session id whose lines live in
// two transcripts — the parent's file and one subagent's — each holding a
// terminated call and its own assistant usage block under a distinct message id.
func splitSession() (parent, subagent string) {
	parent = strings.Join([]string{
		toolCallLines("parent-1", "session-1", "2026-08-13T12:00:00Z", "call-parent", "Bash", `{}`),
		assistantLine("parent-2", "session-1", "2026-08-13T12:00:01Z", "msg_parent", realUsage),
	}, "\n")
	subagent = strings.Join([]string{
		toolCallLines("agent-1", "session-1", "2026-08-13T12:00:02Z", "call-agent", "Bash", `{}`),
		assistantLine("agent-2", "session-1", "2026-08-13T12:00:03Z", "msg_agent", realUsage),
	}, "\n")
	return parent, subagent
}

// AC 1: one session_end, whose totals cover every source of the session.
func TestScanDerivesOneSessionEndAcrossEverySourceOfOneSession(t *testing.T) {
	parent, subagent := splitSession()

	records, result := twoSources(t, Staleness{}, finished, parent, subagent)

	ends := sessionEnds(records)
	if len(ends) != 1 {
		t.Fatalf("session_end records = %d, want exactly 1 (result = %+v)", len(ends), result)
	}
	end := ends[0]
	assertCount(t, "ToolCalls", end.ToolCalls, 2)
	assertCount(t, "BuiltinToolCalls", end.BuiltinToolCalls, 2)
	// realUsage reports 6 input tokens per message id, and the two sources carry two
	// distinct ids — so the union's total is both, not one source's view of it.
	assertCount(t, "InputTokens", end.InputTokens, 12)
}

// AC 2: an idle subagent file must not close a session whose parent shows activity.
func TestScanDoesNotCloseASessionLiveInAnotherSource(t *testing.T) {
	old := assistantLine("agent-1", "session-1", "2026-08-13T12:00:00Z", "msg_agent", realUsage)
	recent := assistantLine("parent-1", "session-1", "2026-08-13T13:50:00Z", "msg_parent", realUsage)

	records, result := twoSources(t, Staleness{}, finished, old, recent)

	if ends := sessionEnds(records); len(ends) != 0 {
		t.Fatalf("session_end records = %d, want 0: the parent source shows the session active (result = %+v)", len(ends), result)
	}
}

// Constraint 3: resolveStaleCalls is the fourth consumer of the close
// determination, and it must see the union too — a call in a quiet subagent file
// belongs to a session another file shows running.
func TestScanDoesNotInterruptACallLiveInAnotherSource(t *testing.T) {
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(2 * time.Hour)}
	old := openCall("agent-1", "session-1", "2026-08-13T12:00:00Z", "call-agent")
	recent := assistantLine("parent-1", "session-1", "2026-08-13T13:30:00Z", "msg_parent", realUsage)

	records, result := twoSources(t, stale, Idleness{}, old, recent)

	if result.Interrupted != 0 {
		t.Errorf("Interrupted = %d, want 0: the session is live in the other source", result.Interrupted)
	}
	if result.Pending != 1 {
		t.Errorf("Pending = %d, want 1: the call stays buffered", result.Pending)
	}
	for _, event := range records {
		if event.Outcome != nil && *event.Outcome == record.OutcomeInterrupted {
			t.Fatalf("record %+v was emitted interrupted from a partial view", event)
		}
	}
}

// AC 3: a Skill tool_use in one source and the attributed end_turn for the same
// skill in another are one run. The fallback must be dropped, in either order —
// the shape the bundle measured 321 times on a real machine.
func TestScanDropsAShapeAFallbackMatchedInAnotherSource(t *testing.T) {
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(4 * time.Hour)}
	toolUse := skillCall("parent-1", "session-1", "2026-08-13T12:00:00Z", "call-1", "pr-review")
	endTurn := attributedRun("agent-1", "session-1", "2026-08-13T12:00:05Z", "pr-review")

	for name, order := range map[string][]string{
		"tool_use first": {toolUse, endTurn},
		"end_turn first": {endTurn, toolUse},
	} {
		t.Run(name, func(t *testing.T) {
			records, result := twoSources(t, stale, Idleness{}, order...)

			for _, event := range records {
				if event.Kind == record.KindSkill && event.Invoker == record.InvokerUser {
					t.Errorf("Shape-A fallback %+v emitted: the Skill tool_use in the other source already describes this run", event)
				}
			}
			if result.AmbiguousSkillRuns != 0 {
				t.Errorf("AmbiguousSkillRuns = %d, want 0: the run is covered, not uncertain", result.AmbiguousSkillRuns)
			}
		})
	}
}

// AC 4: what a scan derives does not depend on which order the walk visited the
// sources in (ADR-0004, ADR-0036 §1).
//
// The comparison is over the set, ordered by the one thing that identifies a record
// — its event id — rather than over the emission sequence. A record's *content* is
// what has to be walk-order-independent: the emission sequence is per source by
// construction, because each source is read and persisted as it is visited and
// requirement 1 keeps it that way (never concatenated, never all buffered to the
// end).
func TestScanDerivesTheSameRecordsInEitherSourceOrder(t *testing.T) {
	parent, subagent := splitSession()

	forward, _ := twoSources(t, Staleness{}, finished, parent, subagent)
	reverse, _ := twoSources(t, Staleness{}, finished, subagent, parent)

	if len(forward) != len(reverse) {
		t.Fatalf("record counts differ: %d forward, %d reversed", len(forward), len(reverse))
	}
	byEventID(forward)
	byEventID(reverse)
	if !reflect.DeepEqual(forward, reverse) {
		t.Fatalf("records differ by walk order:\nforward  = %+v\nreversed = %+v", forward, reverse)
	}
}

// byEventID sorts records into the total order their derived identity gives them,
// so two walks can be compared as sets. The event id is unique per record by
// construction (ADR-0004), so the order is total.
func byEventID(records []record.Record) {
	slices.SortFunc(records, func(a, b record.Record) int {
		return cmp.Compare(a.EventID, b.EventID)
	})
}

// AC 5: a source whose own lines all look quiet keeps its cursor floor while
// another source shows the session running (ADR-0023 §5 through ADR-0036).
func TestScanHoldsASourceFloorForASessionOpenInAnotherSource(t *testing.T) {
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(2 * time.Hour)}
	quiet := assistantLine("agent-1", "session-1", "2026-08-13T12:00:00Z", "msg_agent", realUsage)
	recent := assistantLine("parent-1", "session-1", "2026-08-13T13:30:00Z", "msg_parent", realUsage)

	scan := NewScan(resolver, names, stale, Idleness{})
	for index, source := range []string{quiet, recent} {
		if _, err := scan.Read(strings.NewReader(source)); err != nil {
			t.Fatalf("Scan.Read(source %d) error = %v", index, err)
		}
	}
	scan.Close()

	if open, offset := scan.sessions.SourceFloor(0, stale); !open || offset != 0 {
		t.Errorf("SourceFloor(0) = (%t, %d), want (true, 0): the quiet source's floor is held", open, offset)
	}
	if open := scan.sessions.OpenSessions(stale); open != 1 {
		t.Errorf("OpenSessions() = %d, want 1", open)
	}
}

// Constraint 11: one source's unreadable line must not stop the staleness rule
// for a session that source never carried.
func TestScanBlindnessIsPerSessionNotPerWalk(t *testing.T) {
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(4 * time.Hour)}
	// A syntax error, so inspectable reports false and the line could have been
	// anything — including the tool_result that terminated the call below it.
	blind := strings.Join([]string{
		`{"uuid":"entry-2","sessionId":"session-1",`,
		openCall("parent-1", "session-1", "2026-08-13T12:00:00Z", "call-parent"),
	}, "\n")
	clear := openCall("other-1", "session-2", "2026-08-13T12:00:00Z", "call-other")

	records, result := twoSources(t, stale, Idleness{}, blind, clear)

	if result.Interrupted != 1 || result.Pending != 1 {
		t.Fatalf("Interrupted = %d, Pending = %d, want 1 and 1 (result = %+v)", result.Interrupted, result.Pending, result)
	}
	for _, event := range records {
		if event.Outcome != nil && *event.Outcome == record.OutcomeInterrupted && event.SessionID != "session-2" {
			t.Errorf("interrupted record %+v belongs to the blind session", event)
		}
	}
}

// truncatedSource is a source whose bytes stop part-way: every line before the
// failure is delivered, and then the read fails. It is what a transcript a running
// harness is appending to looks like when the read does not complete — os.Open
// succeeding says nothing about reading to the end.
type truncatedSource struct {
	head *strings.Reader
	err  error
}

func (s *truncatedSource) Read(buffer []byte) (int, error) {
	if s.head.Len() > 0 {
		return s.head.Read(buffer)
	}
	return 0, s.err
}

// Requirement 4, plan §3.3, ADR-0015: a source whose read failed part-way has
// already folded its lines into the walk's buffers, so the sessions it carried are
// judged from a view that is known-incomplete. None of them may be resolved — the
// unread remainder of that source may hold the tool_result that terminated a
// buffered call, and both the interrupted record and the session_end are permanent
// (ADR-0015 rejects upsert, ADR-0004 deduplicates the correction away).
func TestScanResolvesNoSessionOfASourceThatFailedPartWay(t *testing.T) {
	stale := Staleness{Timeout: time.Hour, Now: callInstant.Add(4 * time.Hour)}
	// Newline-terminated, so both lines are delivered before the failure: what is
	// unread is whatever the harness wrote after them.
	partial := strings.Join([]string{
		openCall("parent-1", "session-1", "2026-08-13T12:00:00Z", "call-parent"),
		assistantLine("parent-2", "session-1", "2026-08-13T12:00:01Z", "msg_parent", realUsage),
		"",
	}, "\n")
	clean := openCall("other-1", "session-2", "2026-08-13T12:00:00Z", "call-other")

	scan := NewScan(resolver, names, stale, finished)
	source := &truncatedSource{head: strings.NewReader(partial), err: errors.New("device error")}
	if _, err := scan.Read(source); err == nil {
		t.Fatal("Scan.Read(truncated source) error = nil, want the failed read reported")
	}
	result, err := scan.Read(strings.NewReader(clean))
	if err != nil {
		t.Fatalf("Scan.Read(clean source) error = %v", err)
	}
	final := scan.Close()

	if final.Interrupted != 1 {
		t.Errorf("Interrupted = %d, want 1: only the clean source's session may be judged silent", final.Interrupted)
	}
	if final.Pending != 1 {
		t.Errorf("Pending = %d, want 1: the truncated source's call stays buffered", final.Pending)
	}
	for _, event := range append(result.Records, final.Records...) {
		if event.SessionID == "session-1" {
			t.Errorf("record derived for the truncated source's session: %+v", event)
		}
	}
}

// Constraint 14, ADR-0007: the walk-scoped state holds ordinals, ids, timestamps,
// offsets and counts — no path and no transcript value.
func TestScanRetainsNoPathOrTranscriptValue(t *testing.T) {
	for _, value := range hostileValues {
		source := strings.Join([]string{
			skillCall("parent-1", "session-1", "2026-08-13T12:00:00Z", "call-1", value),
			attributedRun("parent-2", "session-1", "2026-08-13T12:00:01Z", value),
			assistantLine("parent-3", "session-1", "2026-08-13T12:00:02Z", "msg_parent", realUsage),
		}, "\n")

		records, _ := twoSources(t, Staleness{Timeout: time.Hour, Now: callInstant.Add(4 * time.Hour)}, finished, source)

		for _, event := range records {
			encoded, err := record.Marshal(event)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			for _, fragment := range []string{value, consentedPath, "/", `\`} {
				if strings.Contains(string(encoded), fragment) {
					t.Fatalf("scan-derived record retains %q: %s", fragment, encoded)
				}
			}
		}
	}
}
