// Package errorcell renders the ERRORS cell of the per-primitive table, once,
// for every renderer that draws that table (ADR-0011's thin renderers over one
// aggregation layer).
//
// It is a package rather than a helper in either renderer because it used to be
// neither: internal/report and internal/ui each carried a hand-mirrored copy,
// and the two had already drifted apart in their fallback branch. A cell that
// two copies must agree on is a cell neither owns.
//
// It consumes a metrics.Ratio and never raw counts, so no renderer reconstructs
// a rate for itself — the alternative ADR-0006 rejected precisely because the
// same decision would then be re-made once per consumer, and diverge.
package errorcell

import (
	"fmt"

	"github.com/SupermodularAI/agents-wake/internal/metrics"
)

// Render returns one primitive's ERRORS cell: "0" when nothing failed, and
// otherwise the failure count glued to the population it was rated against —
// "1 of 3 rated (33.3%)", with "; 1 unrated" appended when the source did not
// report an outcome for every call (ADR-0005).
//
// The percentage never appears without that population. This cell sits in a
// column beside CALLS, and a bare "1 (100.0%)" invites a reader to divide the
// one by the other: for 2 calls of which 1 failed and 1 was never rated, both
// numbers are correct and the sentence they form together is not (DG-103).
func Render(ratio metrics.Ratio) string {
	failures := ratio.Numerator()
	if failures == 0 {
		return "0"
	}
	// Percent's second result is not a branch here: NewRatio refuses a numerator
	// greater than its denominator, so a non-zero failure count cannot coexist
	// with a zero denominator. Reaching this line with an undefined rate would be
	// a bug in the metrics layer that NewRatio already panics on, not data to
	// render around.
	percent, _ := ratio.Percent()
	cell := fmt.Sprintf("%d of %d rated (%.1f%%)", failures, ratio.Denominator(), percent)
	if excluded := ratio.Excluded(); excluded > 0 {
		cell += fmt.Sprintf("; %d unrated", excluded)
	}
	return cell
}
