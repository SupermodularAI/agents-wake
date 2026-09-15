package cli

import (
	"fmt"
	"strconv"

	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

func init() { diagnosisSections = append(diagnosisSections, harnessDiagnosis) }

// harnessDiagnosis is doctor's per-harness answer: for each harness this build
// reads, whether this scan looked at it at all, and — where it did — whether it
// collected nothing or collected zero.
//
// It goes through the seam rather than into writeDiagnosis for globalBoundary's
// reason: the seam sees only config.Paths, so this section re-reads the counter
// file itself, exactly as the boundary section re-reads the project table.
//
// One harness at a time, in the order the build registers them, so a build with no
// reader for a harness prints nothing about it — the list is what the binary can
// do rather than what somebody wrote down (ADR-0013).
//
// Every value is a word or a count, and never a path, a label or an id: this
// output is what people paste into issues (ADR-0019 §7). The harness labels are
// spelled with a space rather than with their slug's hyphen, because a hyphenated
// slug reads as data on a line of prose and the counters beside it are prose keys.
func harnessDiagnosis(paths config.Paths) []string {
	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		// A counter file this build cannot read has no per-harness answer, and
		// printing "not observed" for it would say the opposite of what is true.
		return []string{"harnesses: unreadable"}
	}

	sections := map[record.Identifier]health.HarnessScan{
		"claude-code": report.Scan.ClaudeCode,
		"opencode":    report.Scan.OpenCode,
	}
	lines := []string{}
	for _, harness := range adapter.Harnesses() {
		scan := sections[harness]
		label := harnessLabel(harness)
		lines = append(lines, fmt.Sprintf("%s: %s", label, health.DiagnoseHarness(scan)))
		for _, counter := range []counterLine{
			{label + " sources", scan.Sources},
			{label + " unreadable sources", scan.Unreadable},
			{label + " parse errors", scan.ParseErrors},
			{label + " refused calls", scan.RefusedCalls},
			{label + " unknown outcomes", scan.UnknownOutcomes},
			{label + " events written", scan.EventsWritten},
		} {
			// A harness nobody read has no counts, and a 0 beside it would read as a
			// measurement (ADR-0046). The substitution is the one writeReasons
			// performs, restated here because the seam returns lines rather than
			// writing them.
			value := health.SkippedUnobserved
			if scan.Observed {
				value = strconv.Itoa(counter.value)
			}
			lines = append(lines, fmt.Sprintf("%s: %s", counter.key, value))
		}
	}
	return lines
}

// harnessLabel renders a harness slug as the words doctor prints it under.
func harnessLabel(harness record.Identifier) string {
	label := []rune(string(harness))
	for index, r := range label {
		if r == '-' {
			label[index] = ' '
		}
	}
	return string(label)
}
