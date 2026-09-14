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
//
// The number lives here rather than in an ADR, and that is the decision rather
// than an omission: ADR-0009 settles that size thresholds and row caps are policy
// belonging in one tunable place, and this file is that place. An ADR carrying the
// figure too would be a second place to change it and a second thing to drift from
// the code, which is the opposite of what putting it in one place was for.
//
// What an ADR would have carried, and what was missing here, is the evidence. Two
// harness stores measured on a real machine sit above this value, and both are
// stated rather than re-derived: opencode's at ~514 MB, and Cursor's at ~6.1 GB —
// an order of magnitude apart, and the second is the multi-gigabyte case the
// paragraph above refuses to copy outright. 64 MiB sits an order of magnitude
// below even the smaller of them, which is deliberately conservative: a store a
// harness has been accumulating into is measured in hundreds of megabytes, so
// copy-then-read is the fallback for a small or young store, never the routine
// path. A tuning that wanted the 514 MB case copied would be raising this number
// against those two measurements, which is the conversation this comment exists to
// make possible.
const CopyThreshold = 64 << 20
