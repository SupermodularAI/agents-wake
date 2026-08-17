package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
)

func init() { commands = append(commands, newIngestCmd) }
func newIngestCmd() *cobra.Command {
	var quiet bool
	var rebuild bool
	cmd := &cobra.Command{Use: "ingest", Short: "Import activity for consented projects", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		paths, err := config.ResolvePaths()
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		claudeDir := filepath.Join(home, ".claude")
		var written int
		if rebuild {
			written, err = activation.Rebuild(paths, claudeDir)
		} else {
			written, err = activation.Ingest(paths, claudeDir)
		}
		if err != nil {
			return err
		}
		if !quiet {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Imported %d terminal events.\n", written)
		}
		return err
	}}
	cmd.Flags().BoolVar(&quiet, "quiet", false, "produce no output")
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "rebuild the derived event store from consented history")
	return cmd
}
