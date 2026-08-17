package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/store"
	"github.com/SupermodularAI/agents-wake/internal/ui"
)

func init() { commands = append(commands, newServeCmd) }

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "serve", Short: "Serve the local dashboard", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		paths, err := config.ResolvePaths()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Serving dashboard at http://127.0.0.1:8080")
		scope, err := resolveDiscoveryScope(cmd, paths)
		if err != nil {
			return err
		}
		events := store.New(filepath.Join(paths.DataDir, "events.ndjson"))
		primitives := inventory.New(paths.PrimitivesFile)
		if err := primitives.Refresh(events, inventory.ClaudeCodeInScope(scope)); err != nil {
			return err
		}
		return ui.ListenAndServe(8080, events, primitives)
	}}
	return cmd
}
