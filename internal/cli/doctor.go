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
// The two collection-boundary lines sit beside the refused-entry count because they
// are the same family — what registration could not do — and they are two lines for
// the same reason pending and interrupted are: a directory that is gone is an honest
// zero, since there is nothing left there to read, and a registration that was refused
// is collection that was lost. Neither moves the state word below, and the refused one
// not moving it is a decision argued in Diagnose: every scan re-observes the same
// directory and refuses it again, so a state word driven by that counter could never
// change back. These lines are what report the loss, which is why they are printed
// whatever the state word says.
//
// The refused-subagent-run line sits beside the refused-call count for the same
// family reason, and it is a second line rather than part of that count because the two
// are read differently below: a call refused while a source was read is what a harness
// renaming a primitive's identity field looks like, and it moves the state word. A
// subagent run refused for want of a name is lost collection too, but it is a standing
// fact about a transcript — ADR-0036 §2 refuses to name those runs, and every scan
// re-reads the whole history and refuses the same ones — so it deliberately does not,
// for the reason Diagnose argues. This line is what reports the loss, which is why it
// prints whatever the state word says.
//
// The stale-record count and the store-rebuild word are two lines for the same reason:
// the count says how many records the store holds that this build cannot read, and the
// word says whether anything has re-derived them. The scan that found them may not have
// been allowed to — the one a hook fires collects inside each repository's recorded
// boundary (ADR-0025), so it would re-derive less than it deleted — and the word is
// what keeps the count from reading as the report of a rebuild that never happened. It
// is the one line here that names a command, because it is the one state whose remedy
// is a command the user has to type.
//
// They live in this function because every health.Scan counter does; the boundary's own
// state word needs the project table, which this function knows nothing about, so it
// arrives through the seam instead (doctor_boundary.go).
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
		{"records from an earlier schema version", report.Scan.StaleRecords},
		{"refused project entries", report.Scan.RefusedProjects},
		{"global boundary directories gone", report.Scan.BoundarySkipped},
		{"global boundary registrations refused", report.Scan.BoundaryRefused},
		{"refused calls", report.Scan.RefusedCalls},
		{"refused subagent runs", report.Scan.RefusedSubagentRuns},
		{"pending calls", report.Scan.PendingCalls},
		{"interrupted calls", report.Scan.InterruptedCalls},
		{"ambiguous skill runs", report.Scan.AmbiguousSkillRuns},
	} {
		if _, err := fmt.Fprintf(out, "%s: %d\n", line.key, line.value); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(out, "store rebuild: %s\n", diagnosis.StoreRebuild); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(out, "integration: %s\n", diagnosis.State); err != nil {
		return err
	}

	// The seam, after every line this function owns: a feature with extra
	// diagnosis to add appends a section from its own init() rather than
	// editing this file (extensions.go).
	return writeDiagnosisSections(out, paths)
}
