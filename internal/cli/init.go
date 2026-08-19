package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
)

func init() { commands = append(commands, newInitCmd) }

func newInitCmd() *cobra.Command {
	var full bool
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
		//
		// The list is the files *this* invocation writes, not the files init can
		// write (ADR-0024): the event spool appears only under --full, because
		// without it no history is read and nothing is appended. It is still named in
		// the sentence below, so the answer to "will my spool be touched" is plain
		// either way, and --full is named in the output that used to just do it.
		spool := filepath.Join(paths.DataDir, "events.ndjson")
		modifies := []string{
			paths.ConfigFile,
			paths.ProjectsFile,
			paths.HealthFile,
			filepath.Join(claudeDir, "settings.json"),
		}
		history := fmt.Sprintf("Existing Claude Code history will not be imported, so %s is not written. Run \"wake init --full\" to import it now.", spool)
		if full {
			modifies = append(modifies, spool)
			history = "Existing Claude Code history will be imported now."
		}
		// One Fprintf, so a write that fails cannot leave half a disclosure on screen.
		if _, discloseErr := fmt.Fprintf(cmd.OutOrStdout(), "Wake will modify:\n%s\n%s\n", strings.Join(modifies, "\n"), history); discloseErr != nil {
			return discloseErr
		}
		written, err := activation.Init(paths, root, claudeDir, self, full)
		if err != nil {
			return err
		}
		// Two lines, because one number cannot carry both meanings: written is a count
		// of terminal events (ADR-0015), and reporting 0 of them on a path that never
		// looked would read as an import that found nothing.
		confirmation := "Claude Code collection enabled; collection starts now. Run \"wake init --full\" or \"wake ingest\" to import existing history.\n"
		if full {
			confirmation = fmt.Sprintf("Claude Code collection enabled; imported %d terminal events.\n", written)
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), confirmation)
		return err
	}}
	cmd.Flags().BoolVar(&full, "full", false, "also import this project's existing Claude Code history now")
	return cmd
}
