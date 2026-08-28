package cli

import (
	"fmt"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

func init() { diagnosisSections = append(diagnosisSections, globalBoundaryDiagnosis) }

// globalBoundaryDiagnosis is doctor's collection-boundary section: one state word and
// one count.
//
// It goes through the seam rather than into writeDiagnosis because the state word
// needs the project table, which that function knows nothing about — it reads the
// counter file and the settings file and nothing else. The two boundary counters do
// live there, beside every other health.Scan counter.
//
// Requirement 8's distinction is why there are two lines rather than one. "not set"
// and "set" with "repositories: 0" are different states — no boundary at all versus a
// boundary that has discovered nothing yet — and a single count of zero cannot tell
// them apart. "refused" is the fail-closed state made visible rather than silent: the
// boundary is being treated as absent, and a user whose repositories stopped being
// registered needs to know it was rejected rather than never recorded.
//
// A word or a count on every line, never a path, a label or an id (ADR-0019 §7). The
// boundary is a real directory of the user's, and doctor output is what people paste
// into issues — `wake init --global` is where that path is printed, once, to the person
// who typed it.
//
// A state this build cannot read renders one word and stops. The section signature has
// no error channel, and reporting "not set" for a table nobody could read would say
// the opposite of what is true — the "collects nothing" versus "collects zero"
// distinction doctor exists to keep (ADR-0010).
//
// The count of sessions still outside consent is `skipped transcripts` above, and it is
// deliberately not repeated here: it counts transcripts a scan read completely and
// collected nothing from, which is a different fact from anything about the boundary.
func globalBoundaryDiagnosis(paths config.Paths) []string {
	state, err := config.GlobalRootStateFor(paths)
	if err != nil {
		return []string{"global boundary: unreadable"}
	}

	word := "not set"
	switch {
	case state.Refused:
		word = "refused"
	case state.Set:
		word = "set"
	}
	return []string{
		fmt.Sprintf("global boundary: %s", word),
		fmt.Sprintf("global boundary repositories: %d", state.Discovered),
	}
}
