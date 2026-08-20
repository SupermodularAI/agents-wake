package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/style"
)

func init() { commands = append(commands, newRemoveCmd) }

func newRemoveCmd() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{Use: "remove", Short: "Remove Wake's Claude Code integration", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		pretty := ttyOutput(cmd)
		paths, err := config.ResolvePaths()
		if err != nil {
			return err
		}
		claudeDir, err := config.ClaudeCodeDir()
		if err != nil {
			return err
		}
		label := "Removing Wake's Claude Code integration"
		if purge {
			label = "Removing Wake's Claude Code integration and local data"
		}
		var removed bool
		spinErr := style.WithSpinner(cmd.OutOrStdout(), pretty, label, func() error {
			var removeErr error
			removed, removeErr = activation.Uninstall(paths, claudeDir, purge)
			return removeErr
		})
		if spinErr != nil {
			return spinErr
		}
		if removed {
			line := "Removed Wake's Claude Code integration."
			if pretty {
				line = style.Paint(pretty, style.Green, "✓") + " " + line
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), line)
		} else {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Wake's Claude Code integration was not installed.")
		}
		if err != nil {
			return err
		}
		if purge {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed local data at %s.\n", style.Paint(pretty, style.Dim, paths.DataDir))
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Local data was kept at %s. Remove it with `wake remove --purge`.\n", style.Paint(pretty, style.Dim, paths.DataDir))
		return err
	}}
	cmd.Flags().BoolVar(&purge, "purge", false, "remove Wake's local data")
	return cmd
}
