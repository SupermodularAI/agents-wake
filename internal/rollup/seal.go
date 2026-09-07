package rollup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

const (
	// DirName is the rollup directory under the data root. It holds only
	// derived data: removing it costs one reseal and never a different answer.
	DirName = "rollup"

	// blockFileMode and blockDirMode match internal/store. A block carries no
	// path and no label, but it is state about this user's machine and the rest
	// of the local layout is 0600 under 0700.
	blockFileMode fs.FileMode = 0o600
	blockDirMode  fs.FileMode = 0o700
)

// ErrForeignBlock is the one refusal a caller is meant to recognise: a block an
// earlier build wrote correctly under a contract this build does not read.
//
// It is separate from a decode failure for the reason record.ErrUnsupportedVersion
// is: every other error means the file was never valid, while this one means the
// answer is to seal it again. Both versions produce it — record.SchemaVersion
// because a block's contents describe records of that version, BlockVersion
// because the reduce itself may have changed.
var ErrForeignBlock = errors.New("block written under another version")

// blockName is the file name for one block, and the file name is the identity:
// a seal that finds this file present does no work. The range is half-open,
// [start, end), and is rendered zero-padded so a directory listing sorts in
// position order.
func blockName(tier uint, start, end uint64) string {
	return fmt.Sprintf("t%d-%012d-%012d.json", tier, start, end)
}

// blockNamePattern recognises this package's own files, so a stray file in the
// directory is left alone rather than parsed as a block. It is also what makes
// Prune safe to point at a directory a user might have put something in.
var blockNamePattern = regexp.MustCompile(`^t(\d+)-(\d{12})-(\d{12})\.json$`)

// Dir is the rollup directory for a data root.
func Dir(dataDir string) string { return filepath.Join(dataDir, DirName) }

// ReadBlock loads one sealed block, refusing one this build cannot read.
//
// A missing file is reported as fs.ErrNotExist so the caller can tell "not
// sealed yet" from "sealed and unreadable" — the first is the normal state of
// the frontier and the second means a reseal is owed.
func ReadBlock(path string) (Block, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Block{}, err
	}
	var block Block
	if err := json.Unmarshal(raw, &block); err != nil {
		// The message names the file and not the content: a JSON error embeds
		// the bytes it choked on, and those bytes are local telemetry.
		return Block{}, fmt.Errorf("parsing %s: invalid block", filepath.Base(path))
	}
	if block.BlockVersion != BlockVersion || block.SchemaVersion != record.SchemaVersion {
		return Block{}, fmt.Errorf("%s: %w", filepath.Base(path), ErrForeignBlock)
	}
	if len(block.Keys) > 0 {
		// A block whose keys are not in the order this build sorts them was
		// either hand-edited or written by a build with a different rule, and
		// merging it would produce a block whose bytes depend on that. Refused
		// as foreign rather than repaired, which is the same answer a version
		// mismatch gets and for the same reason.
		if !blockKeysOrdered(block.Keys) {
			return Block{}, fmt.Errorf("%s: %w", filepath.Base(path), ErrForeignBlock)
		}
	}
	return block, nil
}

// blockKeysOrdered reports whether keys are in the canonical sorted order.
func blockKeysOrdered(keys []KeyAggregate) bool {
	for index := 1; index < len(keys); index++ {
		if compareKeys(keys[index-1].Key, keys[index].Key) >= 0 {
			return false
		}
	}
	return true
}

// WriteBlock publishes a block, and does nothing if one is already sealed for
// that range.
//
// The skip is the design's incrementality: the cost of a seal is the new
// frontier and never the history behind it. It is also why no lock is needed —
// a block is a pure function of its range, so two processes sealing the same
// range write identical bytes.
//
// The write is a temporary file plus a rename, which is atomicfile's mechanism
// without atomicfile's fsync. That difference is deliberate and it is the one
// place this package departs from the local convention, so it is worth stating
// why. atomicfile.Publish syncs the file before the rename and the directory
// after it, because the state it was written for — a delivery watermark, a
// health counter — is the only copy of something that cannot be recomputed. A
// block is the opposite: it is derived from a spool that is still there, and
// every path that reads one already treats missing or unreadable as "seal it
// again". Paying two fsyncs per block would buy durability no reader needs, and
// it is not free — a backfill of 12,000 records seals 1,333 blocks, which took
// ten seconds of fsync and takes well under one without.
//
// Atomicity is kept, and it is the half that matters here: the rename is still
// atomic, so a concurrent reader sees a complete block or no block, never a
// half-written one. Durability is recovered in bulk by the caller — see Seal's
// single SyncDir — so a seal is still durable once it returns, just not once per
// block.
//
// Returns whether it wrote.
func WriteBlock(dir string, block Block) (bool, error) {
	if err := os.MkdirAll(dir, blockDirMode); err != nil {
		return false, fmt.Errorf("creating rollup directory: %w", err)
	}
	path := filepath.Join(dir, blockName(block.Tier, block.Start, block.End))
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("inspecting block: %w", err)
	}

	// Indented, and with a trailing newline, because these files are meant to
	// be readable when someone is working out why a number looks wrong. They
	// are bounded by key cardinality, so the bytes are affordable.
	data, err := json.MarshalIndent(block, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encoding block: %w", err)
	}

	temp, err := os.CreateTemp(dir, ".seal-*")
	if err != nil {
		return false, fmt.Errorf("creating a temporary block in %s: %w", dir, err)
	}
	name := temp.Name()
	if _, err := temp.Write(append(data, '\n')); err != nil {
		return false, errors.Join(fmt.Errorf("writing a block: %w", err), temp.Close(), os.Remove(name))
	}
	// CreateTemp opens at 0600 whatever the umask, and blockFileMode is 0600, so
	// this is belt-and-braces rather than the mechanism — and it is on the
	// handle, not the path, so it cannot land on a different file.
	if err := temp.Chmod(blockFileMode); err != nil {
		return false, errors.Join(fmt.Errorf("setting a block's mode: %w", err), temp.Close(), os.Remove(name))
	}
	if err := temp.Close(); err != nil {
		return false, errors.Join(fmt.Errorf("closing a block: %w", err), os.Remove(name))
	}
	if err := os.Rename(name, path); err != nil {
		return false, errors.Join(fmt.Errorf("publishing a block: %w", err), os.Remove(name))
	}
	return true, nil
}

// Remove deletes the whole rollup directory.
//
// It is called from store.Discard, and that pairing is load-bearing. Discard is
// the rebuild path: it removes the spool so a scan can re-derive it. A block
// that survived that would describe records which no longer exist, and because
// a sealed block is never recomputed it would keep describing them forever. The
// version stamps in ReadBlock are the second, independent defence — this one
// covers a rebuild that changes which records exist without changing any
// version.
//
// An absent directory is not an error: dropping derived data is idempotent, and
// a machine that never enabled rollups is a normal first run.
func Remove(dataDir string) error {
	// RemoveAll takes the enablement mark with it, which is deliberate: the mark
	// describes blocks, so one that outlived them would leave a floor above
	// history nothing had sealed and no reseal could reach.
	if err := os.RemoveAll(Dir(dataDir)); err != nil {
		return fmt.Errorf("removing rollup directory: %w", err)
	}
	return nil
}

// SealResult reports what a seal did.
type SealResult struct {
	// Sealed is how many block files were written.
	Sealed int
	// Skipped is how many were already present, which is the steady state.
	Skipped int
	// Refused is how many existing blocks this build could not read. A positive
	// count means blocks are owed a reseal, and Seal removes them so the next
	// run writes them again.
	Refused int
	// Pruned is how many superseded fine blocks were removed because a coarser
	// block already covers their range.
	Pruned int
}

// entryRange is the half-open position range a tier-1 block covers.
func entryRange(index uint64) (uint64, uint64) {
	start := index * Fanout
	return start, start + Fanout
}

// Seal brings the rollup directory up to date for every complete block below
// frontier, and is safe to call repeatedly.
//
// frontier is the highest position eligible for sealing — records newer than the
// retention window sit above it — and head is what the spool holds. Pruning uses
// head, because retention has to keep what a view over the whole store reads.
//
// Only complete blocks are sealed. A partial frontier is left unsealed on
// purpose: a block covering positions 30-40 that was written when the spool held
// 34 records would be sealed forever with six records missing, and nothing would
// ever revisit it. The unsealed frontier is exactly the region a view reads
// verbatim, so nothing is lost by waiting.
//
// The tier loop merges upward from blocks already on disk rather than from
// records. That is what keeps the work linear in events with no re-reads: a
// tier-3 block costs ten file reads and some addition, whatever its range.
func Seal(dataDir string, source *store.Store, frontier, head uint64) (SealResult, error) {
	return seal(dataDir, source, frontier, head, false)
}

// Backfill seals history from before rollups were enabled, and is the explicit
// opt-in the design calls for.
//
// Sealing builds forward from enablement, so a store with existing history
// leaves it unsealed until this is called. With an arithmetic reduce the cost is
// milliseconds rather than the model calls the upstream design pays, so the
// opt-in is a policy choice about touching a user's whole history rather than a
// cost one — but it stays an opt-in either way, and the mark records that it
// happened so a later seal does not undo it.
func Backfill(dataDir string, source *store.Store, frontier, head uint64) (SealResult, error) {
	return seal(dataDir, source, frontier, head, true)
}

// seal is Seal with the backfill decision left to the caller.
func seal(dataDir string, source *store.Store, frontier, head uint64, backfill bool) (SealResult, error) {
	result := SealResult{}
	dir := Dir(dataDir)

	// The floor is where sealing may begin: on a first run it is the current
	// head, so nothing already in the spool is summarised and rollups describe
	// only what happens from now on. A backfill lowers it to zero.
	//
	// It is established before the early return below, so enabling on an empty
	// store records a floor of zero rather than deferring the decision. That
	// deferral was a real bug: the mark would then be written at whatever the
	// head had grown to, leaving every record appended in between permanently
	// below the floor and never sealed.
	mark, found := ReadEnablement(dir)
	switch {
	case !found && backfill:
		mark = Enablement{Backfilled: true}
	case !found:
		// The frontier and not the head: a record inside the retention window
		// is not sealable yet, and a floor above it would leave it permanently
		// below the floor once it aged out.
		mark = Enablement{Floor: frontier}
	case backfill && !mark.Backfilled:
		mark.Floor, mark.Backfilled = 0, true
	default:
		// A mark already exists and says what this run needs. Nothing to write.
		mark.Floor = snapToFanout(mark.Floor)
		return sealFrom(dir, source, frontier, head, mark.Floor)
	}
	// Backfilled is set wherever the floor is lowered, including on a first run
	// that is itself a backfill — the flag is what stops a later plain seal
	// raising the floor again, so a backfill it did not record would be one a
	// later run could undo.
	if err := WriteEnablement(dir, mark); err != nil {
		return result, err
	}
	// Snapped by the same rule WriteEnablement applies, rather than re-reading
	// the file to discover what it did. The write is the authority on what is
	// stored; relying on a re-read to learn the snapped value made the coupling
	// invisible and the value easy to use unsnapped.
	floor := snapToFanout(mark.Floor)
	return sealFrom(dir, source, frontier, head, floor)
}

// snapToFanout rounds a floor down to a fanout boundary. It is the rule
// WriteEnablement stores by, kept as a function so the in-memory value and the
// stored one cannot disagree.
//
// The snap is what prevents a permanent coverage hole: a floor mid-block leaves
// that block neither sealable whole nor coverable by anything finer, and a block
// is sealed once and never revisited.
func snapToFanout(floor uint64) uint64 { return floor - floor%Fanout }

// sealFrom does the sealing itself, given a settled floor.
//
// frontier is the highest position sealing may cover — the retention window's
// edge — while head is what the spool actually holds. The two are different
// numbers and both are needed: sealing stops at the frontier, but retention has
// to keep what a view over the *whole* store would read.
//
// Only blocks a view would use are written. That is the property that makes a
// repeated scan free, and it took two attempts to get right: sealing every
// complete range and pruning afterwards meant each scan wrote 3,400 blocks and
// deleted them again — at 31,288 records, "sealed once, never recomputed" and
// "the cost of a scan is the new frontier" were both false while nothing
// failed. Higher tiers are still merged from the tiers below them, but through
// blocks held in memory rather than through files that only exist to be deleted.
func sealFrom(dir string, source *store.Store, frontier, head, floor uint64) (SealResult, error) {
	result := SealResult{}

	complete := frontier / Fanout
	if complete == 0 {
		return result, nil
	}

	// Which blocks a view would read, decided before anything is written, so a
	// block that would be pruned is never written in the first place.
	wanted := plannedBlocks(head, frontier, floor)

	// Tier 1 is reduced from records; every higher tier is merged from the tier
	// below. Blocks live in this map whether or not they are written, so a tier
	// that is only scaffolding still contributes to the tier above it.
	tier1, refused, err := reduceTier1(dir, source, complete, floor, wanted, &result)
	if err != nil {
		return result, err
	}
	result.Refused += refused

	below := tier1
	span := Fanout
	for tier := uint(2); tier <= MaxTier; tier++ {
		span *= Fanout
		groups := frontier / span
		if groups == 0 {
			break
		}
		above := make(map[uint64]Block, groups)
		for group := range groups {
			start := group * span
			end := start + span
			if start < floor {
				continue
			}
			children := make([]Block, 0, Fanout)
			childSpan := span / Fanout
			for index := range Fanout {
				child, found := below[start+index*childSpan]
				if !found {
					break
				}
				children = append(children, child)
			}
			if len(children) < int(Fanout) {
				// A child is missing, so this tier cannot be sealed exactly.
				// Skipped rather than sealed from a partial set: a block that
				// summarised nine tenths of its range would be wrong forever.
				continue
			}
			merged := Merge(tier, start, end, children)
			above[start] = merged
			if _, keep := wanted[blockID{tier: tier, start: start, end: end}]; !keep {
				continue
			}
			written, writeErr := WriteBlock(dir, merged)
			if writeErr != nil {
				return result, writeErr
			}
			if written {
				result.Sealed++
			} else {
				result.Skipped++
			}
		}
		below = above
	}

	// Anything on disk that the plan does not want is removed. With the plan
	// driving what gets written, this only has work to do after a change of
	// head, a version bump, or an interrupted run.
	pruned, err := Prune(dir, head)
	if err != nil {
		return result, err
	}
	result.Pruned = pruned

	// One directory sync for the whole seal, recovering in bulk the durability
	// WriteBlock does not pay per block.
	//
	// Reported, and reported alongside a result that still counts what was
	// sealed. Every block has been renamed into place and is readable, so this
	// is not a failed seal: the only thing at risk is whether the directory
	// entries survive a power loss, which costs a reseal of derived data — the
	// same as never having sealed. A caller that treats it as fatal loses
	// nothing either, which is why it is not swallowed.
	if result.Sealed > 0 || result.Pruned > 0 {
		if err := atomicfile.SyncDir(dir); err != nil {
			return result, fmt.Errorf("syncing the rollup directory: %w", err)
		}
	}
	return result, nil
}

// reduceTier1 produces every tier-1 block above the floor, writing only those
// the plan wants, and reports how many existing blocks were refused.
//
// It reads the spool once for the whole pass. store.Entries decodes from the
// start on every call and discards what precedes the position asked for, so
// calling it per block would cost one full decode per block — quadratic in
// history, and worst exactly on the backfill this design promises to make cheap.
func reduceTier1(dir string, source *store.Store, complete, floor uint64, wanted map[blockID]struct{}, result *SealResult) (map[uint64]Block, int, error) {
	blocks := make(map[uint64]Block, complete)
	refused := 0

	entries, err := source.Entries(0)
	if err != nil {
		return nil, 0, err
	}
	for index := range complete {
		start, end := entryRange(index)
		if start < floor {
			// History from before rollups were enabled. Left alone until a
			// backfill asks for it.
			continue
		}
		// Positions are one-based and contiguous within one read, so a block's
		// range indexes directly into the slice. Checked against the slice's
		// own bounds rather than assumed: a spool rebuilt shorter under us
		// would otherwise panic here.
		if end > uint64(len(entries)) {
			continue
		}
		window := entries[start:end]
		if uint64(len(window)) < Fanout || window[0].Position != start+1 {
			// The spool no longer holds a full, contiguously numbered block
			// here — rebuilt shorter, or invalid lines renumbered it. Not
			// sealable now; the next run finds the same gap.
			continue
		}
		block := Reduce(1, start, end, window)
		blocks[start] = block

		id := blockID{tier: 1, start: start, end: end}
		if _, keep := wanted[id]; !keep {
			continue
		}
		path := filepath.Join(dir, blockName(1, start, end))
		present, isRefused, stateErr := blockState(path)
		if stateErr != nil {
			return nil, refused, stateErr
		}
		if isRefused {
			refused++
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				return nil, refused, fmt.Errorf("removing foreign block: %w", removeErr)
			}
			present = false
		}
		if present {
			result.Skipped++
			continue
		}
		written, writeErr := WriteBlock(dir, block)
		if writeErr != nil {
			return nil, refused, writeErr
		}
		if written {
			result.Sealed++
		} else {
			result.Skipped++
		}
	}
	return blocks, refused, nil
}

// plannedBlocks is the set of blocks a view over head would read, given what a
// seal up to frontier is able to produce.
//
// It is computed from the ranges sealing *could* write rather than from the
// files currently on disk, so a first seal plans the same set a later one does
// and no block is written only to be pruned. Blocks above the verbatim floor are
// included even though today's walk does not read them: the floor moves forward
// with the head, and they are the coarse material the next walk will use.
//
// A repeated seal at an unchanged head is therefore free — nothing written,
// nothing pruned, which is the case a hook-fired scan hits on every session end.
// A seal after the head advances is not free and is not meant to be: the
// verbatim floor slides forward, so blocks that were above it fall below and the
// fine blocks in the newly covered region are written and then superseded by the
// coarser tier above them. That churn is bounded by how far the head moved
// rather than by how much history exists — about Fanout blocks per Fanout new
// positions — which is the "cost is the new frontier" property. It is not zero,
// and a reader of these numbers should not expect it to be.
func plannedBlocks(head, frontier, floor uint64) map[blockID]struct{} {
	available := make(map[blockID]Block)
	span := uint64(1)
	for tier := uint(1); tier <= MaxTier; tier++ {
		span *= Fanout
		for group := range frontier / span {
			start := group * span
			if start < floor {
				continue
			}
			id := blockID{tier: tier, start: start, end: start + span}
			available[id] = Block{Tier: tier, Start: start, End: start + span}
		}
	}

	wanted := selectBlocks(available, head).visited
	keepAbove := verbatimFloor(head)
	for id := range available {
		if id.end > keepAbove {
			wanted[id] = struct{}{}
		}
	}
	return wanted
}

// blockState reports whether a block is present and readable, present but
// foreign, or absent.
//
// An unreadable block is reported as refused rather than as an error, and that
// is the whole purpose of the function: the records behind it are still in the
// spool, so the answer to any unreadable block — a foreign version, a truncated
// file, a hand edit — is to remove it and seal the range again. Returning the
// read error instead would fail a seal over derived data that a reseal fixes for
// free.
func blockState(path string) (present, refused bool, err error) {
	if _, statErr := os.Stat(path); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("inspecting block: %w", statErr)
	}
	_, readErr := ReadBlock(path)
	return true, readErr != nil, nil
}

// ParseBlockName reports the tier and range a file name encodes. It exists so a
// reader can enumerate the directory without a naming convention duplicated at
// each call site.
func ParseBlockName(name string) (tier uint, start, end uint64, ok bool) {
	match := blockNamePattern.FindStringSubmatch(name)
	if match == nil {
		return 0, 0, 0, false
	}
	t, err := strconv.ParseUint(match[1], 10, 8)
	if err != nil {
		return 0, 0, 0, false
	}
	s, err := strconv.ParseUint(match[2], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	e, err := strconv.ParseUint(match[3], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	return uint(t), s, e, true
}

// Prune removes every block an assembled view would not use.
//
// Retention is derived from the view's own selection walk rather than from a
// rule that tries to predict it — see selectBlocks. That is the whole design of
// this function, and it is worth stating why, because the obvious alternative
// looks simpler and is wrong: an earlier version reasoned independently about
// which fine blocks a coarser one superseded, and because Prune's rule and the
// view's requirement were two separate pieces of logic, each fix to one opened a
// hole in the other. The failure mode was the expensive kind — a view that
// stalled at the hole and reported 4% of history as though it were all of it,
// with no error anywhere.
//
// A block is now kept if and only if the walk visits it. The two cannot
// disagree, because there is only one of them.
//
// This is what bounds what is *stored*, as distinct from what is read. Without
// it every tier stays on disk forever and the rollup grows linearly with
// history, exactly like the spool it was meant to summarise: measured at the
// 31,288 records that motivated this package, the unpruned directory held 3,474
// blocks and 10.8 MB against a 15 MB spool — a second copy rather than a
// summary. Pruned, the same history is 14 blocks and 44 KB.
//
// The source log is untouched here, as everywhere in this package. Pruning a
// derived block costs at most a reseal; the spool is the only copy of the
// records and is never trimmed (see the package comment on positions).
func Prune(dir string, head uint64) (int, error) {
	names, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the rollup directory: %w", err)
	}

	dataDir := filepath.Dir(dir)
	required, err := Required(dataDir, head)
	if err != nil {
		return 0, err
	}
	if len(required) == 0 {
		// The walk found nothing to use — a directory whose whole history is
		// inside the verbatim window, or one not yet sealed far enough to be
		// walkable. Removing everything on that basis would delete blocks a
		// slightly later head will need, so nothing is pruned.
		return 0, nil
	}

	// Blocks above the verbatim floor are kept even though today's walk does not
	// visit them: a view reads those positions from the spool, so they are not
	// needed *now*, but the floor moves forward with the head and they are the
	// coarse material the next walk will use.
	//
	// Leaving them out was the churn bug. Retention that considered only the
	// current walk deleted exactly the blocks the seal had just written, so
	// every scan resealed and repruned them — 3,456 blocks per scan at 31,288
	// records, with nothing failing and the incrementality claim quietly false.
	// The test that catches it has to run above MaxVerbatimEntries, because
	// below that the floor is zero and this region does not exist.
	keepAbove := verbatimFloor(head)

	pruned := 0
	for _, entry := range names {
		if entry.IsDir() {
			continue
		}
		if isEnablementFile(entry.Name()) {
			// The mark is this package's own state, not a block. Removing it
			// would reset the floor to the head on the next run and strand
			// everything below it.
			continue
		}
		tier, start, end, ok := ParseBlockName(entry.Name())
		if !ok {
			// Not one of this package's files. The directory is this package's,
			// but a stray file in it is still not this package's to delete.
			continue
		}
		if _, needed := required[blockID{tier: tier, start: start, end: end}]; needed {
			continue
		}
		if end > keepAbove {
			continue
		}
		if removeErr := os.Remove(filepath.Join(dir, entry.Name())); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return pruned, fmt.Errorf("pruning a superseded block: %w", removeErr)
		}
		pruned++
	}
	return pruned, nil
}
