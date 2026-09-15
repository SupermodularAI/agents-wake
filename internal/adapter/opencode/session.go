package opencode

import (
	"slices"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// sessionSeparator delimits the two halves of a session_end's source identity. It
// is the same byte, and the same rule, the first adapter uses: no token-domain
// value can contain it, so a session_end id cannot collide with an invocation's —
// which here is a bare part.id carrying no separator at all.
const sessionSeparator = "\x1e"

// sessionName is the Name a session-grain row carries. record.Validate requires a
// non-empty name-domain identifier and a session has no primitive name, so this is
// Wake's own constant for the row: it is not derived from the store and carries
// nothing of it.
const sessionName = record.Identifier("session")

// sessionEndSourceEvent identifies the one session_end a session id ever produces:
// the session id, plus a kind-discriminating component. Never a sequence number —
// ADR-0034 §1 settles that there is never more than one.
func sessionEndSourceEvent(sessionID record.Identifier) record.Identifier {
	return record.Identifier(string(sessionID) + sessionSeparator + string(record.KindSessionEnd))
}

// subagentSeparator delimits the two halves of a subagent invocation's source
// identity. It is a distinct byte from the session grain's on purpose: both ids
// are composed from the same session id, and a kind-discriminating component is
// what ADR-0034 §1 licenses to keep two records derived off one tuple structurally
// disjoint. It is the same byte, and the same rule, the first adapter uses.
const subagentSeparator = "\x1d"

// subagentSourceEvent identifies the one subagent invocation a child session ever
// produces: the child session's own id, plus a kind-discriminating component.
//
// The child session row is the canonical source event (ADR-0036 §1) and its id is
// the analogue of Claude Code's agentId — never the invoking part's id, which is
// the same logical event seen from the other side.
func subagentSourceEvent(sessionID record.Identifier) record.Identifier {
	return record.Identifier(string(sessionID) + subagentSeparator + string(record.KindSubagent))
}

// resolveFinishedSessions derives one session_end per session whose last activity
// is further back than the idle threshold, in ascending session-id order.
//
// Sorted rather than map order, for the reason Close states: two scans over the
// same rows have to produce byte-identical store contents, and a session id is
// unique per row so the order is total.
//
// A session the harness recorded no last activity for is refused and counted
// rather than judged. Its instant is both the comparison's input and the record's
// whole timestamp, so substituting one would call the session finished the moment
// it was read and stamp the result at 1970 — a record nothing measured, and one
// ADR-0004's derived id makes permanent. Blindness counted is what doctor needs to
// see (plan §3.3, §12).
func (s *Scan) resolveFinishedSessions() {
	if !s.idle.Enabled() {
		return
	}
	finished := make([]string, 0, len(s.sessions))
	for id, registered := range s.sessions {
		// Refused once for the whole row, and neither grain is due: a session with
		// no last-activity instant is one this walk cannot judge finished at all,
		// so the subagent invocation it might have carried is not reached either.
		if !registered.HasUpdated {
			s.result.Refused++
			continue
		}
		// Strictly greater than the threshold, matching the staleness rule's own
		// comparison: a session silent for exactly the threshold is still open.
		if s.idle.Now.Sub(time.UnixMilli(registered.UpdatedMS)) > s.idle.Timeout {
			finished = append(finished, id)
		}
	}
	slices.Sort(finished)
	for _, id := range finished {
		registered := s.sessions[id]
		switch derived := sessionEnd(registered, s.resolve); {
		case derived.refused:
			s.result.Refused++
		case derived.record.EventID == "":
		default:
			s.file(id, derived.record)
		}
		// The grain-1 subagent record resolves in the same pass, off the same row,
		// at the same idle threshold. No second threshold is introduced (ADR-0023
		// §3) and no cursor: a cursor is what would make ADR-0049's carry due, and
		// this walk re-reads every session and part row on every scan.
		//
		// Two grains off one row is expected symmetry, not duplication: ADR-0002's
		// grains answer different questions from the same underlying event, and
		// ADR-0034 §1's kind-discriminating component keeps the two ids disjoint.
		//
		// Order within one session id is fixed rather than incidental — the session
		// grain, then the invocation — for the reason Close states: two scans over
		// the same rows have to produce byte-identical store contents (ADR-0004).
		switch run := subagentInvocation(registered, s.resolve, s.names); {
		case run.refused:
			s.result.Refused++
		case run.record.EventID == "":
		default:
			s.file(id, run.record)
		}
	}
}

// sessionEnd derives the session grain for one finished session.
//
// The five totals are opencode's own integers, copied and never scaled: this
// harness pre-totals them, so nothing here estimates and no code path in this
// package computes bytes ÷ 4 (plan §2.6). ToolCalls and BuiltinToolCalls stay nil
// because opencode pre-totals neither, and nil means "the harness reported
// nothing", which is never 0.
//
// A session whose directory is outside consent derives nothing at all, and that
// is not a refusal: it is an honest zero, judged by the caller's resolver like
// every invocation row.
func sessionEnd(from Session, resolve adapter.Resolver) derivation {
	at := time.UnixMilli(from.UpdatedMS).UTC()
	repo, consented := resolve(from.Directory, at)
	if !consented {
		return derivation{}
	}
	sessionID, err := record.BoundedToken(from.ID)
	if err != nil {
		return derivation{refused: true}
	}
	input, output := from.TokensInput, from.TokensOutput
	reasoning, cacheRead, cacheWrite := from.TokensReasoning, from.TokensCacheRead, from.TokensCacheWrite
	derived := record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID(harness, sessionEndSourceEvent(sessionID)),
		Timestamp:     record.NormalizedTimestamp(at),
		Harness:       harness,
		SessionID:     sessionID,
		Repo:          repo,
		Kind:          record.KindSessionEnd,
		Name:          sessionName,
		// Nobody invoked the end of a session; it is inferred from silence.
		Invoker:             record.InvokerAuto,
		InputTokens:         &input,
		OutputTokens:        &output,
		ThinkingTokens:      &reasoning,
		CacheReadTokens:     &cacheRead,
		CacheCreationTokens: &cacheWrite,
	}
	if version, err := record.BoundedVersion(from.Version); err == nil {
		derived.HarnessVersion = version
	}
	return finish(derived)
}

// subagentInvocation derives the grain-1 record for one finished child session.
//
// The canonical source event is the child session row itself (ADR-0036 §1): the
// harness's own record of what ran. Every dimension comes from that row and from
// nothing else — §5 declines correlating the invoking side at all, in either
// direction — so nothing here reads the parent session, and a parent this walk
// never registered changes nothing.
//
// parent_id is the gate and agent is the name. Measured on a real store: 79 of 115
// sessions carry a parent and all 79 declare an agent, while 36 top-level sessions
// declare an agent with no parent — so gating on the agent would fabricate 36
// subagent invocations out of ordinary sessions. ParentID is read as a gate and
// never persisted, which is why it needs no domain validation: it reaches no field.
//
// Outcome stays nil. ADR-0036 §2: ok is never derived for a subagent, from either
// side, and absence stays nil. opencode carries no analogue of the structured
// failure marker that is Claude Code's one exception — state.status reads
// "completed" on every task part and the session table carries no status or error
// column — so anything else would be the guess ADR-0005 forbids. DurationMS is nil
// for the reason the first adapter's subagent record states: nil means the harness
// reported nothing, and ADR-0015's no-upsert store makes the other direction
// permanent, so the conservative one is what a later schema bump can still widen.
//
// The name goes through DerivedName rather than BoundedIdentifier for the reason
// invocation states: the name domain admits ':', and a value already wearing the
// keyed scope digest's shape must be refused verbatim (ADR-0020).
//
// A directory outside consent derives nothing at all, and that is not a refusal —
// it is the honest zero sessionEnd already gives for the same row.
func subagentInvocation(from Session, resolve adapter.Resolver, names record.Namer) derivation {
	if from.ParentID == "" {
		return derivation{}
	}
	// The instant first, and before consent is asked: it is what the record is
	// stamped with and what consent is judged at, so substituting one would put a
	// number nothing measured into both. It is the rule Scan.Part already gives a
	// part with no start instant.
	if !from.HasCreated {
		return derivation{refused: true}
	}
	at := time.UnixMilli(from.CreatedMS).UTC()
	repo, consented := resolve(from.Directory, at)
	if !consented {
		return derivation{}
	}
	sessionID, err := record.BoundedToken(from.ID)
	if err != nil {
		return derivation{refused: true}
	}
	name, err := names.DerivedName(from.Agent)
	if err != nil {
		return derivation{refused: true}
	}
	derived := record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID(harness, subagentSourceEvent(sessionID)),
		Timestamp:     record.NormalizedTimestamp(at),
		Harness:       harness,
		SessionID:     sessionID,
		Repo:          repo,
		Kind:          record.KindSubagent,
		Name:          name,
		// A subagent run is entered by the model, never typed by the user.
		Invoker: record.InvokerModel,
	}
	if version, err := record.BoundedVersion(from.Version); err == nil {
		derived.HarnessVersion = version
	}
	return finish(derived)
}
