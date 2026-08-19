package cli

import (
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/report"
	"github.com/SupermodularAI/agents-wake/internal/store"
	"github.com/SupermodularAI/agents-wake/internal/style"
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
			pretty := ttyOutput(cmd)
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			scope, _, err := resolveDiscoveryScope(cmd, paths)
			if err != nil {
				return err
			}
			events := store.New(filepath.Join(paths.DataDir, "events.ndjson"))
			primitives := inventory.New(paths.PrimitivesFile)
			refreshErr := style.WithSpinner(cmd.OutOrStdout(), pretty, "Refreshing primitive inventory", func() error {
				discovery, discoverErr := discoverAllRepos(paths, scope.ClaudeDir)
				if discoverErr != nil {
					return discoverErr
				}
				return primitives.Refresh(events, discovery)
			})
			if refreshErr != nil {
				return refreshErr
			}
			options := report.Options{Usage: usage, Unused: unused, Pretty: pretty}
			return report.Print(cmd.OutOrStdout(), events, primitives, options)
		},
	}
	cmd.Flags().BoolVar(&usage, "usage", false, "show only primitive activity")
	cmd.Flags().BoolVar(&unused, "unused", false, "show only unused primitives")
	return cmd
}
