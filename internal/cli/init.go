package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
)

func init() { commands = append(commands, newInitCmd) }

func newInitCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{Use: "init", Short: "Enable local Claude Code collection for this project", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		root := cwd
		if output, gitErr := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); gitErr == nil {
			root = strings.TrimSpace(string(output))
		}
		paths, err := config.ResolvePaths()
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		claudeDir := filepath.Join(home, ".claude")
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Wake will modify:\n%s\n%s\n%s\n%s\n", paths.ConfigFile, paths.ProjectsFile, filepath.Join(claudeDir, "settings.json"), filepath.Join(paths.DataDir, "events.ndjson"))
		if !yes {
			return errors.New("re-run with --yes to enable collection")
		}
		written, err := activation.Init(paths, root, claudeDir)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Claude Code collection enabled; imported %d terminal events.\n", written)
		return err
	}}
	cmd.Flags().BoolVar(&yes, "yes", false, "apply the displayed local integration")
	return cmd
}
