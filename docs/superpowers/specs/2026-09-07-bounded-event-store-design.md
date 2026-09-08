# Sealed-block tiered rollup for the local event store

Status: approved for implementation (2026-09-07)

## Problem

The local event store grows without bound. Measured on the first machine to
install wake: 31,288 records from 1,420 transcripts over roughly six weeks of
history, in one `~/.local/state/wake/events.ndjson`. It grows linearly and
nothing trims it.

Three readers — `internal/report/report.go`, `internal/ui/ui.go` and
`internal/inventory/state.go` — each call `Entries(0)` and decode the whole
spool into memory to answer a question about it. That cost grows with history
even when the question is about last week.

## What this bounds, and what it does not

This bounds **the assembled view and the cost of reading it**. It does not
bound disk.

The spool is never modified and never truncated. `Store.Entries` assigns
`Position` by counting decoded lines from the start of the file
(`store.go:359-366`), so `Position` is an ordinal and not a stored field.
Truncating the spool would renumber every position, and the remote delivery
watermark is a position — `watermark.go:58` states it directly: "Position
counts the records the spool held". A prune would leave that cursor pointing at
a different record, and nothing downstream would notice.

Pruning is therefore deliberately out of scope rather than merely unimplemented.
It needs a durable record identity that does not exist today.

## Design

Adopted from `headlong`'s sealed-block tiered rollup, with its LLM reduce
replaced by arithmetic.

### Blocks

- Immutable blocks keyed by position range. Tier *k* covers `F^k` events;
  fanout `F` is 10, so tier 1 = 10 events, tier 2 = 100, tier 3 = 1,000.
- One file per block, named by its range: `rollup/t<tier>-<start>-<end>.json`.
  If the file exists, the work is skipped. Cost is only ever the new frontier,
  so the process is incremental by construction rather than by bookkeeping.
- Written with `atomicfile.Publish` at `0600` under a `0700` directory.
  Write-once by name plus atomic publish means no new lock is required.
- The source log is never modified. Blocks are a derived cache.

### The reduce is arithmetic, and the histogram is why

Per `(kind, name, outcome)` key a block stores an invocation count, a session
count, and a fixed-boundary latency histogram.

The upstream note asks for "counts and percentiles". Percentiles are stored as
histogram buckets instead, because **percentiles are not mergeable**: `p95` of a
union is not derivable from the `p95` of its parts. A block storing percentile
values could not seal tier 2 from tier-1 blocks without re-reading source
records, which would destroy the "sealed once, never recomputed" property the
whole scheme rests on.

Bucket counts are integers, so merging is elementwise addition — associative,
commutative and exact. Percentiles are derived at view-assembly time from the
merged histogram and never stored. Bucket boundaries are a frozen package
constant so a block sealed today merges with one sealed a year from now.

The reduce performs no model call, no network I/O and no map iteration that
reaches output order. `internal/remote/otlp.go` declares its encoder pure; this
keeps that property.

### Drill-down: a summary is a pointer set

Each block retains the earliest `event_id` per distinct key, ordered by
`(timestamp, event_id)`. IDs propagate upward on merge under the same rule.

This is the load-bearing detail. A summary must stay queryable: "skill X was
never used" is answered by a zero count, and any positive count drills to an
exact invocation. Keeping one id per key also bounds block size by key
*cardinality* rather than by event count, which is what keeps the higher tiers
small.

### The assembled view

At most `F-1` blocks per tier, coarse-to-fine, filling a fixed budget. Recent
events stay verbatim.

The budget is **absolute, not a fraction of what is available**. Upstream found
that scaling a view to the available window ballooned the cost of re-reading
history on every wakeup.

The boundary between summarised and verbatim history is a sealed block's edge
rather than a fixed offset from the head. Blocks are selected first and the
verbatim tail covers whatever they do not, which is what makes a gap between the
two unrepresentable. It follows that the tail is not exactly the verbatim limit
and cannot be: it is at least that many records and at most that plus one block
of the coarsest retained tier. A constant ceiling on the read is the property
that matters, not a precise tail length.

### Enablement and backfill

`store.rollup_after` already exists in `internal/config/keys.go:23` as a
`KindDuration` key defaulting to the sentinel `never`, inert by ADR-0014. It
becomes the enablement switch: `never` keeps today's behaviour exactly, and a
duration seals blocks for events older than it.

The key stays a duration and is not redefined as a count. How many events fall
in seven days varies per machine, so a duration cannot name a position-range
boundary; the tiering underneath stays count-based and the key governs only
which events are eligible to be sealed.

Sealing builds forward from enablement, snapped to a fanout boundary, so no
permanent coverage hole is left mid-block. Backfill of history before
enablement is an explicit opt-in. With an arithmetic reduce, backfilling 31k
records costs milliseconds, so the opt-in is a policy choice about touching a
user's whole history rather than a cost one.

The floor is persisted beside the blocks rather than recomputed, because
nothing in the spool records when the key was set. It is written even when
there is nothing to seal — enabling on an empty store records a floor of zero
— because deferring it to the first scan with sealable history would place the
floor above everything appended in between, and nothing below a floor is ever
sealed. That was a real bug, caught by a test of the fresh-install path.

A key that was inert ceasing to be inert is an observable behaviour change and
gets a CHANGELOG entry.

### Pruning superseded blocks

Sealing every tier and keeping all of them is what the upstream note describes,
and on its own it does not bound storage: every tier stays on disk forever, so
the rollup grows linearly with history exactly like the spool it summarises.
Measured at 31,288 records, the unpruned directory held 3,474 blocks and 10.8 MB
against a 15 MB spool — a second copy rather than a summary, with tier 1 alone
accounting for 9.7 MB of it.

So a seal writes only what a view would read, and removes anything left over.
The same history is 75 blocks and 230 KB, or 1.5% of the spool.

Deciding what to write *before* writing it is the part that took two attempts.
Sealing every complete range and pruning afterwards left each scan writing
about 3,400 blocks and deleting them again — at 31,288 records "sealed once,
never recomputed" and "the cost of a scan is the new frontier" were both false
while nothing failed and CI stayed green. Higher tiers are still merged from
the tiers below them, but through blocks held in memory rather than through
files that exist only to be deleted.

Two numbers are needed to get this right and they are not the same: sealing
stops at the retention window's frontier, while retention has to keep what a
view over the whole store reads. Conflating them was the bug.

Retention is derived from the view's own selection walk rather than from a
separate rule about which blocks a coarser one supersedes. That is the load-
bearing part of the design and it was learned the hard way: with Prune and
Assemble each reasoning independently about what was needed, every fix to one
opened a hole in the other, and the failure mode was a view that stalled at the
hole and reported 4% of history as though it were all of it — a plausible wrong
number with no error anywhere. One walk now decides, and a block is retained if
and only if that walk visits it.

Pruning removes derived blocks only. The spool is untouched, so a prune costs at
most a reseal.

### Invalidation

Two independent defences, following the pattern `readDeliveryState` already
uses for a derived position across a schema bump:

1. `Store.Discard` removes the rollup directory. `Discard` is the rebuild path
   (`internal/activation/activation.go:306`); combined with "if the file exists,
   skip", a surviving block would describe records that no longer exist and
   would never be recomputed.
2. Every block carries `schema_version` and is refused on mismatch, then
   resealed.

Position ranges are the block key, and positions renumber on a rebuild. That is
safe because invalidation is tied to `Discard` plus the schema stamp, not to a
range being durable identity. The block-format comment says so, so the next
reader does not assume otherwise.

### No SchemaVersion bump

`internal/record/record.go`'s rule keys on the record contract. Versions 4 and 5
bumped without adding a field because they changed how an `event_id` is
*derived*. This adds no `Record` field and changes no derivation — it adds a new
derived artifact over unchanged records. Blocks carry their own version stamp
instead.

## Testing

The property test that matters: merging N tier-1 blocks produces aggregates
identical to reducing the same record set directly. That single property is
what proves the reduce is mergeable, and it is worth more than the rest of the
suite.

Also: sealing twice writes nothing; the assembled view respects the absolute
cap; a block whose schema stamp is foreign is refused rather than read; a
missing rollup directory is a normal first run and not an error.

## Out of scope

- Rewiring `report.go`, `ui.go` and `inventory/state.go` onto the assembled
  view. The subsystem and its API land first with tests, so the format is
  provable before anything depends on it; rewiring is a focused follow-up with
  its own before/after numbers.
- Pruning the spool, for the position-identity reason above.
- The missing `AGENTS.md` (cited by `Makefile:77`) and the missing `docs/adr/`
  (cited across `internal/`, where the numbers also collide with a different
  scheme). Both are dangling references, both reported for their own item.
- Anything under `internal/remote/`. The OTLP golden test freezes an attribute
  key literal that a privacy allowlist depends on; a clean
  `git diff internal/remote/` is the check that no wire change happened.
