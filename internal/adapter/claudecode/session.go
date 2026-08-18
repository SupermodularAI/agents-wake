package claudecode

import (
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// Staleness carries ADR-0015's staleness rule into one scan: the threshold, and
// the clock the comparison is made against.
//
// One threshold, deliberately. ADR-0023 §3 requires a session to close under "the
// exact staleness rule ADR-0015 already defines ... no second threshold is
// introduced", so Timeout is scan.stale_call_timeout — the key ADR-0014 names the
// interrupted-emission threshold — for every caller, including the session-close
// check itself. session.idle_timeout is a different tunable governing the session
// grain's ended_at, and must not be wired in here.
//
// The threshold arrives as a value rather than being read from config inside,
// because ADR-0015 puts it in config rather than in a reader constant and this
// package must stay testable without config. Now travels for the same reason and
// one more: every call in one scan has to be judged against one instant.
//
// The zero value disables the rule: no call is emitted as interrupted and no
// session is reported closed. That is the behaviour every caller had before this
// existed, and it is the only safe default — ADR-0015 rejects upsert and ADR-0004
// deduplicates, so an interrupted record emitted too early is permanent and
// uncorrectable, while one emitted too late is still correct when it arrives.
type Staleness struct {
	// Timeout is how long a session may be silent before it has ended.
	Timeout time.Duration
	// Now is the instant this scan compares last activity against.
	Now time.Time
}

// enabled reports whether this scan may resolve anything at all. A zero Now would
// make every session look infinitely idle and a non-positive Timeout would make
// every unterminated call stale on sight; both would emit records that cannot be
// taken back.
func (s Staleness) enabled() bool { return s.Timeout > 0 && !s.Now.IsZero() }

// SessionState is what one scan knows about the sessions it read: when each was
// last active, and where in the source its earliest line sits.
//
// It is the extension of the reader's existing pending-call buffer that ADR-0023
// §2 asks for, at the session grain, and it is the one place the "this session is
// closed" determination lives — ADR-0023 §3's "no second threshold" is only true
// if there is exactly one implementation of it. T121's ADR-0023 consumer calls
// Closed here rather than deriving its own.
//
// It is in-memory scan state and nothing persists it: correctness comes from
// event_id dedup, so losing it costs a slower scan and never wrong data
// (ADR-0015). It holds a session id, a timestamp and a byte offset — no path, no
// label and no transcript content (ADR-0007, ADR-0019, plan §4.2).
//
// The zero value is ready to use.
type SessionState struct {
	sessions map[record.Identifier]sessionActivity
}

// sessionActivity is one session's two facts: the latest activity seen for it, and
// the earliest byte offset any of its lines occupies.
type sessionActivity struct {
	lastActivity time.Time
	firstOffset  int64
}

// Observe folds one source line into its session's state. timestamp comes from the
// transcript entry and offset from the line's position in the source.
//
// Latest activity is a max and earliest offset is a min rather than last-wins:
// nothing in the format promises the entries are ordered, and a fold that depended
// on the order could make two scans of one transcript disagree about one session.
func (s *SessionState) Observe(sessionID record.Identifier, timestamp time.Time, offset int64) {
	if sessionID == "" || timestamp.IsZero() {
		return
	}
	if s.sessions == nil {
		s.sessions = map[record.Identifier]sessionActivity{}
	}
	activity, seen := s.sessions[sessionID]
	if !seen {
		s.sessions[sessionID] = sessionActivity{lastActivity: timestamp, firstOffset: offset}
		return
	}
	if timestamp.After(activity.lastActivity) {
		activity.lastActivity = timestamp
	}
	if offset < activity.firstOffset {
		activity.firstOffset = offset
	}
	s.sessions[sessionID] = activity
}

// Closed reports ADR-0015's staleness rule for one session: its last activity is
// further back than the threshold, so the session has ended and whatever it left
// unterminated will never terminate.
//
// A session this state never observed is not closed. Absence of evidence that a
// session is alive is not evidence that it ended, and being wrong here cannot be
// corrected (ADR-0004's dedup).
//
// Activity comes only from what the transcript wrote. A SessionEnd hook may cause
// a scan but may never be the evidence a session ended: ADR-0016 keeps hooks as
// triggers whose payload is discarded.
func (s *SessionState) Closed(sessionID record.Identifier, stale Staleness) bool {
	activity, seen := s.sessions[sessionID]
	if !seen {
		return false
	}
	return closed(activity, stale)
}

// CursorFloor reports how many observed sessions are still open and the earliest
// byte offset any of them occupies.
//
// A future incremental cursor (T020, T102) must not advance past that offset:
// ADR-0023 §5 generalizes ADR-0015's "a reader ... does not advance its cursor
// past [an unterminated call]" from one call to one open session. The offset is
// meaningful only when open is positive; it is zero otherwise, which floors a
// caller that forgets to check at the start of the source — a slower scan, never
// wrong data.
func (s *SessionState) CursorFloor(stale Staleness) (open int, offset int64) {
	for _, activity := range s.sessions {
		if closed(activity, stale) {
			continue
		}
		if open == 0 || activity.firstOffset < offset {
			offset = activity.firstOffset
		}
		open++
	}
	return open, offset
}

// closed is the comparison itself, shared by both callers so there is one rule.
//
// Strictly greater than the threshold: a session silent for exactly the threshold
// is still open, which errs toward buffering.
func closed(activity sessionActivity, stale Staleness) bool {
	if !stale.enabled() {
		return false
	}
	return stale.Now.Sub(activity.lastActivity) > stale.Timeout
}
