package claudecode

import (
	"cmp"
	"slices"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// sessionParent is ADR-0035 §2's case 3: the session's own session_end record, which
// every primitive of that session reaches when neither of the two nearer cases
// applies. It is pure from (harness, session id) through the same
// sessionEndSourceEvent the session_end itself derives from, so it is the right id
// whether or not that record has been written yet (ADR-0035 §4) — which is what lets
// a top-level call be parented the instant it is derived rather than left rootless.
//
// It is the rule for a top-level primitive, not a fallback to omission: nothing is
// left rootless except the session_end itself, which is the trace root.
func sessionParent(sessionID record.Identifier) record.Hash {
	return record.DeriveEventID(harness, sessionEndSourceEvent(sessionID))
}

// skillTarget is what one (session, skill name) pair resolved to across the records
// this walk derived: the first record's id, and how many records carried the pair.
//
// The count is the whole point. ADR-0036 §3 counts a typed invocation once per
// occurrence and §4 treats a command tag and a Skill tool_use for one name as two
// real events, so a name legitimately resolves to more than one record — and picking
// one of them for a child would be a guess that no later scan can take back
// (ADR-0015 rejects upsert).
type skillTarget struct {
	id    record.Hash
	count int
}

// skillTargets indexes the invocation records this walk derived, per skillRun. It is
// a projection of the record set the walk actually emits, which is what makes
// ADR-0035 §6's "never reference a record whose existence is unconfirmed" true by
// construction rather than by inspection.
//
// It is not a second resolution of ADR-0023's question. skillsInvoked and typedRuns
// keep their current shape and their current job — the matched-drop and ADR-0036 §4's
// narrowing — and resolveSessionSkills decides exactly what it decided before. This
// observes the records that determination produces (ADR-0035 §3).
//
// It holds bounded ids and a count, no name from a path and no transcript content
// (ADR-0007).
type skillTargets map[skillRun]skillTarget

// observe registers one derived record as a possible case-2 parent, if its kind is
// one a ViaSkill attribution can point at.
//
// Skill and command both, because the inventory decides which of the two a typed
// name resolved to and the attribution carries only the name — the same rule
// typedRuns already applies at its own registration site. The id is kept from the
// first sight and the count incremented on every one, so an ambiguous pair is
// distinguishable from a resolved one without retaining every id.
//
// Called exactly once per derived record, the contract invocationTally.observe
// already has: counting one record twice would report a spurious ambiguity and drop
// a child to the session span.
func (t skillTargets) observe(event record.Record) {
	if event.Kind != record.KindSkill && event.Kind != record.KindCommand {
		return
	}
	key := skillRun{session: event.SessionID, name: event.Name}
	target, seen := t[key]
	if !seen {
		target.id = event.EventID
	}
	target.count++
	t[key] = target
}

// target answers case 2 for one pair: the id when exactly one record exists, and
// false for zero or for more than one.
//
// More than one drops to case 3 rather than selecting a sibling (ADR-0035 §3 as
// amended, ADR-0036 §3, §4), and zero drops there too — a name with no record behind
// it has no parent to point at, and fabricating one is what ADR-0035 §6 forbids.
func (t skillTargets) target(key skillRun) (record.Hash, bool) {
	resolved, seen := t[key]
	if !seen || resolved.count != 1 {
		return "", false
	}
	return resolved.id, true
}

// deferredChild is one terminal record whose parent ADR-0035 §2 could not answer from
// its own entry: a record derived inside a subagent transcript, or one attributed to
// a skill. Both need the walk's final view — whether the subagent run resolved, and
// how many records the skill name landed on — so the record waits here and is emitted
// once with its parent already set (ADR-0015 rejects upsert).
//
// It holds the record, the source ordinal that record must be credited to, and the
// agent id its entry declared. The agent id is consumed into a derived parent id and
// never persisted: no record field carries it (ADR-0007).
type deferredChild struct {
	event   record.Record
	source  int
	agentID record.Identifier
}

// parentage is everything the precedence resolves against, gathered once at Close.
//
// subagents and buffered are the two halves C4 turns on: a run this walk resolved and
// emitted a record for, against one it has not reached. A run in neither map was
// resolved and refused, and its record will never exist — so a child of it falls
// through the precedence rather than pointing at an id no record has.
type parentage struct {
	// subagents maps each agent id whose subagent record this walk emitted to that
	// record's own EventID. It is supplied by resolveSubagentRuns rather than
	// re-derived here, so a child's link is provably the same value the parent record
	// carries.
	subagents map[record.Identifier]record.Hash
	// buffered is the runs this walk did not resolve — session still open, or never
	// anchored. Their records will exist on a later scan, so a child of one is
	// deferred rather than reparented.
	buffered map[record.Identifier]*subagentRun
	skills   skillTargets
}

// parentOf applies ADR-0035 §2's ordered precedence to one deferred child.
//
// The second return is false when the target is knowable but not yet resolved, which
// means "not this scan" rather than "no parent": the child is re-derived by the next
// scan, exactly as a Shape-A candidate of an open session already is.
//
// The order is the ADR's and no other. Case 1 keys on the agent id the transcript
// declared, never on a ViaAgent name and never on a lookup (ADR-0035 §1, ADR-0036
// §2), so a repeated agent name resolves without ambiguity however many times it was
// invoked. Case 2's self-match — a Skill tool_use record whose own entry carries an
// attributionSkill equal to its own name — falls through to case 3 by a pure
// comparison against the child's own EventID, which is how ADR-0035 §7's
// cycle-freedom is established without a stateful ancestor walk.
func (p parentage) parentOf(child deferredChild) (record.Hash, bool) {
	if child.agentID != "" {
		if id, emitted := p.subagents[child.agentID]; emitted {
			return id, true
		}
		if _, waiting := p.buffered[child.agentID]; waiting {
			return "", false
		}
		// Resolved and refused: !run.declared, the 2% of real transcripts ADR-0036 §2
		// measured. No record was emitted and the refusal repeats on every scan, so a
		// case-1 link here would reference a record that will never exist. §6's own rule
		// applies — fall back to the next known-to-exist ancestor.
	}
	if child.event.ViaSkill != "" {
		if id, resolved := p.skills.target(skillRun{
			session: child.event.SessionID,
			name:    child.event.ViaSkill,
		}); resolved && id != child.event.EventID {
			return id, true
		}
	}
	return sessionParent(child.event.SessionID), true
}

// resolveDeferredChildren emits every deferred child whose session has closed and
// whose parent resolved, with the parent already set on the record.
//
// Session close is the gate ADR-0035's own title names, and it is required rather
// than cautious: the ambiguity rule needs a final count of the invocation records for
// a (session, name) pair, and a count taken while the session is still running can
// grow on a later scan — which would leave a child permanently parented onto a
// sibling that the finished session says is ambiguous. It uses SessionState.Closed,
// the one existing predicate, and introduces no second threshold (ADR-0023 §3).
//
// The order is sorted rather than the slice's arrival order, for the reason
// resolveStaleCalls gives: two scans of one walk have to produce byte-identical store
// contents (ADR-0004). The key is the record's own (timestamp, event id), a total
// order because one source event yields at most one record so no two records share an
// event id.
//
// It returns derivations rather than bare records so each can be credited to the
// source it came from — the distinction doctor's Skipped counter rests on for a source
// whose only contribution was a deferred child.
func resolveDeferredChildren(deferred []deferredChild, sessions *SessionState,
	stale Staleness, targets parentage) []derivation {
	type resolvedChild struct {
		event  record.Record
		source int
	}
	resolved := make([]resolvedChild, 0, len(deferred))
	for _, child := range deferred {
		if !sessions.Closed(child.event.SessionID, stale) {
			continue
		}
		parent, known := targets.parentOf(child)
		if !known {
			continue
		}
		event := child.event
		event.ParentEventID = parent
		resolved = append(resolved, resolvedChild{event: event, source: child.source})
	}
	slices.SortFunc(resolved, func(a, b resolvedChild) int {
		return cmp.Or(
			a.event.Timestamp.Compare(b.event.Timestamp),
			cmp.Compare(string(a.event.EventID), string(b.event.EventID)),
		)
	})
	records := make([]derivation, 0, len(resolved))
	for _, child := range resolved {
		records = append(records, derivation{event: child.event, source: child.source})
	}
	return records
}
