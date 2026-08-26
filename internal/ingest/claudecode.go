// Package ingest coordinates a harness reader and Wake's local event store.
// Consent and source discovery remain outside this package: callers must supply
// a reader only after they have established the repository is consented.
package ingest

import (
	"io"

	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// Result combines reader health with the write result for one transcript.
type Result struct {
	Parsed    int
	Malformed int
	Pending   int
	// Refused is the reader's count of invocations a validated field refused — the
	// primitive's own name, or an entrypoint outside Wake's vocabulary — whether the
	// invocation was a tool call or an attributed skill run.
	// It stays separate from Dropped, which is the store's count of
	// records refused at write time: the two are different fail-closed points and
	// merging them would invent the reason taxonomy doctor (T029) owns.
	//
	// The caller is expected to report it: activation folds it into
	// health.Scan.RefusedCalls, which doctor renders and which puts integration
	// state in "collects nothing". A dropped call nobody counts is how format drift
	// stops collection while doctor still says "collecting" (plan §3.3, §12).
	Refused int
	// RefusedSubagentRuns is the reader's count of subagent runs it could not name: the
	// transcript declared no name, one the grammar refuses, or a directory-scoped one
	// with no scope key (ADR-0036 §2, ADR-0020). Lost collection on the same fail-closed
	// terms as Refused, and reported for the same reason.
	//
	// It is its own counter because doctor treats it differently: activation folds it
	// into health.Scan.RefusedSubagentRuns, which doctor renders as its own line but
	// which does not put integration state in "collects nothing" — the refusal is a
	// standing fact about a transcript, so a state word following it could never change
	// back (see claudecode.Result.RefusedSubagentRuns, health.Diagnose).
	//
	// It arrives on ClaudeCodeScan.Close only, never on Read: a subagent run is judged
	// once its session has closed, and one source's read has no answer to give.
	RefusedSubagentRuns int
	// Interrupted is the reader's count of calls that resolved to outcome interrupted
	// because their session went quiet past the staleness threshold (ADR-0015). Those
	// records are terminal and are also counted by Parsed and Written; this counter
	// exists so doctor can say the transition happened, which is new information
	// rather than lost collection.
	Interrupted int
	// AmbiguousSkillRuns is the reader's count of attributed skill runs it collapsed
	// into an already-emitted fallback for the same session and skill. It is not an
	// invocation count and never joins one: activation folds it into
	// health.Scan.AmbiguousSkillRuns, which doctor renders as its own line so the
	// accepted collapse ADR-0023 documents is visible rather than presented as exact.
	AmbiguousSkillRuns int
	// SkippedSources is the reader's count of sources in this walk that derived
	// nothing, refused nothing, and had nothing attributed back to them once the walk
	// resolved — most often because their working directory belongs to no consented
	// repository. It is a clean zero rather than a failure, and activation folds it
	// into health.Scan.Skipped, which doctor renders as an honest zero.
	//
	// It is reported by the walk rather than per source because after ADR-0036 a
	// source's contribution can resolve after that source's own read has ended, so
	// "parsed nothing" at the end of one read no longer distinguishes an honest zero
	// from a deferral (see claudecode.Result.SkippedSources).
	SkippedSources int
	Written        int
	Duplicate      int
	Dropped        int
}

// ClaudeCode reads one already-authorized Claude Code transcript and persists
// its completed records. It is the service `wake ingest` will use after init
// establishes repository consent.
//
// names is the key a scoped primitive reference is digested under, and travels
// with the resolver because both come from the same consent boundary (ADR-0020).
//
// stale carries ADR-0015's staleness threshold and idle carries ADR-0034's
// session-end inference threshold, both from the caller that owns config; this
// package does not read config (plan §6.2). They are two thresholds answering two
// questions — when an unterminated call is given up on, and when a session id is
// believed finished — and each zero value disables only its own rule.
//
// It is the one-transcript form, and it is a faithful one-source walk: a caller
// reading a set of transcripts that may share a session id must drive a
// ClaudeCodeScan instead, or each file's resolution will judge that session from a
// partial view (ADR-0036 §Consequences).
func ClaudeCode(reader io.Reader, resolve claudecode.Resolver, names record.Namer, stale claudecode.Staleness, idle claudecode.Idleness, destination *store.Store) (Result, error) {
	derived, err := claudecode.Read(reader, resolve, names, stale, idle)
	if err != nil {
		return Result{}, err
	}
	return persist(derived, destination)
}

// ClaudeCodeScan is one walk's ingest: it pairs the reader's walk-scoped session
// state with the store, so a session split across several transcripts is resolved
// once and its records are persisted like any other (ADR-0036 §Consequences).
//
// One source at a time is still read, and each source's own byte offsets keep their
// meaning — the sources are never concatenated, because a cursor floor over a
// concatenated stream is a number about nothing (ADR-0023 §5).
//
// Discovery stays outside this package, as it does for ClaudeCode: the caller walks
// the filesystem and hands over one reader per source, having already established
// that the repository is consented.
type ClaudeCodeScan struct {
	scan        *claudecode.Scan
	destination *store.Store
}

// NewClaudeCodeScan opens a scan over one walk. Its arguments are ClaudeCode's,
// with the same meanings and the same reasons: consent and the scope key travel
// together from one boundary (ADR-0020), and both thresholds arrive as values
// because this package does not read config.
func NewClaudeCodeScan(resolve claudecode.Resolver, names record.Namer, stale claudecode.Staleness,
	idle claudecode.Idleness, destination *store.Store) *ClaudeCodeScan {
	return &ClaudeCodeScan{
		scan:        claudecode.NewScan(resolve, names, stale, idle),
		destination: destination,
	}
}

// Read reads one transcript of the walk and persists what that transcript alone
// made terminal.
//
// Everything a session's resolution owes to the walk's other transcripts is
// resolved by Close, so the counters this returns are the per-source ones only —
// Parsed, Malformed, Refused and the write result. Pending, Interrupted,
// AmbiguousSkillRuns, RefusedSubagentRuns and SkippedSources are zero here and are
// answered by Close; a caller must not fold a zero from this call into a health
// counter.
//
// Refused is complete here rather than half an answer: a subagent run's refusal has
// its own counter on Close (see Result.RefusedSubagentRuns).
func (s *ClaudeCodeScan) Read(reader io.Reader) (Result, error) {
	derived, err := s.scan.Read(reader)
	if err != nil {
		return Result{}, err
	}
	return persist(derived, s.destination)
}

// Close resolves the walk's session-scoped state once, over the union of every
// source it read, and persists the records that resolution derived: the calls the
// staleness rule gave up on, the Shape-A skill fallbacks, and one session_end per
// finished session.
//
// The counters that are only knowable now — Pending, Interrupted,
// AmbiguousSkillRuns, SkippedSources — are the walk's, not any one source's, and
// the caller folds these rather than the per-source zeros.
//
// RefusedSubagentRuns is among them and arrives here only: a subagent transcript
// declaring no usable name can be judged at this boundary and nowhere else, and it is
// its own counter rather than a second half of Refused because doctor's state word
// follows Refused and deliberately not this one (see Result.RefusedSubagentRuns).
//
// A walk that read no source derives nothing, and Append on an empty slice creates
// no spool, so calling Close after an empty walk is a clean zero rather than a
// file appearing.
func (s *ClaudeCodeScan) Close() (Result, error) {
	return persist(s.scan.Close(), s.destination)
}

// persist maps one claudecode.Result onto ingest.Result and writes its records, so
// the one-shot and the walk-scoped forms report identically and neither can drift
// into counting something the other does not.
func persist(derived claudecode.Result, destination *store.Store) (Result, error) {
	written, err := destination.Append(derived.Records)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Parsed:              len(derived.Records),
		Malformed:           derived.Malformed,
		Pending:             derived.Pending,
		Refused:             derived.Refused,
		RefusedSubagentRuns: derived.RefusedSubagentRuns,
		Interrupted:         derived.Interrupted,
		AmbiguousSkillRuns:  derived.AmbiguousSkillRuns,
		SkippedSources:      derived.SkippedSources,
		Written:             written.Written,
		Duplicate:           written.Duplicate,
		Dropped:             written.Dropped,
	}, nil
}
