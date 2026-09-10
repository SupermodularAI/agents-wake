package activation

import (
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
)

// skippedNotes is one walk's note of which working directory the resolver declined for
// each source it read, in the ordinal order claudecode.Scan assigns them.
//
// In memory, for the length of one walk, and nowhere else: nothing here is written,
// printed, cached across scans or delivered (ADR-0047 §2). It exists because the reason
// a transcript was skipped is a question about a directory, and the adapter
// deliberately knows a source only as an ordinal — so the directories stay in this
// package, which already holds every cwd because it is the package that answers consent
// about them.
//
// Both methods are nil-safe on the receiver, matching boundaryDiscovery's convention.
type skippedNotes struct {
	declined []string
}

// openSource claims the ordinal claudecode.Scan is about to claim for the next source.
// It is called immediately before each Read, and exactly once per Read, which is what
// keeps the two ordinal sequences the same one.
func (n *skippedNotes) openSource() {
	if n == nil {
		return
	}
	n.declined = append(n.declined, "")
}

// decline records cwd against the source now being read, first answer wins, so one
// transcript is attributed to one directory whatever else it went on to name.
//
// An empty cwd is not recorded: there is nothing to classify and nothing to point git
// at, and a transcript that named none counts as unattributed rather than as a
// directory that does not exist.
func (n *skippedNotes) decline(cwd string) {
	if n == nil || cwd == "" || len(n.declined) == 0 {
		return
	}
	if last := len(n.declined) - 1; n.declined[last] == "" {
		n.declined[last] = cwd
	}
}

// skippedByDirectory is one walk's skipped transcripts grouped for classification: how
// many were declined for each distinct directory, and how many had no declined
// directory at all.
//
// Grouped, not listed, because ADR-0047 §3 costs one bounded probe per distinct
// unresolved directory rather than one per transcript — which is what makes 1,422
// transcripts affordable.
type skippedByDirectory struct {
	byDirectory  map[string]int
	unattributed int
}

// group folds the ordinals the walk reported skipping into that shape. An ordinal this
// walk has no note for counts as unattributed rather than being guessed at.
func (n *skippedNotes) group(ordinals []int) skippedByDirectory {
	grouped := skippedByDirectory{byDirectory: map[string]int{}}
	for _, ordinal := range ordinals {
		if n == nil || ordinal < 0 || ordinal >= len(n.declined) || n.declined[ordinal] == "" {
			grouped.unattributed++
			continue
		}
		grouped.byDirectory[n.declined[ordinal]]++
	}
	return grouped
}

// classifySkipped turns one walk's skipped transcripts, grouped by the directory each
// was declined for, into the bounded reason counters doctor prints.
//
// It runs in the scan, beside the counters it explains (ADR-0047 §3): a figure produced
// by a later pass than the numbers printed beside it can disagree with them for reasons
// no reader can see. It runs after the walk, never inside it, so no git call reaches the
// derivation path (ADR-0019 §1, ADR-0044 §4).
//
// One classification per distinct directory, not one per transcript (ADR-0047 §3): the
// grouping is what makes 1,422 transcripts cost one bounded call per distinct
// unresolved directory.
//
// It records nothing and consents nothing — a number went up (ADR-0047 §1, §2).
// SkippedClassified is what separates "this scan measured these and they are zero" from
// "no scan measured them" (ADR-0046), and it is set last so a panic mid-classification
// leaves the scan honestly unclassified.
func classifySkipped(repos *config.Repos, skipped skippedByDirectory, scan *health.Scan) {
	scan.SkippedNothingTerminal = skipped.unattributed
	for dir, count := range skipped.byDirectory {
		switch repos.ClassifyUnresolvedDirectory(dir) {
		case config.DirectoryNotARepository:
			scan.SkippedNotARepository += count
		case config.DirectoryUnconsentedRepository:
			scan.SkippedUnconsentedRepository += count
		case config.DirectoryUnregisteredWorktree:
			scan.SkippedUnregisteredWorktree += count
		case config.DirectoryConsented:
			scan.SkippedOutsideCollectionWindow += count
		default:
			// ADR-0047 §4: no classification, never a default bucket. A reason nobody
			// anticipated arrives here rather than in the nearest existing one.
			scan.SkippedUnclassified += count
		}
	}
	scan.SkippedClassified = true
}
