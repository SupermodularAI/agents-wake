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
	{name: "no failures and nothing rated", numerator: 0, denominator: 0, excluded: 2, tot: 2, want: "0"},
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
