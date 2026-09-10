package health

import "testing"

// A scan that did not classify has no zeroes to report: its counters are absent, not
// measured (ADR-0046). Both shapes reach it — no scan at all, and a scan whose walk
// did not finish, which is a Scan carrying counts with the flag still false.
func TestSkippedByReasonReadsUnobservedWhenNoScanClassified(t *testing.T) {
	for _, c := range []struct {
		name string
		scan Scan
	}{
		{"no scan has run", Scan{}},
		{"a scan that did not finish its walk", Scan{At: scannedAt, Skipped: 4, SkippedNotARepository: 4}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := SkippedByReason(c.scan)

			if got.Observed {
				t.Errorf("Observed = true, want false; nothing classified these")
			}
			if got != (SkippedReasons{}) {
				t.Errorf("SkippedByReason() = %+v, want the zero value; an unclassified scan carries no counts", got)
			}
		})
	}
}

// The other half of ADR-0046's distinction: a scan that did classify and found none
// reports zeroes, and those zeroes are real.
func TestSkippedByReasonReadsZeroWhenAScanClassifiedAndFoundNone(t *testing.T) {
	got := SkippedByReason(Scan{At: scannedAt, SkippedClassified: true})

	if !got.Observed {
		t.Fatalf("Observed = false, want true; this scan classified and found none")
	}
	if got != (SkippedReasons{Observed: true}) {
		t.Errorf("SkippedByReason() = %+v, want every count zero", got)
	}
}

// Every reason arrives, and each is distinct, so a helper that read one field into two
// places fails rather than passing on equal numbers.
func TestSkippedByReasonCarriesEveryReason(t *testing.T) {
	got := SkippedByReason(Scan{
		At:                             scannedAt,
		Skipped:                        21,
		SkippedNotARepository:          1,
		SkippedUnconsentedRepository:   2,
		SkippedUnregisteredWorktree:    3,
		SkippedOutsideCollectionWindow: 4,
		SkippedUnclassified:            5,
		SkippedNothingTerminal:         6,
		SkippedClassified:              true,
	})

	want := SkippedReasons{
		Observed:                true,
		NotARepository:          1,
		UnconsentedRepository:   2,
		UnregisteredWorktree:    3,
		OutsideCollectionWindow: 4,
		Unclassified:            5,
		NothingTerminal:         6,
	}
	if got != want {
		t.Errorf("SkippedByReason() = %+v, want %+v", got, want)
	}
}

// The invariant the whole design rests on: the six reasons partition Skipped exactly.
// A breakdown that does not sum to the number above it cannot be read, and a seventh
// field added later without a case here fails this.
func TestTheSkippedReasonsPartitionTheSkippedCount(t *testing.T) {
	for _, scan := range []Scan{
		{At: scannedAt, SkippedClassified: true},
		{At: scannedAt, Skipped: 1, SkippedUnregisteredWorktree: 1, SkippedClassified: true},
		{At: scannedAt, Skipped: 1422, SkippedNotARepository: 900, SkippedUnconsentedRepository: 199,
			SkippedUnregisteredWorktree: 223, SkippedOutsideCollectionWindow: 90,
			SkippedUnclassified: 5, SkippedNothingTerminal: 5, SkippedClassified: true},
	} {
		reasons := SkippedByReason(scan)
		sum := reasons.NotARepository + reasons.UnconsentedRepository + reasons.UnregisteredWorktree +
			reasons.OutsideCollectionWindow + reasons.Unclassified + reasons.NothingTerminal
		if sum != scan.Skipped {
			t.Errorf("the reasons sum to %d for a scan reporting %d skipped transcripts; the breakdown does not partition the count", sum, scan.Skipped)
		}
	}
}
