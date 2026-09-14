package ingest

import (
	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/adapter/opencode"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// OpenCodeResult is opencode's reader counters plus the store's write result.
//
// It is its own type rather than a reuse of Result, whose counters
// (RefusedSubagentRuns, SkippedTypedInvocations, AmbiguousSkillRuns and the source
// ordinals) are Claude Code's vocabulary and would be a permanent row of zeroes
// here. That is the empty column plan §4.5 forbids, one layer below the renderer:
// a number that can only ever be 0 reads as a measurement, and this one would be
// an absence.
type OpenCodeResult struct {
	Parsed int
	// Pending, Interrupted, Refused, UnknownOutcomes, SkippedSources and
	// OutOfOrderPairs are the reader's, with the meanings opencode.Result gives
	// them. The caller folds them into health.HarnessScan; a refusal or an
	// unrecognised status is lost collection and moves that harness's state word,
	// while a pending call and a skipped source do not.
	Pending         int
	Interrupted     int
	Refused         int
	UnknownOutcomes int
	SkippedSources  int
	OutOfOrderPairs int
	Written         int
	Duplicate       int
	Dropped         int
}

// OpenCodeScan is one opencode walk's ingest: it pairs the reader's walk-scoped
// state with the store, so a session split across many part rows is resolved once
// and its records are persisted like any other.
//
// Consent and source discovery stay outside this package, exactly as they do for
// Claude Code: the caller opens the store, pages it, and supplies the resolver
// having already established which repositories are consented.
type OpenCodeScan struct {
	scan        *opencode.Scan
	destination *store.Store
}

// NewOpenCodeScan opens a scan over one walk. Every argument the reader needs
// about this machine arrives as a value for the reasons the reader's own doc
// gives: derivation may not read the filesystem, and this package does not read
// config (plan §6.2).
func NewOpenCodeScan(resolve adapter.Resolver, names record.Namer, servers opencode.Servers,
	stale adapter.Staleness, idle adapter.Idleness, destination *store.Store) *OpenCodeScan {
	return &OpenCodeScan{
		scan:        opencode.NewScan(resolve, names, servers, stale, idle),
		destination: destination,
	}
}

// Session registers one of the walk's sessions.
func (s *OpenCodeScan) Session(session opencode.Session) { s.scan.Session(session) }

// Part offers one tool part to the walk.
func (s *OpenCodeScan) Part(part opencode.ToolPart) { s.scan.Part(part) }

// Buffered is how many parts the walk is holding unterminated. No cursor may
// advance past them (ADR-0015).
func (s *OpenCodeScan) Buffered() int { return s.scan.Buffered() }

// Close resolves the walk once and persists what it derived.
//
// A walk that read nothing derives nothing, and Append on an empty slice creates
// no spool, so closing an empty walk is a clean zero rather than a file appearing.
func (s *OpenCodeScan) Close() (OpenCodeResult, error) {
	derived := s.scan.Close()
	written, err := s.destination.Append(derived.Records)
	if err != nil {
		return OpenCodeResult{}, err
	}
	return OpenCodeResult{
		Parsed:          len(derived.Records),
		Pending:         derived.Pending,
		Interrupted:     derived.Interrupted,
		Refused:         derived.Refused,
		UnknownOutcomes: derived.UnknownOutcomes,
		SkippedSources:  derived.SkippedSources,
		OutOfOrderPairs: derived.OutOfOrderPairs,
		Written:         written.Written,
		Duplicate:       written.Duplicate,
		Dropped:         written.Dropped,
	}, nil
}
