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

func newUninstallCmd() *cobra.Command {
	// Short is one row of `wake --help`, so it stays inside the table every other row
	// fits rather than wrapping the whole listing; the detail is in the disclosure the
	// command prints, which the user reads at the moment it matters.
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove Wake entirely, including this binary",
		Args:  cobra.NoArgs,
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
			if _, discloseErr := fmt.Fprintf(cmd.OutOrStdout(),
				"%s\n"+
					"%s  Wake's own hook entry only; your other hooks are left as they are\n"+
					"%s  all collected activity and the local project map\n"+
					"%s  configuration and the local identity salt\n"+
					"%s  this binary\n",
				style.Heading(pretty, "Wake will permanently delete, and this cannot be undone:"),
				style.Paint(pretty, style.Dim, plan.SettingsFile), style.Paint(pretty, style.Dim, plan.DataDir),
				style.Paint(pretty, style.Dim, plan.ConfigDir), style.Paint(pretty, style.Dim, plan.Executable),
			); discloseErr != nil {
				return discloseErr
			}
			// A fifth path only when there is one: the plan deletes the file a link
			// resolves to, so an installation reached through a link has the link
			// deleted as well, and ADR-0010's disclosure names everything that goes.
			// Its own line rather than folded into the one above, so the four paths
			// every run prints stay the same four.
			if plan.Launcher != "" {
				if _, discloseErr := fmt.Fprintf(cmd.OutOrStdout(), "%s  the link this command was invoked through\n", style.Paint(pretty, style.Dim, plan.Launcher)); discloseErr != nil {
					return discloseErr
				}
			}
			if _, discloseErr := fmt.Fprintln(cmd.OutOrStdout(), "To keep your configuration, use `wake remove --purge` instead."); discloseErr != nil {
				return discloseErr
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
}
