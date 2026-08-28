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
// last active, whether any source of it was read blind, and where in each source
// its earliest line sits.
//
// It is the extension of the reader's existing pending-call buffer that ADR-0023
// §2 asks for, at the session grain, and it is the one place the "this session is
// closed" determination lives — ADR-0023 §3's "no second threshold" is only true
// if there is exactly one implementation of it. T121's ADR-0023 consumer calls
// Closed here rather than deriving its own.
//
// It spans a whole walk, not one source. ADR-0036 establishes that Claude Code
// splits one session id across several transcripts — the parent's file and one per
// subagent — so liveness folded per file judges a session from a partial view.
// Liveness is therefore walk-wide while byte offsets stay per source, which is why
// the two folds have different grains below.
//
// It is in-memory scan state and nothing persists it: correctness comes from
// event_id dedup, so losing it costs a slower scan and never wrong data
// (ADR-0015). It holds a session id, a timestamp, a byte offset and a source
// ordinal — no path, no label and no transcript content (ADR-0007, ADR-0019,
// plan §4.2).
//
// The zero value is ready to use.
type SessionState struct {
	sessions map[record.Identifier]sessionActivity
	offsets  map[sourceSession]int64
}

// sessionActivity is one session's walk-wide liveness: the latest activity seen
// for it in any source, and whether any source carrying it was read blind.
type sessionActivity struct {
	lastActivity time.Time
	// blind marks a session some source of this scan could not be read completely
	// for: that source held a line the reader could not rule out as the terminator
	// of a buffered call. No rule that concludes silence may run for such a session
	// — in that source or in any other — because the line it could not read may be
	// exactly the evidence the session was alive (plan §3.3).
	blind bool
}

// sourceSession keys one session's earliest byte offset inside one source. The
// source half is the ordinal the scan assigned that source in walk order, never a
// path: an offset only means something relative to the file it was measured in,
// and the reader is not allowed to know which file that is (ADR-0007, plan §4.2).
type sourceSession struct {
	source  int
	session record.Identifier
}

// Observe folds one source line into its session's state. source is the ordinal of
// the source being read, timestamp comes from the transcript entry and offset from
// the line's position in that source.
//
// Latest activity is a max and earliest offset is a min rather than last-wins:
// nothing in the format promises the entries are ordered, and a fold that depended
// on the order could make two scans of one transcript disagree about one session.
//
// The two folds have deliberately different grains. Latest activity is a max over
// every source of the session, because a session writing in its parent transcript
// is alive whatever its subagent files show (ADR-0036). The offset min is taken per
// (source, session): a min over offsets measured in different files is a number
// about nothing, and a cursor is per file.
func (s *SessionState) Observe(source int, sessionID record.Identifier, timestamp time.Time, offset int64) {
	if sessionID == "" || timestamp.IsZero() {
		return
	}
	if s.sessions == nil {
		s.sessions = map[record.Identifier]sessionActivity{}
	}
	if s.offsets == nil {
		s.offsets = map[sourceSession]int64{}
	}
	activity, seen := s.sessions[sessionID]
	if !seen || timestamp.After(activity.lastActivity) {
		activity.lastActivity = timestamp
	}
	s.sessions[sessionID] = activity

	key := sourceSession{source: source, session: sessionID}
	if first, held := s.offsets[key]; !held || offset < first {
		s.offsets[key] = offset
	}
}

// MarkBlind records that a source carrying this session held a line the reader
// could not rule out as a terminator, so no rule that concludes silence may run
// for it — in this source or in any other.
//
// The taint is per session rather than per walk because after ADR-0036 a session's
// resolution spans every source that carries it: one source's blindness bears on
// all of them, and only on them. A walk-wide switch would disable the staleness
// rule and session_end derivation for every session on the machine over one
// unreadable line.
//
// It marks only a session already observed. An unobserved session is never Closed
// anyway — absence of evidence that a session is alive is not evidence it ended —
// so there is nothing to hold back, and observing one here would invent activity
// with no timestamp behind it.
func (s *SessionState) MarkBlind(sessionID record.Identifier) {
	activity, seen := s.sessions[sessionID]
	if !seen {
		return
	}
	activity.blind = true
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

// OpenSessions counts observed sessions still open under stale, across every
// source this scan read. It is walk-wide because liveness is: a session is open if
// any of its sources shows recent activity (ADR-0036).
func (s *SessionState) OpenSessions(stale Staleness) int {
	open := 0
	for _, activity := range s.sessions {
		if !closed(activity, stale) {
			open++
		}
	}
	return open
}

// SourceFloor reports the earliest byte offset inside one source belonging to any
// session still open — including a session open only because another source shows
// recent activity for it.
//
// A future incremental cursor (T020, T102) must not advance past that offset:
// ADR-0023 §5 generalizes ADR-0015's "a reader ... does not advance its cursor
// past [an unterminated call]" from one call to one open session, and ADR-0036
// widens the open-session question from one file to every file of the session. A
// subagent transcript whose own lines all look quiet therefore keeps its floor
// while the parent transcript shows the session running.
//
// open is false when this source holds no open session, in which case offset is
// meaningless and is zero — which floors a caller that forgets to check at the
// start of the source: a slower scan, never wrong data.
func (s *SessionState) SourceFloor(source int, stale Staleness) (open bool, offset int64) {
	for key, first := range s.offsets {
		if key.source != source {
			continue
		}
		if closed(s.sessions[key.session], stale) {
			continue
		}
		if !open || first < offset {
			offset = first
		}
		open = true
	}
	return open, offset
}

// closed is the comparison itself, shared by every caller so there is one rule.
//
// A blind session is never closed, checked first: some source carrying it held a
// line the reader could not rule out as a terminator, so its last activity is
// known-understated rather than merely old, and concluding silence from that is
// inferring a terminal outcome from blindness (plan §3.3). That check is what makes
// the taint per session rather than per walk, while leaving resolveSessionSkills,
// resolveStaleCalls and finishedSessions deciding exactly what they decided before.
//
// Strictly greater than the threshold: a session silent for exactly the threshold
// is still open, which errs toward buffering.
func closed(activity sessionActivity, stale Staleness) bool {
	if activity.blind {
		return false
	}
	if !stale.enabled() {
		return false
	}
	return stale.Now.Sub(activity.lastActivity) > stale.Timeout
}
