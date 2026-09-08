package cli

import (
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/repolabel"
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
			// Resolved here for the reason the labels below are: internal/cli is the
			// only layer holding config.Paths. It is handed to the refresh *and* to the
			// renderer, so the persisted snapshot and this run's report count under the
			// same grain.
			rollup := metrics.RepoRollup(config.RepoRollup(paths))
			refreshErr := style.WithSpinner(cmd.OutOrStdout(), pretty, "Refreshing primitive inventory", func() error {
				discovery, discoverErr := discoverAllRepos(paths, scope.ClaudeDir)
				if discoverErr != nil {
					return discoverErr
				}
				return primitives.Refresh(events, discovery, rollup)
			})
			if refreshErr != nil {
				return refreshErr
			}
			// Resolved here because internal/cli is the only layer holding
			// config.Paths: a renderer never reads the file the labels come from.
			options := report.Options{Usage: usage, Unused: unused, Pretty: pretty, Labels: repolabel.Labels(config.ProjectLabels(paths)), Rollup: rollup}
			return report.Print(cmd.OutOrStdout(), events, primitives, options)
		},
	}
	cmd.Flags().BoolVar(&usage, "usage", false, "show only primitive activity")
	cmd.Flags().BoolVar(&unused, "unused", false, "show only unused primitives")
	return cmd
}
