package health

// SkippedUnobserved is what doctor prints in place of a count no scan measured.
//
// ADR-0046: a surface renders 0 only where observation happened and found nothing;
// where it did not happen it renders "not observed". Omitting the lines instead is that
// ADR's rejected alternative — an absent row is indistinguishable from a population
// that does not exist, which is the opposite conclusion from the one the reader needs.
const SkippedUnobserved = "not observed"

// SkippedReasons is the skipped-transcript count split into the bounded set of reasons
// a transcript is skipped for. The six counts partition Scan.Skipped exactly.
//
// It is derived on every read and never written to the counter file, the way State,
// StoreRebuild and CollectionScope are: the file keeps counts and flags, and what they
// mean is this package's to say because internal/cli only parses and prints (ADR-0001,
// plan §6.2).
//
// No field carries a path, a directory name or any spelling of either — doctor output
// is what people paste into issues (ADR-0019 §7, ADR-0047 §2). A reason is a count.
type SkippedReasons struct {
	// Observed is whether a scan classified these at all. False is not "none of them
	// happened": it is "nobody looked", and the two must never render alike.
	Observed bool
	// NotARepository is the working directory git answered was inside no working tree.
	NotARepository int
	// UnconsentedRepository is a repository this machine has not consented, and a
	// linked worktree of one.
	UnconsentedRepository int
	// UnregisteredWorktree is a linked worktree whose repository this machine did
	// consent — the one reason here that describes collection the user asked for and
	// did not get.
	UnregisteredWorktree int
	// OutsideCollectionWindow is a directory the table does match, whose events predate
	// the instant collection began for its repository (ADR-0024, ADR-0025).
	OutsideCollectionWindow int
	// Unclassified is a directory nothing could answer about (ADR-0047 §4).
	Unclassified int
	// NothingTerminal is a transcript no working directory was declined for: it
	// resolved as consented and still derived nothing.
	NothingTerminal int
}

// SkippedByReason reads the breakdown a scan recorded.
//
// A scan that did not classify — none has run, this build could not read the counter
// file, or the walk did not finish — is Observed false and carries no counts: its
// zeroes would be "collects zero for a state nobody measured", which is the failure
// this package exists to refuse.
func SkippedByReason(scan Scan) SkippedReasons {
	if !scan.SkippedClassified {
		return SkippedReasons{}
	}
	return SkippedReasons{
		Observed:                true,
		NotARepository:          scan.SkippedNotARepository,
		UnconsentedRepository:   scan.SkippedUnconsentedRepository,
		UnregisteredWorktree:    scan.SkippedUnregisteredWorktree,
		OutsideCollectionWindow: scan.SkippedOutsideCollectionWindow,
		Unclassified:            scan.SkippedUnclassified,
		NothingTerminal:         scan.SkippedNothingTerminal,
	}
}
