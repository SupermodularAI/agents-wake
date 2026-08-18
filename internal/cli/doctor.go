package cli

import (
	"fmt"
	"io"
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
			claudeDir, err := config.ClaudeCodeDir()
			if err != nil {
				return err
			}
			return writeDiagnosis(cmd.OutOrStdout(), paths, claudeDir)
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

	// The live count, read out of the settings file, rather than what `init` last
	// recorded installing. They differ exactly when somebody edited that file since,
	// and "are the hooks there now" is the question doctor is being asked — a user
	// who deleted Wake's hooks by hand must not be told they are installed. A file
	// this build cannot read is its own state on the integration line below, not a
	// zero here.
	installed, hookErr := activation.HookState(claudeDir)
	if hookErr != nil {
		installed = 0
	}

	for _, line := range []struct {
		key   string
		value int
	}{
		{"hooks installed", installed},
		{"hooks removed", report.Hooks.Removed},
		{"owned hook groups kept", report.Hooks.KeptOwned},
	} {
		if _, err := fmt.Fprintf(out, "%s: %d\n", line.key, line.value); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(out, "last scan: %s\n", scanTime(report, readErr)); err != nil {
		return err
	}
	for _, line := range []struct {
		key   string
		value int
	}{
		{"transcripts", report.Scan.Transcripts},
		{"unreadable sources", report.Scan.Unreadable},
		{"parse errors", report.Scan.ParseErrors},
		{"skipped transcripts", report.Scan.Skipped},
		{"events written", report.Scan.EventsWritten},
		{"refused project entries", report.Scan.RefusedProjects},
		{"refused calls", report.Scan.RefusedCalls},
	} {
		if _, err := fmt.Fprintf(out, "%s: %d\n", line.key, line.value); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintf(out, "integration: %s\n", integrationState(report, readErr, hookErr))
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
// A refused project entry belongs in the first arm for the same reason: an entry
// this build will not resolve is attribution it could not perform, so every
// transcript belonging to that repository counted as holding nothing, and the
// numbers are missing all of it. It is also not a rare tamper case — it is what
// every project table written before match_mac became required looks like on its
// first scan, and the remedy (`wake init` in the repository) is undiscoverable if
// doctor calls the situation a complete count of zero.
//
// A refused call belongs in it for the same reason, and it is the arm that catches
// format drift: a primitive Wake found but could not name was invoked, and the
// numbers below are missing that invocation. Inferring a name would be worse than
// losing it (plan §3.3), so the drop is correct and reporting it is what keeps the
// drop honest — a harness renaming the field a primitive's identity lives in stops
// collection for that whole kind, and this is the only line that says so.
//
// Skipped is deliberately not in it. A transcript whose working directory belongs to
// no consented repository was read completely and collected nothing because consent
// says so, and an unterminated call is a number that is not final yet rather than
// one nobody could read (ADR-0015). Both are honest zeroes.
//
// An input this build cannot read is its own state rather than an error: a
// diagnostic that failed in the situation it exists for is worse than one that says
// what it could not determine. Both unreadable states come first, because a number
// derived from an input nobody could read is not a number worth reading — and a
// settings file this build refuses is exactly what a user comes here after `wake
// init` told them to fix it.
func integrationState(report health.Report, readErr, hookErr error) string {
	switch {
	case readErr != nil:
		return "counters unreadable"
	case hookErr != nil:
		return "hooks unreadable"
	case report.Scan.At.IsZero():
		return "never scanned"
	case report.Scan.Unreadable > 0 || report.Scan.ParseErrors > 0 || report.Scan.RefusedProjects > 0 || report.Scan.RefusedCalls > 0:
		return "collects nothing"
	case report.Scan.EventsWritten == 0:
		return "collects zero"
	}
	return "collecting"
}
