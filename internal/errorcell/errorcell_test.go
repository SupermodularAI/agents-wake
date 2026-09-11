package errorcell

import (
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/metrics"
)

// cases are the ERRORS cell's whole surface. forbidden is the pre-DG-103 shape
// the same counts used to render as — a bare count and a percentage with no
// visible population, which a reader scanning the table binds to the CALLS
// column beside it.
var cases = []struct {
	name                                  string
	numerator, denominator, excluded, tot uint64
	want                                  string
	forbidden                             string
}{
	{name: "the ticket's partially-rated primitive", numerator: 1, denominator: 1, excluded: 1, tot: 2, want: "1 of 1 rated (100.0%); 1 unrated", forbidden: "1 (100.0%)"},
	{name: "some outcomes unknown", numerator: 1, denominator: 3, excluded: 1, tot: 4, want: "1 of 3 rated (33.3%); 1 unrated", forbidden: "1 (33.3%)"},
	{name: "every outcome known", numerator: 2, denominator: 3, excluded: 0, tot: 3, want: "2 of 3 rated (66.7%)", forbidden: "2 (66.7%)"},
	{name: "no failures", numerator: 0, denominator: 3, excluded: 1, tot: 4, want: "0"},
	// DG-103's successor defect, one kind up. Nothing was rated at all, so there
	// is no failure count to report — only a population nobody graded, which used
	// to render as a bare "0" beside a CALLS column reading 2. forbidden stays
	// empty because the want string contains a 0 of its own;
	// TestRenderDistinguishesNoRatedPopulationFromZeroFailures is the assertion
	// that the cell is not a bare zero.
	{name: "no failures and nothing rated", numerator: 0, denominator: 0, excluded: 2, tot: 2, want: "unrated (0 of 2 rated)"},
}

func TestRenderCarriesTheRatedPopulationBesideEveryRate(t *testing.T) {
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := Render(metrics.NewRatio(test.numerator, test.denominator, test.excluded, test.tot))
			if got != test.want {
				t.Errorf("Render() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestRenderNeverEmitsABarePercentage is ADR-0006's guarantee as an executable
// rule rather than a convention: a rate leaves this package only glued to the
// population it was computed over.
func TestRenderNeverEmitsABarePercentage(t *testing.T) {
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := Render(metrics.NewRatio(test.numerator, test.denominator, test.excluded, test.tot))
			if strings.Contains(got, "%") && !strings.Contains(got, " rated (") {
				t.Errorf("Render() = %q, want a rate carrying its rated population", got)
			}
			if test.forbidden != "" && strings.Contains(got, test.forbidden) {
				t.Errorf("Render() = %q, still contains the unattributed shape %q (DG-103)", got, test.forbidden)
			}
		})
	}
}

// A primitive whose calls were all unrated and a primitive rated with zero
// failures are different facts and must not share a cell. The subagent kind is
// the live case: before this, every subagent row printed "0" in the ERRORS
// column, which reads as "never fails" for a kind Wake had never rated at all.
func TestRenderDistinguishesNoRatedPopulationFromZeroFailures(t *testing.T) {
	unrated := Render(metrics.NewRatio(0, 0, 3, 3))
	ratedClean := Render(metrics.NewRatio(0, 3, 0, 3))

	if unrated == "0" {
		t.Errorf("Render() for a wholly unrated population = %q, a bare zero that reads as health", unrated)
	}
	if unrated == ratedClean {
		t.Errorf("Render() = %q for both an unrated and a rated failure-free population", unrated)
	}
	if ratedClean != "0" {
		t.Errorf("Render() for a rated failure-free population = %q, want %q", ratedClean, "0")
	}
}
