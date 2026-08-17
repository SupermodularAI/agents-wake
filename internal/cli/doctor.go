package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
)

func init() { commands = append(commands, newDoctorCmd) }

// newDoctorCmd builds the `doctor` subcommand.
//
// ADR-0010 puts integration state here: "doctor becomes the place where integration
// state is legible, and must distinguish 'collects nothing' from 'collects zero'."
// The hook-invoked scan is required to exit 0 in silence (ADR-0016), so this is the
// only surface on which a scan that could not read a source can say so.
//
// This is deliberately the minimum that distinction can be tested against — counts
// and one state word. T029 and T103 own the fuller diagnostic; nothing here
// forecloses it.
func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Report what Wake's last scan and last hook change managed to do",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			return writeDiagnosis(cmd.OutOrStdout(), paths, filepath.Join(home, ".claude"))
		},
	}
}

// writeDiagnosis prints the counters and the one state word they imply.
//
// Counts only, one `key: value` per line, and never a path, a label or an id: this
// output is what people paste into issues (ADR-0019 §7). A test asserts it carries
// no path separator at all, which is the strongest form of that check since nothing
// printed here has a legitimate slash in it.
func writeDiagnosis(out io.Writer, paths config.Paths, claudeDir string) error {
	report, readErr := health.New(paths.HealthFile).Read()

	// A settings file this build cannot read reports zero hooks rather than failing:
	// the live count is a second opinion on what the counters say, and losing it must
	// not cost the whole diagnosis.
	installed, hookErr := activation.HookState(claudeDir)
	if hookErr != nil {
		installed = 0
	}

	lines := []struct {
		key   string
		value int
	}{
		{"hooks installed", installed},
		{"hooks removed", report.Hooks.Removed},
		{"owned hook groups kept", report.Hooks.KeptOwned},
		{"transcripts", report.Scan.Transcripts},
		{"unreadable sources", report.Scan.Unreadable},
		{"parse errors", report.Scan.ParseErrors},
		{"skipped transcripts", report.Scan.Skipped},
		{"events written", report.Scan.EventsWritten},
		{"refused project entries", report.Scan.RefusedProjects},
	}
	// The recorded install count is what `init` last did; the live one is what the
	// settings file holds now. They differ when somebody edited the file since, and
	// the live one is the honest answer to "are the hooks there".
	if readErr == nil && installed == 0 {
		lines[0].value = report.Hooks.Installed
	}

	if _, err := fmt.Fprintf(out, "last scan: %s\n", scanTime(report, readErr)); err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintf(out, "%s: %d\n", line.key, line.value); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "integration: %s\n", integrationState(report, readErr))
	return err
}

// scanTime renders when the last scan ran, or that there has not been one.
func scanTime(report health.Report, readErr error) string {
	if readErr != nil || report.Scan.At.IsZero() {
		return "never"
	}
	return report.Scan.At.UTC().Format(time.RFC3339)
}

// integrationState is the one word ADR-0010 asks for, and it is exactly one of five.
//
// "collects nothing" and "collects zero" are the pair that matters. A source that
// could not be read or could not be parsed means the numbers below are missing
// something and nobody knows how much; everything read and nothing found means the
// numbers are complete and the answer is zero. Reporting the first as the second is
// how `unused` would come to recommend removing something the user relies on.
//
// A counter file this build cannot read is its own state rather than an error: a
// diagnostic that failed in the situation it exists for is worse than one that says
// what it could not determine.
func integrationState(report health.Report, readErr error) string {
	switch {
	case readErr != nil:
		return "counters unreadable"
	case report.Scan.At.IsZero():
		return "never scanned"
	case report.Scan.Unreadable > 0 || report.Scan.ParseErrors > 0:
		return "collects nothing"
	case report.Scan.EventsWritten == 0:
		return "collects zero"
	}
	return "collecting"
}
