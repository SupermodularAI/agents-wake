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
//
// Pending and interrupted calls are two lines rather than one, because "buffered,
// may still finish" and "resolved as never finishing" are different facts and a
// single number would conflate them. Neither line reports the threshold: it is
// provisional and uncalibrated (ADR-0014), and printing a duration here would read
// as a calibrated promise.
//
// The ambiguous-skill-run line is a third distinct fact: how many attributed skill runs
// were collapsed into an already-counted one for the same session and skill (ADR-0023's
// accepted limitation). It is uncertainty about the invocation numbers and never an
// invocation total, which is why it stands apart from the counts above it rather than
// being folded into any of them.
//
// The interpretation of these counters lives in health.Diagnose, because
// internal/cli only parses and prints (ADR-0001, plan §6.2). This function is the
// print loop and holds no decision about what the numbers mean.
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

	diagnosis := health.Diagnose(report, readErr, hookErr)

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

	lastScan := "never"
	if diagnosis.ScanKnown {
		lastScan = diagnosis.ScanAt.UTC().Format(time.RFC3339)
	}
	if _, err := fmt.Fprintf(out, "last scan: %s\n", lastScan); err != nil {
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
		{"pending calls", report.Scan.PendingCalls},
		{"interrupted calls", report.Scan.InterruptedCalls},
		{"ambiguous skill runs", report.Scan.AmbiguousSkillRuns},
	} {
		if _, err := fmt.Fprintf(out, "%s: %d\n", line.key, line.value); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintf(out, "integration: %s\n", diagnosis.State)
	return err
}
