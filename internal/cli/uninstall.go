package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/style"
)

// selfPath is how the command finds the binary it will unlink. A variable for the
// same reason ingest.go's hookChild is one: a test that let this resolve for real
// would delete the binary running the suite, so a test points it at a throwaway file
// instead.
var selfPath = os.Executable

func init() { commands = append(commands, newUninstallCmd) }

// newUninstallCmd is what the registry attaches: the command wired to the real
// terminal.
func newUninstallCmd() *cobra.Command { return newUninstallCmdWith(osPrompter) }

// newUninstallCmdWith takes the prompt factory as a parameter, exactly as
// newRemoteSetCmd does, so a test drives the confirmation against a fake terminal
// instead of needing a real one.
func newUninstallCmdWith(newPrompter promptFactory) *cobra.Command {
	var assumeYes bool
	// Short is one row of `wake --help`, so it stays inside the table every other row
	// fits rather than wrapping the whole listing. Long carries what the disclosure
	// cannot: ADR-0043 §3 puts the irreversibility, what goes, what stays and the
	// less-destructive alternatives somewhere the user can still act on them, because
	// a line printed above a running removal is not such a place.
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove Wake entirely, including this binary",
		Long: "Remove Wake entirely from this machine. This cannot be undone.\n" +
			"\n" +
			"Deleted:\n" +
			"  Wake's own Claude Code hook entry, and nothing else in settings.json\n" +
			"  all collected activity and the local project map (~/.local/state/wake, or\n" +
			"    $WAKE_DIR when it is set)\n" +
			"  configuration and the local identity salt (~/.config/wake)\n" +
			"  this binary, plus the link it was invoked through if there is one\n" +
			"\n" +
			"Kept: nothing of Wake's. A later `wake init` is a fresh install with a new\n" +
			"identity salt, so every repository is re-identified and earlier records no\n" +
			"longer line up with it.\n" +
			"\n" +
			"Less destructive alternatives:\n" +
			"  wake remove          remove the hook entry only; data and configuration stay\n" +
			"  wake remove --purge  ...and delete collected data; configuration stays\n" +
			"\n" +
			"The exact paths are printed and confirmed before anything is deleted. With no\n" +
			"terminal on standard input this command refuses and deletes nothing unless\n" +
			"--yes is given.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pretty := ttyOutput(cmd)
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			claudeDir, err := config.ClaudeCodeDir()
			if err != nil {
				return err
			}
			executable, err := selfPath()
			if err != nil {
				return err
			}
			plan, err := activation.PlanUninstall(paths, claudeDir, executable)
			if err != nil {
				return err
			}
			// Decided before the disclosure, so a run that is going to refuse never
			// promises a deletion it cannot make — the same ordering PlanUninstall uses
			// for the refusals it can pre-check (ADR-0043 §2).
			gate, gateErr := newConfirmer(cmd, newPrompter, assumeYes)
			if gateErr != nil {
				return gateErr
			}
			// The disclosure goes wherever the question is going to be put, which is
			// stderr when there is one to put and stdout when --yes already answered it
			// (confirmer.discloseTo). Printing it to stdout unconditionally would let
			// `wake uninstall > log` ask a person to authorise a deletion whose paths
			// went to the file, and `wake uninstall > log 2>&1` block on stdin with a
			// blank terminal — the same defect ADR-0043 exists to close, one fd over.
			// Styling follows the stream it lands on rather than stdout, or a redirected
			// stderr collects colour codes.
			disclosure := gate.discloseTo(cmd)
			disclosurePretty := ttyWriter(disclosure)
			// Printed before the first removal, and its error returned rather than
			// discarded: ADR-0010 rests on the command showing the exact paths it will
			// modify, so a disclosure that did not reach the user is a consent step that
			// did not happen. Every path comes from the resolved plan rather than a
			// re-joined literal, so the disclosure cannot drift from what gets deleted.
			// It says what happens to the data as well as where it is (ADR-0010, and
			// ADR-0019 §3's "the tool says what happens to the data"), and it names only
			// Wake's own locations — no consented repository root or label. Each path is
			// dimmed rather than plain (style.Paint no-ops when pretty is false, so a
			// test asserting an exact path never sees this) for the same reason init
			// dims its own list: the sentence around a path is what deserves the eye.
			if _, discloseErr := fmt.Fprintf(disclosure,
				"%s\n"+
					"%s  Wake's own hook entry only; your other hooks are left as they are\n"+
					"%s  all collected activity and the local project map\n"+
					"%s  configuration and the local identity salt\n"+
					"%s  this binary\n",
				style.Heading(disclosurePretty, "Wake will permanently delete, and this cannot be undone:"),
				style.Paint(disclosurePretty, style.Dim, plan.SettingsFile), style.Paint(disclosurePretty, style.Dim, plan.DataDir),
				style.Paint(disclosurePretty, style.Dim, plan.ConfigDir), style.Paint(disclosurePretty, style.Dim, plan.Executable),
			); discloseErr != nil {
				return discloseErr
			}
			// A fifth path only when there is one: the plan deletes the file a link
			// resolves to, so an installation reached through a link has the link
			// deleted as well, and ADR-0010's disclosure names everything that goes.
			// Its own line rather than folded into the one above, so the four paths
			// every run prints stay the same four.
			if plan.Launcher != "" {
				if _, discloseErr := fmt.Fprintf(disclosure, "%s  the link this command was invoked through\n", style.Paint(disclosurePretty, style.Dim, plan.Launcher)); discloseErr != nil {
					return discloseErr
				}
			}
			if _, discloseErr := fmt.Fprintln(disclosure, "To keep your configuration, use `wake remove --purge` instead."); discloseErr != nil {
				return discloseErr
			}
			// The question, after every path has been named and before anything has
			// been deleted (ADR-0043 §1). A --yes gate has nothing to ask and proceeds;
			// any answer that is not a yes deletes nothing and says so, at exit 0,
			// because declining a deletion is not a failure.
			proceed, askErr := gate.ask("Permanently delete all of this? [y/N]: ")
			if askErr != nil {
				return askErr
			}
			if !proceed {
				// Onto the stream that carried the question, for the reason the
				// disclosure went there: a person who answered no at a terminal with
				// stdout redirected still has to be told plainly that nothing was
				// deleted (ADR-0043 §1).
				_, abortErr := fmt.Fprintln(disclosure, nothingDeleted)
				return abortErr
			}
			var removed bool
			spinErr := style.WithSpinner(cmd.OutOrStdout(), pretty, "Removing Wake", func() error {
				var removeErr error
				removed, removeErr = plan.Remove()
				return removeErr
			})
			if spinErr != nil {
				// What the removal managed before it stopped, printed before the error
				// itself: the disclosure has already named four paths as about to be
				// deleted, so an error on its own leaves the reader unable to tell which
				// of them survived. `removed` is the one step the sequence can still
				// report through a failure, and it is never claimed in the negative —
				// a data root that failed part way through leaves removed false with
				// the hook entry already gone, so the other branch says only what it
				// knows. The binary is always still there, because activation removes
				// it last, which is what makes the retry possible.
				report := "The removal stopped part way; whatever it had not reached is still in place."
				if removed {
					report = "Removed Wake's Claude Code integration, then the removal stopped part way."
				}
				if _, printErr := fmt.Fprintf(cmd.OutOrStdout(), "%s This binary was not removed — run `wake uninstall` again once the reported problem is fixed.\n", report); printErr != nil {
					return printErr
				}
				return spinErr
			}
			if removed {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Removed Wake's Claude Code integration.")
			} else {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Wake's Claude Code integration was not installed.")
			}
			if err != nil {
				return err
			}
			// The state it leaves behind, not a list of removals it performed: on a
			// machine that was never `init`ed there is no data root and no config root
			// to remove, and "Removed …" would be claiming work that never happened.
			// "are gone" is true either way, which is the only thing the user is
			// checking here — and true either way is what earns it the checkmark.
			confirmation := "Wake's local data, configuration and binary are gone. A later `wake init` is a fresh install with a new identity salt.\n"
			if pretty {
				confirmation = style.Paint(pretty, style.Green, "✓") + " " + confirmation
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), confirmation)
			return err
		},
	}
	// No -y shorthand. This is the first flag in the CLI whose purpose is to skip a
	// safety gate (ADR-0043 § Consequences), and a one-letter form is the one a hand
	// slips onto.
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "delete without asking for confirmation")
	return cmd
}
