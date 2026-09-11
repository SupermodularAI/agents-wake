// Package adapter holds what every harness reader shares. It is extracted from
// two concrete readers and from nothing else (ADR-0013): the Claude Code reader,
// which walks transcripts, and the opencode reader, which consumes rows.
//
// What is shared is the contract values, not a walk. The two readers take their
// sources by routes their storage decides (an io.Reader per transcript; rows off
// a bounded query), resume differently (a walk-scoped session fold with a carry;
// a full re-derivation with no cursor), attribute differently (explicit for
// Claude Code, absent for opencode), measure duration differently (derived from a
// pair; exact from two instants the harness recorded), and derive ids differently
// (a composed source-event string; a bare primary key). ADR-0013 names every one
// of those differences, so a method set over them would be satisfied by exactly
// one shape and rewritten by the third reader.
//
// What every reader does owe its caller is the same, and that is what this
// package says: how consent is asked, when an unterminated invocation is given up
// on, when a session is believed finished, what the counters mean, and which
// harness the walk is for.
//
// It holds values only. A reader may import it without acquiring any capability —
// no filesystem, no store, no config — which is what keeps each reader's own
// import freeze meaningful.
package adapter

import (
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// Resolver is the consent seam. Given a working directory a harness recorded and
// the instant of the event in it, it answers which repository the event belongs to
// and whether that repository consented to collect an event at that instant.
//
// Consent has two dimensions and one answer: the directory was never consented, or
// the event predates the instant collection began for its repository (ADR-0024,
// ADR-0025). The reader passes the event's own timestamp and never learns the
// boundary, so no adapter can widen consent in either dimension.
type Resolver func(cwd string, at time.Time) (record.Hash, bool)

// Staleness is ADR-0015's rule: how long an unterminated invocation may go
// unresolved before it is emitted as interrupted, and the instant to compare
// against.
//
// The zero value disables the rule, which is what a caller that cannot read its
// threshold must do — ADR-0015 rejects upsert and ADR-0004 deduplicates, so an
// interrupted record emitted too early is permanent and uncorrectable, while one
// emitted too late is still correct when it arrives.
type Staleness struct {
	// Timeout is how long an invocation's session may be silent before the
	// invocation is given up on.
	Timeout time.Duration
	// Now is the instant this scan compares last activity against. It travels with
	// the threshold because every invocation in one scan has to be judged against
	// one instant.
	Now time.Time
}

// Enabled reports whether this scan may give up on anything at all. A zero Now
// would make every session look infinitely idle and a non-positive Timeout would
// make every unterminated invocation stale on sight; both write records that
// cannot be taken back.
func (s Staleness) Enabled() bool { return s.Timeout > 0 && !s.Now.IsZero() }

// Idleness is ADR-0034's rule: how long a session id may be silent before it is
// believed finished.
//
// A second type beside Staleness rather than a field on it, deliberately. ADR-0023
// §3 requires Staleness.Timeout to be scan.stale_call_timeout for every caller,
// and session.idle_timeout is a different tunable answering a different question.
// Two thresholds, two types, two predicates, one each.
type Idleness struct {
	Timeout time.Duration
	Now     time.Time
}

// Enabled reports whether this scan may believe any session finished, on the same
// terms and for the same reason as Staleness.Enabled.
func (i Idleness) Enabled() bool { return i.Timeout > 0 && !i.Now.IsZero() }

// Result is the counter vocabulary every reader reports.
//
// A reader with counters of its own keeps them on its own type and folds into
// this. A counter here that some reader cannot have would be a permanent row of
// zeroes for it, which is the empty column plan §4.5 forbids, one layer below the
// renderer — so this holds only what both readers genuinely measure.
type Result struct {
	Records []record.Record
	// Pending is how many invocations the walk is holding unterminated. A number
	// that is not final yet, never collection that was lost (ADR-0015).
	Pending int
	// Interrupted is how many the staleness rule gave up on. Those records are in
	// Records, carrying the outcome that says they never finished.
	Interrupted int
	// Refused is how many invocations a validated field refused. Lost collection,
	// counted so doctor can say so — the refused value is never carried, only the
	// count (plan §4.2).
	Refused int
	// SkippedSources is how many of the walk's sources yielded nothing. An honest
	// zero, never a failure.
	SkippedSources int
	// OutOfOrderPairs is how many invocations reported an end before their start.
	// Their records carry a nil duration rather than a clamped 0 (ADR-0027).
	OutOfOrderPairs int
}

// Scan is what a caller may ask any reader's walk, and it is deliberately two
// questions.
//
// Harness is the slug ADR-0004 namespaces every id by, so a caller folding
// per-harness diagnostics never has to hold the slug beside the reader. Buffered
// is ADR-0015's hard requirement on the interface: a reader buffers invocations
// that have no result yet, a caller must be able to see how many, and no cursor
// may advance past them.
type Scan interface {
	Harness() record.Identifier
	Buffered() int
}
