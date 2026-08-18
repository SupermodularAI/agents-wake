package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
)

func init() { commands = append(commands, newRemoveCmd) }

func newRemoveCmd() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{Use: "remove", Short: "Remove Wake's Claude Code integration", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		paths, err := config.ResolvePaths()
		if err != nil {
			return err
		}
		claudeDir, err := config.ClaudeCodeDir()
		if err != nil {
			return err
		}
		removed, err := activation.Uninstall(paths, claudeDir, purge)
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
		if purge {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed local data at %s.\n", paths.DataDir)
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Local data was kept at %s. Remove it with `wake remove --purge`.\n", paths.DataDir)
		return err
	}}
	cmd.Flags().BoolVar(&purge, "purge", false, "remove Wake's local data")
	return cmd
}
