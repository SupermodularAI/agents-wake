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
	Written            int
	Duplicate          int
	Dropped            int
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
func ClaudeCode(reader io.Reader, resolve claudecode.Resolver, names record.Namer, stale claudecode.Staleness, idle claudecode.Idleness, destination *store.Store) (Result, error) {
	derived, err := claudecode.Read(reader, resolve, names, stale, idle)
	if err != nil {
		return Result{}, err
	}
	written, err := destination.Append(derived.Records)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Parsed:             len(derived.Records),
		Malformed:          derived.Malformed,
		Pending:            derived.Pending,
		Refused:            derived.Refused,
		Interrupted:        derived.Interrupted,
		AmbiguousSkillRuns: derived.AmbiguousSkillRuns,
		Written:            written.Written,
		Duplicate:          written.Duplicate,
		Dropped:            written.Dropped,
	}, nil
}
