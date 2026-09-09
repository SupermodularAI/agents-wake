package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/style"
)

func init() { commands = append(commands, newRemoveCmd) }

// newRemoveCmd is what the registry attaches: the command wired to the real terminal.
func newRemoveCmd() *cobra.Command { return newRemoveCmdWith(osPrompter) }

// newRemoveCmdWith takes the prompt factory as a parameter, exactly as
// newRemoteSetCmd does, so a test drives `--purge`'s confirmation against a fake
// terminal instead of needing a real one.
func newRemoveCmdWith(newPrompter promptFactory) *cobra.Command {
	var purge bool
	var assumeYes bool
	cmd := &cobra.Command{Use: "remove", Short: "Remove Wake's Claude Code integration", Long: "Remove Wake's Claude Code hook entry. Your other hooks are left as they are.\n" +
		"\n" +
		"Forms:\n" +
		"  wake remove          remove the hook entry; keep every collected record\n" +
		"  wake remove --purge  ...and delete every collected record on this machine\n" +
		"\n" +
		"Either form keeps ~/.config/wake — configuration and the local identity salt —\n" +
		"so a later `wake init` keeps the same repository identity. `wake uninstall` is\n" +
		"the form that removes those and the binary as well, and cannot be undone.\n" +
		"\n" +
		"--purge prints the exact paths and asks before deleting; with no terminal on\n" +
		"standard input it refuses and deletes nothing unless --yes is given. Plain\n" +
		"`wake remove` changes only the hook entry, is not gated, and is undone by\n" +
		"running `wake init` again.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		pretty := ttyOutput(cmd)
		paths, err := config.ResolvePaths()
		if err != nil {
			return err
		}
		claudeDir, err := config.ClaudeCodeDir()
		if err != nil {
			return err
		}
		if purge {
			// Only --purge is gated. Plain `remove` takes the hook entry and nothing
			// else, is undone by `wake init`, and gating it would train the user to
			// answer yes without reading — which is what costs the gate its value on
			// the commands that need it (ADR-0043 §1).
			gate, gateErr := newConfirmer(cmd, newPrompter, assumeYes)
			if gateErr != nil {
				return gateErr
			}
			// The disclosure `--purge` never had. Same shape as uninstall's: a
			// heading, then one dimmed path per line with what happens to it, every
			// path resolved through the same helper the removal uses rather than
			// re-joined here. It does not name the identity salt as deleted — the salt
			// sits under the config root, which --purge keeps, and claiming otherwise
			// would tell the user their repositories are about to be re-identified
			// when they are not (ADR-0010, ADR-0019 §3).
			if _, discloseErr := fmt.Fprintf(cmd.OutOrStdout(),
				"%s\n"+
					"%s  Wake's own hook entry only; your other hooks are left as they are\n"+
					"%s  all collected activity and the local project map\n"+
					"Configuration and the local identity salt at %s are kept; `wake uninstall` removes those too.\n",
				style.Heading(pretty, "Wake will permanently delete:"),
				style.Paint(pretty, style.Dim, activation.SettingsFilePath(claudeDir)),
				style.Paint(pretty, style.Dim, paths.DataDir),
				style.Paint(pretty, style.Dim, paths.ConfigDir),
			); discloseErr != nil {
				return discloseErr
			}
			proceed, askErr := gate.ask("Permanently delete this data? [y/N]: ")
			if askErr != nil {
				return askErr
			}
			if !proceed {
				_, abortErr := fmt.Fprintln(cmd.OutOrStdout(), nothingDeleted)
				return abortErr
			}
		}
		label := "Removing Wake's Claude Code integration"
		if purge {
			label = "Removing Wake's Claude Code integration and local data"
		}
		var removed bool
		spinErr := style.WithSpinner(cmd.OutOrStdout(), pretty, label, func() error {
			var removeErr error
			removed, removeErr = activation.Uninstall(paths, claudeDir, purge)
			return removeErr
		})
		if spinErr != nil {
			return spinErr
		}
		if removed {
			line := "Removed Wake's Claude Code integration."
			if pretty {
				line = style.Paint(pretty, style.Green, "✓") + " " + line
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), line)
		} else {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Wake's Claude Code integration was not installed.")
		}
		if err != nil {
			return err
		}
		if purge {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed local data at %s.\n", style.Paint(pretty, style.Dim, paths.DataDir))
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Local data was kept at %s. Remove it with `wake remove --purge`.\n", style.Paint(pretty, style.Dim, paths.DataDir))
		return err
	}}
	cmd.Flags().BoolVar(&purge, "purge", false, "remove Wake's local data")
	// Accepted without --purge, where it does nothing: erroring would break a script
	// that passes it defensively, and plain `remove` is not gated in the first place.
	// No -y shorthand, for the reason uninstall's carries none.
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "with --purge, delete without asking for confirmation")
	return cmd
}
