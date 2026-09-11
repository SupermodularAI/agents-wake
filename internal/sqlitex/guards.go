package sqlitex

// The read guards ADR-0009 puts in one place so they can be tuned once: a store
// this package opens is never scanned without a row cap, and never copied because
// it happened to be large.

// MaxRowsPerPage is the explicit row cap on any single statement. A caller pages
// by advancing its own key until a short page arrives, so no query can ever ask a
// harness's store for an unbounded result set.
const MaxRowsPerPage = 500

// MaxRowsPerScan is the whole-scan budget. Exceeding it stops the walk with
// ErrRowCap rather than reading further: partial collection is safe here because
// every id is derived from its source event, so the next scan re-derives what this
// one did not reach (ADR-0004).
const MaxRowsPerScan = 250_000

// CopyThreshold is the size below which copy-then-read applies. Above it a store
// that will not open directly is reported as blindness instead: copying a
// multi-gigabyte store to read it would pay for observation in disk the user
// never agreed to spend (ADR-0009).
const CopyThreshold = 64 << 20
