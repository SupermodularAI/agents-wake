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
	// Refused is the reader's count of calls whose primitive name failed
	// validation. It stays separate from Dropped, which is the store's count of
	// records refused at write time: the two are different fail-closed points and
	// merging them would invent the reason taxonomy doctor (T029) owns.
	//
	// The caller is expected to report it: activation folds it into
	// health.Scan.RefusedCalls, which doctor renders and which puts integration
	// state in "collects nothing". A dropped call nobody counts is how format drift
	// stops collection while doctor still says "collecting" (plan §3.3, §12).
	Refused   int
	Written   int
	Duplicate int
	Dropped   int
}

// ClaudeCode reads one already-authorized Claude Code transcript and persists
// its completed records. It is the service `wake ingest` will use after init
// establishes repository consent.
//
// names is the key a scoped primitive reference is digested under, and travels
// with the resolver because both come from the same consent boundary (ADR-0020).
func ClaudeCode(reader io.Reader, resolve claudecode.Resolver, names record.Namer, destination *store.Store) (Result, error) {
	derived, err := claudecode.Read(reader, resolve, names)
	if err != nil {
		return Result{}, err
	}
	written, err := destination.Append(derived.Records)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Parsed:    len(derived.Records),
		Malformed: derived.Malformed,
		Pending:   derived.Pending,
		Refused:   derived.Refused,
		Written:   written.Written,
		Duplicate: written.Duplicate,
		Dropped:   written.Dropped,
	}, nil
}
