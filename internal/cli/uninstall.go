package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
)

// selfPath is how the command finds the binary it will unlink. A variable for the
// same reason ingest.go's hookChild is one: a test that let this resolve for real
// would delete the binary running the suite, so a test points it at a throwaway file
// instead.
var selfPath = os.Executable

func init() { commands = append(commands, newUninstallCmd) }

func newUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove Wake entirely: the Claude Code integration, all local data and configuration, and this binary",
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
			// Wake's own locations — no consented repository root or label.
			if _, discloseErr := fmt.Fprintf(cmd.OutOrStdout(),
				"Wake will permanently delete, and this cannot be undone:\n"+
					"%s  Wake's own hook entry only; your other hooks are left as they are\n"+
					"%s  all collected activity and the local project map\n"+
					"%s  configuration and the local identity salt\n"+
					"%s  this binary\n"+
					"To keep your configuration, use `wake remove --purge` instead.\n",
				plan.SettingsFile, plan.DataDir, plan.ConfigDir, plan.Executable,
			); discloseErr != nil {
				return discloseErr
			}
			removed, err := plan.Remove()
			if err != nil {
				return err
			}
			if removed {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Removed Wake's Claude Code integration.")
			} else {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Wake's Claude Code integration was not installed.")
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Removed Wake's local data, configuration and binary. A later `wake init` is a fresh install with a new identity salt.")
			return err
		},
	}
}
