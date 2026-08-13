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
	cmd := &cobra.Command{Use: "ingest", Short: "Import activity for consented projects", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		paths, err := config.ResolvePaths()
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		written, err := activation.Ingest(paths, filepath.Join(home, ".claude"))
		if err != nil {
			return err
		}
		if !quiet {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Imported %d terminal events.\n", written)
		}
		return err
	}}
	cmd.Flags().BoolVar(&quiet, "quiet", false, "produce no output")
	return cmd
}
