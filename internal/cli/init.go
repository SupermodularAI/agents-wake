package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
)

func init() { commands = append(commands, newInitCmd) }

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "init", Short: "Enable local Claude Code collection for this project", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		root, err := config.DiscoverRootForRegistration()
		if err != nil {
			return err
		}
		paths, err := config.ResolvePaths()
		if err != nil {
			return err
		}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		claudeDir, err := config.ClaudeCodeDir()
		if err != nil {
			return err
		}
		// The error is returned rather than discarded, and returned before any state
		// is modified: ADR-0010 rests on `init` disclosing the exact files it will
		// change, so a disclosure that did not reach the user is a consent step that
		// did not happen. Every path comes from the resolved Paths rather than a
		// re-joined literal, so the disclosure cannot drift from where the file goes.
		if _, discloseErr := fmt.Fprintf(cmd.OutOrStdout(), "Wake will modify:\n%s\n%s\n%s\n%s\n%s\n",
			paths.ConfigFile,
			paths.ProjectsFile,
			paths.HealthFile,
			filepath.Join(claudeDir, "settings.json"),
			filepath.Join(paths.DataDir, "events.ndjson"),
		); discloseErr != nil {
			return discloseErr
		}
		written, err := activation.Init(paths, root, claudeDir, self)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Claude Code collection enabled; imported %d terminal events.\n", written)
		return err
	}}
	return cmd
}
