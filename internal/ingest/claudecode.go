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
	Written   int
	Duplicate int
	Dropped   int
}

// ClaudeCode reads one already-authorized Claude Code transcript and persists
// its completed records. It is the service `wake ingest` will use after init
// establishes repository consent.
func ClaudeCode(reader io.Reader, repo record.Hash, destination *store.Store) (Result, error) {
	derived, err := claudecode.Read(reader, repo)
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
		Written:   written.Written,
		Duplicate: written.Duplicate,
		Dropped:   written.Dropped,
	}, nil
}
