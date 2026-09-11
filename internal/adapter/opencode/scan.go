package opencode

import (
	"cmp"
	"slices"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// Scan is one opencode collection pass. Sessions are registered first, then parts
// are offered in any order; Close resolves the walk once.
//
// The carry is empty by construction, and that is a decision rather than an
// omission. ADR-0049's pending carry is load-bearing for the first adapter
// because the entries behind it are declined rather than re-derived. This reader
// declines none: its caller re-reads the whole part table on every scan with no
// persisted cursor, so a part buffered by one scan is re-read and re-judged by the
// next. There is therefore nothing to carry, no pending section to add and no
// pending version to bump. That holds only while the reader has no cursor — a
// future cursor would make the carry due, and this comment is where that is
// recorded.
type Scan struct {
	resolve Resolver
	servers Servers
	stale   Staleness
	idle    Idleness

	sessions map[string]Session
	// buffered holds every part with no terminal status yet, in arrival order.
	// ADR-0015 forbids emitting them and forbids advancing a cursor past them.
	buffered []ToolPart
	// contributed names the sessions that produced at least one record, so Close
	// can report how many yielded nothing.
	contributed map[string]struct{}

	records []record.Record
	result  Result
}

// NewScan starts one walk. Every capability it needs about the machine — the
// consent answer, the configured servers, both thresholds — arrives here as a
// value, because derivation may not read the filesystem (ADR-0019 §1).
func NewScan(resolve Resolver, servers Servers, stale Staleness, idle Idleness) *Scan {
	return &Scan{
		resolve:     resolve,
		servers:     servers,
		stale:       stale,
		idle:        idle,
		sessions:    map[string]Session{},
		contributed: map[string]struct{}{},
	}
}

// Harness is the slug every record this scan derives carries, so a caller folding
// per-harness diagnostics never has to hold it beside the reader.
func (s *Scan) Harness() record.Identifier { return harness }

// Session registers one session's directory, version and totals.
func (s *Scan) Session(registered Session) { s.sessions[registered.ID] = registered }

// Part offers one tool part. A part whose status is terminal derives its record
// immediately; one that is not is buffered (ADR-0015) and resolved by Close.
//
// A part whose session was never registered derives nothing and is counted as
// refused: its directory is unknown, so its consent is unknown, and an unknown
// consent is a refusal rather than an assumption (fail closed).
func (s *Scan) Part(part ToolPart) {
	from, registered := s.sessions[part.SessionID]
	if !registered {
		s.result.Refused++
		return
	}
	outcome, terminal, known := statusOutcome(part.Status)
	if !known {
		s.result.UnknownOutcomes++
	}
	if !terminal {
		s.buffered = append(s.buffered, part)
		return
	}
	duration, outOfOrder := toolDuration(part)
	if outOfOrder {
		s.result.OutOfOrderPairs++
	}
	s.emit(part, from, outcome, duration)
}

// Buffered is how many parts this scan is holding unterminated. It is the count
// ADR-0015 forbids emitting and forbids advancing a cursor past.
func (s *Scan) Buffered() int { return len(s.buffered) }

// Close resolves the walk once: the buffered parts the staleness rule gives up
// on, and one session_end per finished session.
//
// Order is fixed rather than incidental — parts in arrival order, then the
// interrupted ones by part id, then the session grain by session id. Two scans
// over the same rows have to produce byte-identical store contents (ADR-0004),
// and map iteration order is randomised.
func (s *Scan) Close() Result {
	s.resolveStaleParts()
	s.resolveFinishedSessions()

	s.result.Records = s.records
	for id := range s.sessions {
		if _, yielded := s.contributed[id]; !yielded {
			s.result.SkippedSources++
		}
	}
	return s.result
}

// resolveStaleParts emits the buffered parts whose session has gone quiet past the
// staleness threshold, through the same derivation the completed record would have
// used — so the id it carries is the id a later result would derive, and the
// duplicate is deduplicated away rather than upserted (ADR-0004, ADR-0015).
//
// A part with an unknown status is never given up on this way. An unrecognised
// status may be terminal, so writing "interrupted" for it would be a permanent
// wrong record; it stays buffered and is counted as blindness instead.
func (s *Scan) resolveStaleParts() {
	stale := make([]ToolPart, 0, len(s.buffered))
	for _, part := range s.buffered {
		if _, _, known := statusOutcome(part.Status); !known {
			continue
		}
		from := s.sessions[part.SessionID]
		// Strictly greater, matching the first adapter's rule: a session silent for
		// exactly the threshold is still open, which errs toward not writing a
		// record that cannot be taken back.
		if s.stale.Enabled() && s.stale.Now.Sub(time.UnixMilli(from.UpdatedMS)) > s.stale.Timeout {
			stale = append(stale, part)
			continue
		}
		s.result.Pending++
	}
	slices.SortFunc(stale, func(a, b ToolPart) int { return cmp.Compare(a.ID, b.ID) })
	for _, part := range stale {
		from := s.sessions[part.SessionID]
		before := len(s.records)
		s.emit(part, from, record.OutcomeInterrupted, nil)
		if len(s.records) > before {
			s.result.Interrupted++
		}
	}
}

// emit derives one invocation record and files it, or files what the attempt cost.
// Consent is asked once per record, with the record's own instant, so the caller's
// two-dimensional answer decides every row (ADR-0025).
func (s *Scan) emit(part ToolPart, from Session, outcome record.Outcome, duration *int64) {
	at := time.UnixMilli(part.StartMS).UTC()
	repo, consented := s.resolve(from.Directory, at)
	if !consented {
		return
	}
	derived := invocation(part, from, repo, s.servers, outcome, duration)
	if derived.refused {
		s.result.Refused++
		return
	}
	s.file(from.ID, derived.record)
}

// file appends one derived record and credits the session that produced it.
func (s *Scan) file(sessionID string, derived record.Record) {
	s.records = append(s.records, derived)
	s.contributed[sessionID] = struct{}{}
}
