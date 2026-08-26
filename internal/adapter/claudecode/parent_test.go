package claudecode

import (
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
