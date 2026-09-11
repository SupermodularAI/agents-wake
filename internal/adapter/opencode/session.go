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
		derived := sessionEnd(registered, s.resolve)
		if derived.refused {
			s.result.Refused++
			continue
		}
		if derived.record.EventID == "" {
			continue
		}
		s.file(id, derived.record)
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
