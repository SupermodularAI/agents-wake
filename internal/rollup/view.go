package rollup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// MaxViewBlocks is the absolute ceiling on how many sealed blocks one assembled
// view reads, and it is absolute on purpose.
//
// The obvious alternative — a fraction of what is available — is the mistake the
// upstream design calls out from experience: scaling the view to the size of the
// history means the cost of answering a question grows with the history, which
// is the very property this package exists to remove. A fixed ceiling means a
// view over six weeks and a view over six years cost the same.
//
// The value follows from the tiering rather than being chosen freely. A view
// takes at most Fanout-1 blocks per tier, coarse to fine, across MaxTier tiers,
// so (Fanout-1) * MaxTier is the most that arrangement can ask for.
const MaxViewBlocks = int(Fanout-1) * MaxTier

// KeepPerTier is how many blocks an assembled view may take from any one tier
// before it must drop to a finer one.
//
// Fanout-1, from the upstream design. It is a budget on the view and not a
// retention rule: Prune decides what stays on disk, and it keeps whatever is
// needed to bridge each tier to the one above it. The two are related only in
// that a view can never ask for more than Prune retains.
const KeepPerTier = int(Fanout - 1)

// MaxVerbatimEntries is how many of the most recent records a view reads
// unsummarised. Recent events stay exact — that is the half of the design that
// makes a bounded view usable rather than merely small — and this bounds the
// cost of that exactness.
const MaxVerbatimEntries = 500

// View is a bounded summary of the whole store: coarse blocks for old history,
// exact records for recent activity.
type View struct {
	// Blocks are the sealed blocks the view covers, coarse to fine.
	Blocks []Block
	// Verbatim are the most recent records, unsummarised.
	Verbatim []store.Entry
	// CoveredTo is the position where block coverage ends and Verbatim begins.
	CoveredTo uint64
	// Truncated reports that the budget stopped the view short of the oldest
	// sealed block, so the summary does not reach the start of history. It is
	// surfaced rather than hidden for the reason the upstream truncation triple
	// gives: a consumer that cannot tell a complete answer from a clipped one
	// will present the clipped one as complete.
	Truncated bool
}

// Assemble builds a bounded view of the store.
//
// The selection is coarse-to-fine and greedy: the largest tier that fits inside
// the uncovered range is taken first, then the next, so a long history is
// described by a few large blocks and a short one by several small blocks. At
// most Fanout-1 blocks are taken from any one tier, which is what forces the
// walk down a tier instead of letting one tier consume the budget.
func Assemble(dataDir string, source *store.Store, head uint64) (View, error) {
	available, err := readDir(dataDir)
	if err != nil {
		return View{}, err
	}

	selection := selectBlocks(available, head)
	view := View{
		Blocks:    selection.blocks,
		CoveredTo: selection.coveredTo,
		Truncated: selection.exhausted,
	}
	entries, err := source.Entries(view.CoveredTo)
	if err != nil {
		return View{}, err
	}
	view.Verbatim = entries
	return view, nil
}

// selection is the outcome of one coarse-to-fine walk.
type selection struct {
	// blocks are the chosen blocks in ascending position order.
	blocks []Block
	// coveredTo is where block coverage ends and the verbatim tail begins.
	coveredTo uint64
	// exhausted reports that the block budget ran out before reaching the start
	// of history, so the view describes only part of it.
	exhausted bool
	// stalled reports that no block ended at the cursor, leaving a hole. Over a
	// directory this package pruned it is unreachable, because Prune retains
	// exactly what this walk visits — so it is a bug signal and not a normal
	// outcome, kept separate from exhausted for that reason.
	stalled bool
	// visited is every block the walk used, which is exactly the set Prune must
	// retain.
	visited map[blockID]struct{}
}

// selectBlocks walks the available blocks coarse-to-fine, backwards from the
// coverage boundary, and reports what a view needs.
//
// It is deliberately the only place that decides which blocks matter. An earlier
// arrangement had Prune reason about parent coverage while Assemble reasoned
// about a backward chain, and because neither knew the other's requirement,
// every fix to one opened a hole in the other — a coverage gap that presented as
// a plausible wrong number rather than as an error. Retention is now derived
// from this walk (see Prune), so a block is kept if and only if the walk visits
// it, and the two cannot disagree.
//
// At each step the coarsest block ending exactly at the cursor is taken. Picking
// per step rather than draining a tier at a time is what lets the walk cross a
// pruned directory, whose tiers end at different positions: a walk that insisted
// on finishing tier k first would stall at the first cursor no tier-k block ends
// at, even with a perfectly good coarser block available. Per-tier budgets are
// still enforced, so no single tier can consume the view.
func selectBlocks(available map[blockID]Block, head uint64) selection {
	result := selection{visited: make(map[blockID]struct{})}

	var floor uint64
	if head > MaxVerbatimEntries {
		floor = head - MaxVerbatimEntries
	}
	cursor := coverageEnd(available, floor)
	result.coveredTo = cursor

	taken := make(map[uint]int, MaxTier)
	for cursor > 0 {
		if len(result.blocks) >= MaxViewBlocks {
			result.exhausted = true
			break
		}
		progressed := false
		for tier := uint(MaxTier); tier >= 1; tier-- {
			if taken[tier] >= KeepPerTier {
				continue
			}
			id := blockID{tier: tier, start: startOf(tier, cursor), end: cursor}
			block, found := available[id]
			if !found {
				continue
			}
			result.blocks = append(result.blocks, block)
			result.visited[id] = struct{}{}
			cursor = block.Start
			taken[tier]++
			progressed = true
			break
		}
		if !progressed {
			result.stalled = true
			break
		}
	}
	// The walk ran backwards, so reversing puts the blocks in ascending position
	// order — oldest and coarsest first, the order a reader consumes them in. No
	// sort is needed: the ranges are disjoint and were appended in strictly
	// descending order.
	slices.Reverse(result.blocks)
	return result
}

// spanOf is how many positions one block of the given tier covers.
func spanOf(tier uint) uint64 {
	span := uint64(1)
	for level := uint(0); level < tier; level++ {
		span *= Fanout
	}
	return span
}

// startOf is where a block of the given tier ending at end begins, or end when
// no such block could exist.
func startOf(tier uint, end uint64) uint64 {
	span := spanOf(tier)
	if end < span || end%span != 0 {
		// Not a boundary this tier can end on, so the lookup is guaranteed to
		// miss rather than matching some other block.
		return end
	}
	return end - span
}

// coverageEnd is the highest position any available block reaches without
// exceeding floor, and so where block coverage hands over to the verbatim tail.
//
// Choosing the boundary from what is actually sealed is what keeps the two
// halves adjacent with neither a gap nor an overlap. A record must be counted
// exactly once, and blocks carry no ids for most of their members, so an overlap
// would be undetectable to a reader that added the two halves.
//
// It follows that the verbatim tail is not exactly MaxVerbatimEntries long and
// cannot be: it runs from the last block boundary at or below the floor up to
// the head, so it is at least MaxVerbatimEntries and at most that plus one block
// of the coarsest retained tier. A constant ceiling on the read is the property
// that matters, not a precise tail length.
//
// Zero means no block ends at or below floor — a store never sealed, or one
// whose whole history is inside the verbatim window. Both are answered from
// records alone.
func coverageEnd(available map[blockID]Block, floor uint64) uint64 {
	var end uint64
	for id := range available {
		if id.end <= floor {
			end = max(end, id.end)
		}
	}
	return end
}

// Required reports the blocks an assembled view would use over the given head.
// It is what Prune retains, and it exists so that "what a view needs" has one
// definition rather than two that can drift apart.
func Required(dataDir string, head uint64) (map[blockID]struct{}, error) {
	available, err := readDir(dataDir)
	if err != nil {
		return nil, err
	}
	return selectBlocks(available, head).visited, nil
}

// blockID identifies a block by tier and range.
type blockID struct {
	tier       uint
	start, end uint64
}

// readDir loads every readable block in the rollup directory.
//
// A block this build refuses contributes nothing and is not an error: Seal
// removes and rewrites it, and a view is a read path that must still answer
// while that is pending. A view built from fewer blocks reports Truncated, so
// the gap is visible rather than silent.
func readDir(dataDir string) (map[blockID]Block, error) {
	dir := Dir(dataDir)
	names, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	blocks := make(map[blockID]Block, len(names))
	for _, name := range names {
		if name.IsDir() {
			continue
		}
		tier, start, end, ok := ParseBlockName(name.Name())
		if !ok {
			continue
		}
		block, err := ReadBlock(filepath.Join(dir, name.Name()))
		if err != nil {
			continue
		}
		blocks[blockID{tier: tier, start: start, end: end}] = block
	}
	return blocks, nil
}

// Summary reduces a whole view to one set of aggregates.
//
// It merges the sealed blocks and the verbatim tail through exactly the same
// operations a higher tier is sealed with, so the answer does not depend on
// where the boundary between summarised and exact history happens to fall.
func (v View) Summary() Block {
	blocks := make([]Block, 0, len(v.Blocks)+1)
	blocks = append(blocks, v.Blocks...)
	if len(v.Verbatim) > 0 {
		first := v.Verbatim[0].Position - 1
		last := v.Verbatim[len(v.Verbatim)-1].Position
		blocks = append(blocks, Reduce(0, first, last, v.Verbatim))
	}
	if len(blocks) == 0 {
		return Block{BlockVersion: BlockVersion, SchemaVersion: record.SchemaVersion}
	}
	start := blocks[0].Start
	end := blocks[len(blocks)-1].End
	return Merge(0, start, end, blocks)
}
