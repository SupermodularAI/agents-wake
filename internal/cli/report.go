package cli

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/report"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

func init() { commands = append(commands, newReportCmd) }

func newReportCmd() *cobra.Command {
	var usage bool
	var unused bool
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Show current local activity in the terminal",
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
			root, err := os.Getwd()
			if err != nil {
				return err
			}
			events := store.New(filepath.Join(paths.DataDir, "events.ndjson"))
			primitives := inventory.New(paths.PrimitivesFile)
			if err := primitives.Refresh(events, inventory.ClaudeCode(home, root)); err != nil {
				return err
			}
			return report.Print(cmd.OutOrStdout(), events, primitives, report.Options{Usage: usage, Unused: unused})
		},
	}
	cmd.Flags().BoolVar(&usage, "usage", false, "show only primitive activity")
	cmd.Flags().BoolVar(&unused, "unused", false, "show only unused primitives")
	return cmd
}
