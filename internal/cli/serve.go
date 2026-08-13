package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/store"
	"github.com/SupermodularAI/agents-wake/internal/ui"
)

func init() { commands = append(commands, newServeCmd) }

func newServeCmd() *cobra.Command {
	var port int
	var noOpen bool
	cmd := &cobra.Command{Use: "serve", Short: "Serve the local dashboard", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if port < 1 || port > 65535 {
			return fmt.Errorf("port must be between 1 and 65535")
		}
		paths, err := config.ResolvePaths()
		if err != nil {
			return err
		}
		if !noOpen {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Serving dashboard at http://127.0.0.1:%d\n", port)
		}
		return ui.ListenAndServe(port, store.New(filepath.Join(paths.DataDir, "events.ndjson")))
	}}
	cmd.Flags().IntVar(&port, "port", 8080, "local port to serve")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "do not open a browser")
	return cmd
}
