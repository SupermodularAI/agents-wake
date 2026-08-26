package claudecode

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// parentChild builds one deferred child's record with just the fields the
// precedence reads. It is deliberately not a valid record — parentOf is pure over
// (agent id, ViaSkill, session id, event id) and reads nothing else.
func parentChild(session, viaSkill record.Identifier, eventID record.Hash) record.Record {
	return record.Record{SessionID: session, ViaSkill: viaSkill, EventID: eventID}
}

func TestSkillTargetsResolvesExactlyOneRecord(t *testing.T) {
	targets := skillTargets{}
	targets.observe(record.Record{
		Kind: record.KindSkill, SessionID: "session-1", Name: "pr-review", EventID: "id-1",
	})

	id, ok := targets.target(skillRun{session: "session-1", name: "pr-review"})
	if !ok {
		t.Fatal("target() reported no target for a single observed record")
	}
	if id != "id-1" {
		t.Fatalf("target() = %q, want %q", id, "id-1")
	}
}

// TestSkillTargetsRefusesAnAmbiguousName is C6: DG-85 counts a typed invocation per
// occurrence, so one name legitimately resolves to several records. Picking a
// sibling would be a permanent guess (ADR-0015 rejects upsert), so the pair reports
// no target and the caller drops to the session span.
func TestSkillTargetsRefusesAnAmbiguousName(t *testing.T) {
	targets := skillTargets{}
	for _, id := range []record.Hash{"id-1", "id-2", "id-3"} {
		targets.observe(record.Record{
			Kind: record.KindCommand, SessionID: "session-1", Name: "pr-review", EventID: id,
		})
	}

	if id, ok := targets.target(skillRun{session: "session-1", name: "pr-review"}); ok {
		t.Fatalf("target() resolved an ambiguous name to %q", id)
	}
}

func TestSkillTargetsRefusesAnUnobservedName(t *testing.T) {
	targets := skillTargets{}
	targets.observe(record.Record{
		Kind: record.KindSkill, SessionID: "session-1", Name: "pr-review", EventID: "id-1",
	})

	if _, ok := targets.target(skillRun{session: "session-1", name: "run-sdlc"}); ok {
		t.Fatal("target() resolved a name no record was observed for")
	}
	if _, ok := targets.target(skillRun{session: "session-2", name: "pr-review"}); ok {
		t.Fatal("target() resolved a name across sessions")
	}
}

// TestSkillTargetsIgnoresANonInvocationKind keeps the index a projection of the
// records that can actually be a case-2 parent: a skill or command invocation, and
// nothing else.
func TestSkillTargetsIgnoresANonInvocationKind(t *testing.T) {
	targets := skillTargets{}
	for _, kind := range []record.Kind{
		record.KindMCPTool, record.KindBuiltinTool, record.KindSubagent, record.KindSessionEnd,
	} {
		targets.observe(record.Record{
			Kind: kind, SessionID: "session-1", Name: "pr-review", EventID: "id-1",
		})
	}

	if _, ok := targets.target(skillRun{session: "session-1", name: "pr-review"}); ok {
		t.Fatal("target() resolved a name only non-invocation records carried")
	}
}

func TestParentOfPrefersTheSubagentRunOverTheSkill(t *testing.T) {
	targets := skillTargets{}
	targets.observe(record.Record{
		Kind: record.KindSkill, SessionID: "session-1", Name: "run-sdlc", EventID: "skill-id",
	})
	resolution := parentage{
		subagents: map[record.Identifier]record.Hash{"agent-1": "subagent-id"},
		skills:    targets,
	}

	parent, ok := resolution.parentOf(deferredChild{
		event:   parentChild("session-1", "run-sdlc", "child-id"),
		agentID: "agent-1",
	})
	if !ok {
		t.Fatal("parentOf() deferred a child whose subagent record was emitted")
	}
	if parent != "subagent-id" {
		t.Fatalf("parentOf() = %q, want the subagent record", parent)
	}
}

// TestParentOfDefersAChildOfAnUnresolvedRun is C4's middle row: the record will
// exist, just not yet, so the child waits rather than pointing at the session and
// being wrong forever.
func TestParentOfDefersAChildOfAnUnresolvedRun(t *testing.T) {
	resolution := parentage{
		buffered: map[record.Identifier]*subagentRun{"agent-1": {}},
		skills:   skillTargets{},
	}

	parent, ok := resolution.parentOf(deferredChild{
		event:   parentChild("session-1", "", "child-id"),
		agentID: "agent-1",
	})
	if ok {
		t.Fatalf("parentOf() resolved %q for a run this walk has not resolved", parent)
	}
}

// TestParentOfFallsThroughARefusedRun is C4's third row: an anchored run with no
// usable name emits no record and never will, so a case-1 link would reference a
// record that cannot exist (ADR-0035 §6). It falls through the precedence instead.
func TestParentOfFallsThroughARefusedRun(t *testing.T) {
	resolution := parentage{
		subagents: map[record.Identifier]record.Hash{},
		buffered:  map[record.Identifier]*subagentRun{},
		skills:    skillTargets{},
	}

	parent, ok := resolution.parentOf(deferredChild{
		event:   parentChild("session-1", "", "child-id"),
		agentID: "agent-refused",
	})
	if !ok {
		t.Fatal("parentOf() deferred a child of a run that was refused")
	}
	if want := sessionParent("session-1"); parent != want {
		t.Fatalf("parentOf() = %q, want the session span %q", parent, want)
	}
}

func TestParentOfUsesTheSkillWhenThereIsNoSubagent(t *testing.T) {
	targets := skillTargets{}
	targets.observe(record.Record{
		Kind: record.KindSkill, SessionID: "session-1", Name: "pr-review", EventID: "skill-id",
	})
	resolution := parentage{skills: targets}

	parent, ok := resolution.parentOf(deferredChild{
		event: parentChild("session-1", "pr-review", "child-id"),
	})
	if !ok {
		t.Fatal("parentOf() deferred a skill-attributed child")
	}
	if parent != "skill-id" {
		t.Fatalf("parentOf() = %q, want the skill record", parent)
	}
}

func TestParentOfFallsBackToTheSessionForAnAmbiguousSkill(t *testing.T) {
	targets := skillTargets{}
	for _, id := range []record.Hash{"id-1", "id-2"} {
		targets.observe(record.Record{
			Kind: record.KindSkill, SessionID: "session-1", Name: "pr-review", EventID: id,
		})
	}
	resolution := parentage{skills: targets}

	parent, ok := resolution.parentOf(deferredChild{
		event: parentChild("session-1", "pr-review", "child-id"),
	})
	if !ok {
		t.Fatal("parentOf() deferred a child whose skill name was ambiguous")
	}
	if want := sessionParent("session-1"); parent != want {
		t.Fatalf("parentOf() = %q, want the session span %q", parent, want)
	}
}

// TestParentOfRefusesToParentARecordOntoItself is C10's one reachable self-match,
// closed by a pure comparison rather than by a stateful ancestor walk.
func TestParentOfRefusesToParentARecordOntoItself(t *testing.T) {
	targets := skillTargets{}
	targets.observe(record.Record{
		Kind: record.KindSkill, SessionID: "session-1", Name: "pr-review", EventID: "child-id",
	})
	resolution := parentage{skills: targets}

	parent, ok := resolution.parentOf(deferredChild{
		event: parentChild("session-1", "pr-review", "child-id"),
	})
	if !ok {
		t.Fatal("parentOf() deferred a self-matching child")
	}
	if parent == "child-id" {
		t.Fatal("parentOf() parented a record onto itself")
	}
	if want := sessionParent("session-1"); parent != want {
		t.Fatalf("parentOf() = %q, want the session span %q", parent, want)
	}
}

// TestResolveDeferredChildrenWaitsForTheSession pins why resolution is at session
// close and not earlier: the ambiguity rule needs a session's final count, and a
// count taken while the session runs can still grow.
func TestResolveDeferredChildrenWaitsForTheSession(t *testing.T) {
	now := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	sessions := &SessionState{}
	sessions.Observe(0, "session-open", now, 0)
	stale := Staleness{Timeout: time.Hour, Now: now}

	deferred := []deferredChild{{
		event:  parentChild("session-open", "", "child-id"),
		source: 0,
	}}
	if got := resolveDeferredChildren(deferred, sessions, stale, parentage{skills: skillTargets{}}); len(got) != 0 {
		t.Fatalf("resolveDeferredChildren() emitted %d records for an open session", len(got))
	}
}

func TestResolveDeferredChildrenOrdersByTimestampThenEventID(t *testing.T) {
	now := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	sessions := &SessionState{}
	sessions.Observe(0, "session-1", now.Add(-2*time.Hour), 0)
	stale := Staleness{Timeout: time.Hour, Now: now}

	early := parentChild("session-1", "", "bbbb")
	early.Timestamp = now.Add(-3 * time.Hour)
	lateA := parentChild("session-1", "", "aaaa")
	lateA.Timestamp = now.Add(-2 * time.Hour)
	lateB := parentChild("session-1", "", "cccc")
	lateB.Timestamp = now.Add(-2 * time.Hour)

	deferred := []deferredChild{
		{event: lateB, source: 2},
		{event: early, source: 0},
		{event: lateA, source: 1},
	}
	resolved := resolveDeferredChildren(deferred, sessions, stale, parentage{skills: skillTargets{}})
	if len(resolved) != 3 {
		t.Fatalf("resolveDeferredChildren() emitted %d records, want 3", len(resolved))
	}
	want := []record.Hash{"bbbb", "aaaa", "cccc"}
	for i, derived := range resolved {
		if derived.event.EventID != want[i] {
			t.Fatalf("record %d = %q, want %q", i, derived.event.EventID, want[i])
		}
		if derived.event.ParentEventID != sessionParent("session-1") {
			t.Fatalf("record %d parent = %q, want the session span", i, derived.event.ParentEventID)
		}
	}
	// The source ordinal travels with the record, so Close can credit the source
	// whose only contribution was a deferred child.
	if resolved[0].source != 0 || resolved[1].source != 1 || resolved[2].source != 2 {
		t.Fatalf("resolveDeferredChildren() lost the source ordinals: %v %v %v",
			resolved[0].source, resolved[1].source, resolved[2].source)
	}
}

func TestSessionParentIsTheSessionEndsEventID(t *testing.T) {
	want := record.DeriveEventID(harness, sessionEndSourceEvent("session-1"))
	if got := sessionParent("session-1"); got != want {
		t.Fatalf("sessionParent() = %q, want %q", got, want)
	}
}

// The integration half of this file. Every fixture below is a hand-written JSONL
// string driven through NewScan/Read/Close, exactly as scan_test.go and
// subagent_test.go build theirs: the repo-root testdata/ holds captured transcripts
// and is never hand-written (CLAUDE.md § Off-limits paths), and what these tests need
// is not a real transcript but a specific parentage shape.
//
// Every one of them passes a closing Staleness, because ADR-0035 resolves a deferred
// child at session close — a test that wants such a record emitted has to say the
// session ended.

// subagentToolCall is one subagent transcript's worth of lines: an anchor entry
// declaring the agent id and its name, then a terminated tool call on an entry
// carrying the same agent id. attributionSkill is what a subagent's entries inherit
// from the parent turn, so it is the case-1-outranks-case-2 shape when set.
func subagentToolCall(agentID, agentName, session, at, callID, tool, attributionSkill string) string {
	skillField := ""
	if attributionSkill != "" {
		skillField = fmt.Sprintf(`"attributionSkill":%q,`, attributionSkill)
	}
	return strings.Join([]string{
		fmt.Sprintf(
			`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":%q,"version":"1.0.0","entrypoint":"cli",`+
				`"agentId":%q,"attributionAgent":%q,"isSidechain":true,`+
				`"message":{"model":"sonnet","id":%q,"content":[]}}`,
			agentID+"-anchor", session, at, agentID, agentName, agentID+"-anchor-msg"),
		fmt.Sprintf(
			`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":%q,"version":"1.0.0","entrypoint":"cli",`+
				`"agentId":%q,"attributionAgent":%q,%s"isSidechain":true,`+
				`"message":{"model":"sonnet","id":%q,"content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]}}`,
			agentID+"-call", session, at, agentID, agentName, skillField, agentID+"-call-msg", callID, tool),
		fmt.Sprintf(
			`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":%q,"entrypoint":"cli","agentId":%q,"isSidechain":true,`+
				`"message":{"content":[{"type":"tool_result","tool_use_id":%q,"is_error":false}]}}`,
			agentID+"-result", session, at, agentID, callID),
	}, "\n")
}

// attributedToolCall is a terminated tool call whose entry carries a skill
// attribution — the case-2 child shape, and the one whose emission ADR-0035 moves
// from Read to Close.
func attributedToolCall(uuid, session, at, callID, tool, attributionSkill string) string {
	return strings.Join([]string{
		fmt.Sprintf(
			`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":%q,"version":"1.0.0","entrypoint":"cli",`+
				`"attributionSkill":%q,"message":{"model":"sonnet","id":%q,`+
				`"content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]}}`,
			uuid, session, at, attributionSkill, uuid+"-msg", callID, tool),
		fmt.Sprintf(
			`{"uuid":%q,"sessionId":%q,"cwd":"/repo","timestamp":%q,"entrypoint":"cli",`+
				`"message":{"content":[{"type":"tool_result","tool_use_id":%q,"is_error":false}]}}`,
			uuid+"-result", session, at, callID),
	}, "\n")
}

// closing is a Staleness and an Idleness under which every fixture in this file —
// all stamped at or just after callInstant — has gone quiet, so a deferred child and
// a session_end are both resolved.
var (
	closingStale = Staleness{Timeout: time.Hour, Now: callInstant.Add(8 * time.Hour)}
	closingIdle  = Idleness{Timeout: sessionIdleTimeout, Now: callInstant.Add(8 * time.Hour)}
)

// byKind returns the records of one kind, so a test can name the record it means
// without counting the ones beside it.
func byKind(records []record.Record, kind record.Kind) []record.Record {
	found := make([]record.Record, 0, len(records))
	for _, event := range records {
		if event.Kind == kind {
			found = append(found, event)
		}
	}
	return found
}

// onlyOfKind fails unless exactly one record of kind exists.
func onlyOfKind(t *testing.T, records []record.Record, kind record.Kind) record.Record {
	t.Helper()
	found := byKind(records, kind)
	if len(found) != 1 {
		t.Fatalf("%s records = %d, want exactly 1", kind, len(found))
	}
	return found[0]
}

// threeLevelChain is AC 1's fixture: a parent transcript that invokes a skill and a
// subagent, and the subagent's own transcript, in which an MCP tool call happens.
func threeLevelChain() (parent, subagent string) {
	parent = strings.Join([]string{
		skillCall("parent-1", "session-1", "2026-08-13T12:00:00Z", "call-skill", "run-sdlc"),
		// An Agent tool_use derives nothing by design: the subagent's own transcript is
		// that invocation's canonical source (ADR-0036 §2).
		fmt.Sprintf(
			`{"uuid":"parent-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z",`+
				`"version":"1.0.0","entrypoint":"cli","message":{"model":"sonnet","id":"parent-2-msg",`+
				`"content":[{"type":"tool_use","id":%q,"name":"Agent","input":{}}]}}`, "call-agent"),
		assistantLine("parent-3", "session-1", "2026-08-13T12:00:02Z", "msg_parent", realUsage),
	}, "\n")
	subagent = subagentToolCall("agent-1", "sdlc-plan", "session-1", "2026-08-13T12:00:03Z",
		"call-mcp", "mcp__atlassian__search", "run-sdlc")
	return parent, subagent
}

// TestAnMCPToolInsideASubagentReachesTheSessionSpanInThreeLevels is AC 1. The chain
// is mcp_tool -> subagent -> session_end: three levels, which is the level ADR-0036
// §Consequences says parentage gains.
func TestAnMCPToolInsideASubagentReachesTheSessionSpanInThreeLevels(t *testing.T) {
	parent, subagent := threeLevelChain()

	records, result := twoSources(t, closingStale, closingIdle, parent, subagent)

	tool := onlyOfKind(t, records, record.KindMCPTool)
	run := onlyOfKind(t, records, record.KindSubagent)
	end := onlyOfKind(t, records, record.KindSessionEnd)

	wantSubagentID := record.DeriveEventID(harness, subagentSourceEvent("agent-1"))
	if run.EventID != wantSubagentID {
		t.Fatalf("subagent EventID = %q, want %q (result = %+v)", run.EventID, wantSubagentID, result)
	}
	if tool.ParentEventID != wantSubagentID {
		t.Errorf("mcp_tool ParentEventID = %q, want the subagent record %q", tool.ParentEventID, wantSubagentID)
	}
	if want := sessionParent("session-1"); run.ParentEventID != want {
		t.Errorf("subagent ParentEventID = %q, want the session span %q", run.ParentEventID, want)
	}
	if end.ParentEventID != "" {
		t.Errorf("session_end ParentEventID = %q, want none: it is the trace root", end.ParentEventID)
	}
	// Two hops from the leaf to the root, walked rather than asserted piecewise.
	byID := map[record.Hash]record.Record{}
	for _, event := range records {
		byID[event.EventID] = event
	}
	hops := 0
	at := tool
	for at.EventID != end.EventID {
		if at.ParentEventID == "" {
			t.Fatalf("the walk stopped at %q, which is rootless and is not the session_end", at.EventID)
		}
		next, exists := byID[at.ParentEventID]
		if !exists {
			t.Fatalf("hop %d points at %q, which no record carries", hops, at.ParentEventID)
		}
		at = next
		hops++
		if hops > len(records) {
			t.Fatal("the walk from mcp_tool did not terminate")
		}
	}
	if hops != 2 {
		t.Errorf("hops from mcp_tool to session_end = %d, want 2 (three levels)", hops)
	}
}

// TestASubagentChildParentsOntoItsOwnTranscriptEvenWhenTheNameRepeats is AC 2. Three
// runs of one agent name: the parent is computed from the agent id each transcript
// declares, so the repeated name is not ambiguity at all (ADR-0036 §2).
func TestASubagentChildParentsOntoItsOwnTranscriptEvenWhenTheNameRepeats(t *testing.T) {
	sources := []string{
		assistantLine("parent-1", "session-1", "2026-08-13T12:00:00Z", "msg_parent", realUsage),
	}
	for _, agentID := range []string{"agent-1", "agent-2", "agent-3"} {
		sources = append(sources, subagentToolCall(agentID, "sdlc-plan", "session-1",
			"2026-08-13T12:00:01Z", "call-"+agentID, "Bash", ""))
	}

	records, result := twoSources(t, closingStale, closingIdle, sources...)

	runs := byKind(records, record.KindSubagent)
	if len(runs) != 3 {
		t.Fatalf("subagent records = %d, want 3 (result = %+v)", len(runs), result)
	}
	// One name across all three, which is what makes "no name matching" an
	// observation rather than a claim.
	for _, run := range runs {
		if run.Name != runs[0].Name {
			t.Fatalf("subagent names differ: %q and %q", run.Name, runs[0].Name)
		}
	}
	children := byKind(records, record.KindBuiltinTool)
	if len(children) != 3 {
		t.Fatalf("builtin_tool records = %d, want 3", len(children))
	}
	parents := map[record.Hash]struct{}{}
	for _, child := range children {
		parents[child.ParentEventID] = struct{}{}
	}
	if len(parents) != 3 {
		t.Fatalf("distinct parents = %d, want 3: each child belongs to its own transcript", len(parents))
	}
	for _, agentID := range []record.Identifier{"agent-1", "agent-2", "agent-3"} {
		want := record.DeriveEventID(harness, subagentSourceEvent(agentID))
		if _, linked := parents[want]; !linked {
			t.Errorf("no child parents onto %s's own record %q", agentID, want)
		}
	}
}

// TestASkillNameResolvingToSeveralRecordsParentsOntoTheSession is AC 3 — the case
// DG-85 made common rather than hypothetical.
func TestASkillNameResolvingToSeveralRecordsParentsOntoTheSession(t *testing.T) {
	t.Run("three typed occurrences", func(t *testing.T) {
		source := strings.Join([]string{
			typedTurn("typed-1", "pr-review", "2026-08-13T12:00:00Z"),
			typedTurn("typed-2", "pr-review", "2026-08-13T12:00:01Z"),
			typedTurn("typed-3", "pr-review", "2026-08-13T12:00:02Z"),
			attributedToolCall("call-entry", "session-1", "2026-08-13T12:00:03Z", "call-1", "Bash", "pr-review"),
		}, "\n")

		records, result := twoSources(t, closingStale, closingIdle, source)

		typed := byKind(records, record.KindSkill)
		if len(typed) != 3 {
			t.Fatalf("skill records = %d, want 3 (one per occurrence, ADR-0036 §3) (result = %+v)", len(typed), result)
		}
		child := onlyOfKind(t, records, record.KindBuiltinTool)
		if want := sessionParent("session-1"); child.ParentEventID != want {
			t.Fatalf("child ParentEventID = %q, want the session span %q", child.ParentEventID, want)
		}
		for _, sibling := range typed {
			if child.ParentEventID == sibling.EventID {
				t.Fatal("child parented onto a sibling typed record")
			}
		}
	})

	t.Run("a tag and a tool_use for one name", func(t *testing.T) {
		source := strings.Join([]string{
			typedTurn("typed-1", "pr-review", "2026-08-13T12:00:00Z"),
			skillCall("skill-1", "session-1", "2026-08-13T12:00:01Z", "call-skill", "pr-review"),
			attributedToolCall("call-entry", "session-1", "2026-08-13T12:00:02Z", "call-1", "Bash", "pr-review"),
		}, "\n")

		records, result := twoSources(t, closingStale, closingIdle, source)

		if skills := byKind(records, record.KindSkill); len(skills) != 2 {
			t.Fatalf("skill records = %d, want 2 (two real events, ADR-0036 §4) (result = %+v)", len(skills), result)
		}
		child := onlyOfKind(t, records, record.KindBuiltinTool)
		if want := sessionParent("session-1"); child.ParentEventID != want {
			t.Fatalf("child ParentEventID = %q, want the session span %q", child.ParentEventID, want)
		}
	})
}

// TestAnUnattributedPrimitiveParentsOntoTheSession is AC 4: no attribution is the
// normal shape of a top-level call, and case 3 is the rule for it — not omission.
func TestAnUnattributedPrimitiveParentsOntoTheSession(t *testing.T) {
	source := strings.Join([]string{
		toolCallLines("entry-1", "session-1", "2026-08-13T12:00:00Z", "call-1", "Bash", `{}`),
		toolCallLines("entry-2", "session-1", "2026-08-13T12:00:01Z", "call-2", "mcp__atlassian__search", `{}`),
	}, "\n")

	records, _ := twoSources(t, closingStale, closingIdle, source)

	want := sessionParent("session-1")
	for _, kind := range []record.Kind{record.KindBuiltinTool, record.KindMCPTool} {
		event := onlyOfKind(t, records, kind)
		if event.ParentEventID == "" {
			t.Errorf("%s ParentEventID is empty: an unattributed call must not render as a trace root", kind)
		}
		if event.ParentEventID != want {
			t.Errorf("%s ParentEventID = %q, want the session span %q", kind, event.ParentEventID, want)
		}
	}
}

// TestASkillAttributedChildMatchesTheShapeItsSkillRecordCarries is AC 5: the parent
// is whichever shape the skill's own invocation record took, so the link lands on a
// record that exists rather than on a shape the resolution did not choose.
func TestASkillAttributedChildMatchesTheShapeItsSkillRecordCarries(t *testing.T) {
	child := attributedToolCall("call-entry", "session-1", "2026-08-13T12:00:05Z", "call-1", "Bash", "pr-review")

	t.Run("typed_invocation", func(t *testing.T) {
		source := strings.Join([]string{
			typedTurn("typed-1", "pr-review", "2026-08-13T12:00:00Z"),
			child,
		}, "\n")

		records, _ := twoSources(t, closingStale, closingIdle, source)

		want := record.DeriveEventID(harness, typedSourceEvent("typed-1"))
		event := onlyOfKind(t, records, record.KindBuiltinTool)
		if event.ParentEventID != want {
			t.Fatalf("ParentEventID = %q, want the typed record %q", event.ParentEventID, want)
		}
	})

	t.Run("model_invoked_tool_use", func(t *testing.T) {
		source := strings.Join([]string{
			skillCall("skill-1", "session-1", "2026-08-13T12:00:00Z", "call-skill", "pr-review"),
			child,
		}, "\n")

		records, _ := twoSources(t, closingStale, closingIdle, source)

		want := record.DeriveEventID(harness, callSourceEvent("skill-1", "call-skill"))
		event := onlyOfKind(t, records, record.KindBuiltinTool)
		if event.ParentEventID != want {
			t.Fatalf("ParentEventID = %q, want the Skill tool_use record %q", event.ParentEventID, want)
		}
	})

	t.Run("shape_a_fallback", func(t *testing.T) {
		source := strings.Join([]string{
			attributedRun("fallback-1", "session-1", "2026-08-13T12:00:00Z", "pr-review"),
			child,
		}, "\n")

		records, _ := twoSources(t, closingStale, closingIdle, source)

		want := record.DeriveEventID(harness, record.Identifier("fallback-1"))
		event := onlyOfKind(t, records, record.KindBuiltinTool)
		if event.ParentEventID != want {
			t.Fatalf("ParentEventID = %q, want the Shape-A fallback %q", event.ParentEventID, want)
		}
	})
}

// TestParentLinksAreIdenticalOnEveryScanAndInEverySourceOrder is AC 6. The
// derivation takes no clock, no scan order, no file order and no cursor, so
// re-delivering the same history produces the same links (ADR-0004, C13).
func TestParentLinksAreIdenticalOnEveryScanAndInEverySourceOrder(t *testing.T) {
	parent, subagent := threeLevelChain()

	first, _ := twoSources(t, closingStale, closingIdle, parent, subagent)
	second, _ := twoSources(t, closingStale, closingIdle, parent, subagent)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two scans of one history produced different records")
	}

	reversed, _ := twoSources(t, closingStale, closingIdle, subagent, parent)
	links := func(records []record.Record) map[record.Hash]record.Hash {
		found := map[record.Hash]record.Hash{}
		for _, event := range records {
			found[event.EventID] = event.ParentEventID
		}
		return found
	}
	if !reflect.DeepEqual(links(first), links(reversed)) {
		t.Fatalf("parent links depend on source order:\n%v\n%v", links(first), links(reversed))
	}
}

// TestNoParentChainFormsACycle is AC 7, established over the records rather than by
// a runtime ancestor check — record.Validate stays per-record and pure, which
// TestValidateDoesNotRejectASelfParent in internal/record pins from the other side.
func TestNoParentChainFormsACycle(t *testing.T) {
	parent, subagent := threeLevelChain()
	attributed := strings.Join([]string{
		typedTurn("typed-1", "pr-review", "2026-08-13T12:00:00Z"),
		skillCall("skill-1", "session-2", "2026-08-13T12:00:01Z", "call-skill", "run-sdlc"),
		attributedToolCall("call-entry", "session-2", "2026-08-13T12:00:02Z", "call-1", "Bash", "run-sdlc"),
	}, "\n")

	records, _ := twoSources(t, closingStale, closingIdle, parent, subagent, attributed)

	byID := map[record.Hash]record.Record{}
	for _, event := range records {
		byID[event.EventID] = event
	}
	for _, start := range records {
		seen := map[record.Hash]struct{}{start.EventID: {}}
		at := start
		for hop := 0; at.ParentEventID != ""; hop++ {
			if hop > len(records) {
				t.Fatalf("walk from %q did not terminate in %d hops", start.EventID, len(records))
			}
			if _, revisited := seen[at.ParentEventID]; revisited {
				t.Fatalf("walk from %q revisited %q: the chain is a cycle", start.EventID, at.ParentEventID)
			}
			seen[at.ParentEventID] = struct{}{}
			next, exists := byID[at.ParentEventID]
			if !exists {
				// A parent whose record this walk did not emit is the session_end of a
				// session still open, which is a later scan's record and not a cycle.
				break
			}
			at = next
		}
	}
}

// TestASelfAttributedSkillCallParentsOntoTheSessionNotItself is AC 7's one reachable
// self-match (C10): a Skill tool_use whose own entry carries the same
// attributionSkill, so case 2 would resolve onto its own EventID.
func TestASelfAttributedSkillCallParentsOntoTheSessionNotItself(t *testing.T) {
	source := strings.Join([]string{
		fmt.Sprintf(
			`{"uuid":"skill-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z",`+
				`"version":"1.0.0","entrypoint":"cli","attributionSkill":"pr-review",`+
				`"message":{"model":"sonnet","id":"skill-1-msg","content":[{"type":"tool_use","id":%q,`+
				`"name":"Skill","input":{"skill":"pr-review"}}]}}`, "call-skill"),
		`{"uuid":"skill-1-result","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z",` +
			`"entrypoint":"cli","message":{"content":[{"type":"tool_result","tool_use_id":"call-skill","is_error":false}]}}`,
	}, "\n")

	records, result := twoSources(t, closingStale, closingIdle, source)

	skills := byKind(records, record.KindSkill)
	if len(skills) != 1 {
		t.Fatalf("skill records = %d, want 1 (result = %+v)", len(skills), result)
	}
	event := skills[0]
	if event.ViaSkill != event.Name {
		t.Fatalf("fixture did not build the self-match: ViaSkill = %q, Name = %q", event.ViaSkill, event.Name)
	}
	if event.ParentEventID == event.EventID {
		t.Fatal("a record was parented onto itself")
	}
	if want := sessionParent("session-1"); event.ParentEventID != want {
		t.Fatalf("ParentEventID = %q, want the session span %q", event.ParentEventID, want)
	}
}

// TestAChildOfARefusedSubagentRunFallsThroughToTheSession is C4 / ADR-0035 §6: a run
// with no usable name emits no record and never will, so a case-1 link would point at
// a record that cannot exist.
func TestAChildOfARefusedSubagentRunFallsThroughToTheSession(t *testing.T) {
	// The same shape subagentToolCall builds, minus attributionAgent anywhere — the
	// 2% ADR-0036 §2 measured.
	source := strings.Join([]string{
		`{"uuid":"agent-1-anchor","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z",` +
			`"version":"1.0.0","entrypoint":"cli","agentId":"agent-1","isSidechain":true,` +
			`"message":{"model":"sonnet","id":"agent-1-anchor-msg","content":[]}}`,
		`{"uuid":"agent-1-call","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z",` +
			`"version":"1.0.0","entrypoint":"cli","agentId":"agent-1","isSidechain":true,` +
			`"message":{"model":"sonnet","id":"agent-1-call-msg","content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{}}]}}`,
		`{"uuid":"agent-1-result","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z",` +
			`"entrypoint":"cli","agentId":"agent-1","isSidechain":true,` +
			`"message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")

	records, result := twoSources(t, closingStale, closingIdle, source)

	if result.RefusedSubagentRuns != 1 {
		t.Fatalf("RefusedSubagentRuns = %d, want 1 (result = %+v)", result.RefusedSubagentRuns, result)
	}
	if runs := byKind(records, record.KindSubagent); len(runs) != 0 {
		t.Fatalf("subagent records = %d, want 0: the run was refused", len(runs))
	}
	child := onlyOfKind(t, records, record.KindBuiltinTool)
	forbidden := record.DeriveEventID(harness, subagentSourceEvent("agent-1"))
	if child.ParentEventID == forbidden {
		t.Fatal("child references the refused run's record, which no scan will ever emit")
	}
	if want := sessionParent("session-1"); child.ParentEventID != want {
		t.Fatalf("ParentEventID = %q, want the session span %q", child.ParentEventID, want)
	}
}

// TestCaseOneOutranksCaseTwo pins the precedence itself: a child inside a subagent
// transcript whose entry also carries an unambiguous skill attribution takes the
// subagent, because case 1 is first.
func TestCaseOneOutranksCaseTwo(t *testing.T) {
	parent := strings.Join([]string{
		skillCall("parent-1", "session-1", "2026-08-13T12:00:00Z", "call-skill", "run-sdlc"),
		assistantLine("parent-2", "session-1", "2026-08-13T12:00:01Z", "msg_parent", realUsage),
	}, "\n")
	subagent := subagentToolCall("agent-1", "sdlc-plan", "session-1", "2026-08-13T12:00:02Z",
		"call-1", "Bash", "run-sdlc")

	records, _ := twoSources(t, closingStale, closingIdle, parent, subagent)

	child := onlyOfKind(t, records, record.KindBuiltinTool)
	if child.ViaSkill == "" {
		t.Fatal("fixture did not build the shape: the child carries no skill attribution")
	}
	wantSubagent := record.DeriveEventID(harness, subagentSourceEvent("agent-1"))
	if child.ParentEventID != wantSubagent {
		t.Fatalf("ParentEventID = %q, want the subagent record %q", child.ParentEventID, wantSubagent)
	}
	skill := onlyOfKind(t, records, record.KindSkill)
	if child.ParentEventID == skill.EventID {
		t.Fatal("case 2 outranked case 1")
	}
}

// TestAHostileAgentIDIsNotACaseOneParent is this adapter's hostile-payload corpus
// obligation for the new field (ADR-0007). An agentId outside the token domain is a
// clean zero on both sides — the run is not observed and the child is not case 1 —
// and the value reaches no record field, no counter and no error.
func TestAHostileAgentIDIsNotACaseOneParent(t *testing.T) {
	for _, value := range hostileValues {
		source := strings.Join([]string{
			fmt.Sprintf(
				`{"uuid":"agent-1-anchor","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z",`+
					`"version":"1.0.0","entrypoint":"cli","agentId":%s,"attributionAgent":"sdlc-plan","isSidechain":true,`+
					`"message":{"model":"sonnet","id":"agent-1-anchor-msg","content":[]}}`, quoted(t, value)),
			fmt.Sprintf(
				`{"uuid":"agent-1-call","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z",`+
					`"version":"1.0.0","entrypoint":"cli","agentId":%s,"attributionAgent":"sdlc-plan","isSidechain":true,`+
					`"message":{"model":"sonnet","id":"agent-1-call-msg",`+
					`"content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{}}]}}`, quoted(t, value)),
			`{"uuid":"agent-1-result","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z",` +
				`"entrypoint":"cli","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
		}, "\n")

		records, _ := twoSources(t, closingStale, closingIdle, source)

		forbidden := record.DeriveEventID(harness, subagentSourceEvent(record.Identifier(value)))
		for _, event := range records {
			if event.ParentEventID == forbidden {
				t.Errorf("agentId %q became a case-1 parent id", value)
			}
			encoded, err := record.Marshal(event)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if value != "" && strings.Contains(string(encoded), value) {
				t.Errorf("record carries the hostile agentId %q: %s", value, encoded)
			}
		}
		child := onlyOfKind(t, records, record.KindBuiltinTool)
		if want := sessionParent("session-1"); child.ParentEventID != want {
			t.Errorf("agentId %q: ParentEventID = %q, want the session span %q", value, child.ParentEventID, want)
		}
	}
}

// TestADeferredChildOfAnOpenSessionIsNotEmitted pins ADR-0035's accepted
// consequence, so it cannot regress into a guess: a skill-attributed child of a
// session that is still running is not emitted at all this scan, rather than emitted
// with a fabricated or empty parent. The next scan re-derives it — there is no
// incremental cursor, and SourceFloor already refuses to pass an open session.
func TestADeferredChildOfAnOpenSessionIsNotEmitted(t *testing.T) {
	source := attributedToolCall("call-entry", "session-1", "2026-08-13T12:00:00Z", "call-1", "Bash", "pr-review")
	open := Staleness{Timeout: time.Hour, Now: callInstant.Add(10 * time.Minute)}

	records, result := twoSources(t, open, Idleness{}, source)

	if len(records) != 0 {
		t.Fatalf("records = %+v, want none while the session may still be running", records)
	}
	if result.OpenSessions != 1 {
		t.Errorf("OpenSessions = %d, want 1", result.OpenSessions)
	}
}

// TestScanCreditsASourceWhoseOnlyContributionWasADeferredChild is the counter
// regression the routing change could plausibly introduce: a source whose only record
// is deferred to Close must still not be reported as having produced nothing.
func TestScanCreditsASourceWhoseOnlyContributionWasADeferredChild(t *testing.T) {
	source := attributedToolCall("call-entry", "session-1", "2026-08-13T12:00:00Z", "call-1", "Bash", "pr-review")

	records, result := twoSources(t, closingStale, closingIdle, source)

	if result.SkippedSources != 0 {
		t.Errorf("SkippedSources = %d, want 0: the source's record was credited at Close", result.SkippedSources)
	}
	if len(byKind(records, record.KindBuiltinTool)) != 1 {
		t.Fatalf("builtin_tool records = %d, want 1", len(byKind(records, record.KindBuiltinTool)))
	}
}
