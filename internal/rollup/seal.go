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
// head, and is safe to call repeatedly.
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
func Seal(dataDir string, source *store.Store, head uint64) (SealResult, error) {
	return seal(dataDir, source, head, false)
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
func Backfill(dataDir string, source *store.Store, head uint64) (SealResult, error) {
	return seal(dataDir, source, head, true)
}

// seal is Seal with the backfill decision left to the caller.
func seal(dataDir string, source *store.Store, head uint64, backfill bool) (SealResult, error) {
	result := SealResult{}
	dir := Dir(dataDir)

	// The floor is established before the early return below, so enabling on an
	// empty store records a floor of zero rather than deferring the decision to
	// the next scan. Deferring it was a real bug: the mark would then be written
	// at whatever the head had grown to, so the records appended in between were
	// permanently below the floor and nothing was ever sealed.
	//
	// The floor is where sealing may begin. On a first run it is the current
	// head, so nothing already in the spool is sealed and rollups describe only
	// what happens from now on; a backfill lowers it to zero and records that.
	mark, found := ReadEnablement(dir)
	if !found {
		mark = Enablement{Floor: head}
		if backfill {
			mark.Floor = 0
		}
		if err := WriteEnablement(dir, mark); err != nil {
			return result, err
		}
		mark, _ = ReadEnablement(dir)
	} else if backfill && !mark.Backfilled {
		mark.Floor, mark.Backfilled = 0, true
		if err := WriteEnablement(dir, mark); err != nil {
			return result, err
		}
	}
	floor := mark.Floor

	complete := head / Fanout
	if complete == 0 {
		return result, nil
	}

	// Which tier-1 blocks are missing is decided before the spool is read, so a
	// steady-state seal — every block already present but the newest — reads no
	// records at all.
	missing := make([]uint64, 0, complete)
	for index := range complete {
		start, end := entryRange(index)
		if start < floor {
			// History from before rollups were enabled. Left alone until a
			// backfill asks for it.
			continue
		}
		path := filepath.Join(dir, blockName(1, start, end))
		present, refused, err := blockState(path)
		if err != nil {
			return result, err
		}
		if refused {
			result.Refused++
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				return result, fmt.Errorf("removing foreign block: %w", removeErr)
			}
			present = false
		}
		if present {
			result.Skipped++
			continue
		}
		missing = append(missing, index)
	}

	// One read of the spool for the whole seal, not one per block.
	//
	// This is load-bearing rather than an optimisation. store.Entries decodes
	// the spool from the start on every call and discards what precedes the
	// position asked for, so calling it once per missing block would cost one
	// full decode per block — quadratic in history, and worst exactly on the
	// backfill this design promises to make cheap. Reading once and slicing
	// keeps a seal linear in the records it actually summarises.
	if len(missing) > 0 {
		entries, err := source.Entries(0)
		if err != nil {
			return result, err
		}
		// Positions are one-based and contiguous within one read, so a block's
		// range indexes directly into the slice. Verified against the slice's
		// own bounds rather than assumed: a spool rebuilt shorter under us would
		// otherwise panic here.
		for _, index := range missing {
			start, end := entryRange(index)
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
			written, err := WriteBlock(dir, Reduce(1, start, end, window))
			if err != nil {
				return result, err
			}
			if written {
				result.Sealed++
			} else {
				result.Skipped++
			}
		}
	}

	// Higher tiers are merged from the tier below, never from records.
	span := Fanout
	for tier := uint(2); tier <= MaxTier; tier++ {
		span *= Fanout
		groups := head / span
		if groups == 0 {
			break
		}
		for group := range groups {
			start := group * span
			end := start + span
			if start < floor {
				continue
			}
			path := filepath.Join(dir, blockName(tier, start, end))
			present, refused, err := blockState(path)
			if err != nil {
				return result, err
			}
			if refused {
				result.Refused++
				if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
					return result, fmt.Errorf("removing foreign block: %w", removeErr)
				}
				present = false
			}
			if present {
				result.Skipped++
				continue
			}
			children, err := readChildren(dir, tier, start, span)
			if err != nil {
				return result, err
			}
			if len(children) < int(Fanout) {
				// A child is missing, so this tier cannot be sealed exactly.
				// Skipped rather than sealed from a partial set: a block that
				// summarised nine tenths of its range would be wrong forever.
				continue
			}
			written, err := WriteBlock(dir, Merge(tier, start, end, children))
			if err != nil {
				return result, err
			}
			if written {
				result.Sealed++
			} else {
				result.Skipped++
			}
		}
	}

	// Superseded fine blocks are dropped once the coarse block covering them
	// exists. Without this the directory grows linearly with events — measured
	// at the 31,288 records that motivated this package, tier 1 alone was 9.7 MB
	// against a 15 MB spool, so the "summary" cost 65% of the data it
	// summarised. See Prune.
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

// readChildren loads the Fanout blocks of the tier below that make up one block.
func readChildren(dir string, tier uint, start, span uint64) ([]Block, error) {
	childSpan := span / Fanout
	children := make([]Block, 0, Fanout)
	for index := range Fanout {
		childStart := start + index*childSpan
		path := filepath.Join(dir, blockName(tier-1, childStart, childStart+childSpan))
		child, err := ReadBlock(path)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrForeignBlock) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, nil
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
		if removeErr := os.Remove(filepath.Join(dir, entry.Name())); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return pruned, fmt.Errorf("pruning a superseded block: %w", removeErr)
		}
		pruned++
	}
	return pruned, nil
}
