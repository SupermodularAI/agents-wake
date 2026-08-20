package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/detach"
	"github.com/SupermodularAI/agents-wake/internal/style"
)

// discard consumes an error the hook-invoked path is forbidden to report.
//
// ADR-0016: "exit 0 in silence on any failure; collecting nothing beats disturbing
// a session." A trigger that printed to a session's stderr, or that exited
// non-zero, is a trigger the user turns off — and because every id is derived from
// the source event (ADR-0004), a skipped scan costs nothing a later one cannot
// recover. Naming the discard makes it one reviewable decision instead of blank
// assignments scattered across the path; the counters in health.json, which
// `wake doctor` reads, are where the failure actually goes.
func discard(error) {}

// hookChild is what the quiet trigger re-execs. It is a variable so a test can
// point it somewhere other than the test binary, which would otherwise run the
// whole suite in a detached background process.
var hookChild = detach.Start

func init() { commands = append(commands, newIngestCmd) }
func newIngestCmd() *cobra.Command {
	var quiet bool
	var rebuild bool
	var hookScan bool
	cmd := &cobra.Command{Use: "ingest", Short: "Import activity for consented projects", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		// Both quiet forms return nil unconditionally. The order matters only
		// because --hook-scan is the child of --quiet and carries both flags.
		if hookScan {
			runHookScan()
			return nil
		}
		if quiet {
			spawnHookScan()
			return nil
		}

		paths, err := config.ResolvePaths()
		if err != nil {
			return err
		}
		claudeDir, err := config.ClaudeCodeDir()
		if err != nil {
			return err
		}
		label := "Importing activity for consented projects"
		if rebuild {
			label = "Rebuilding the derived event store"
		}
		var written int
		spinErr := style.WithSpinner(cmd.OutOrStdout(), ttyOutput(cmd), label, func() error {
			var scanErr error
			if rebuild {
				written, scanErr = activation.Rebuild(paths, claudeDir)
			} else {
				written, scanErr = activation.Ingest(paths, claudeDir)
			}
			return scanErr
		})
		if spinErr != nil {
			return spinErr
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Imported %s.\n", terminalEvents(written))
		return err
	}}
	cmd.Flags().BoolVar(&quiet, "quiet", false, "run as the Claude Code trigger: scan in a detached child, print nothing, and always exit 0")
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "rebuild the derived event store from consented history")
	cmd.Flags().BoolVar(&hookScan, "hook-scan", false, "internal: the detached scan --quiet starts")
	// Assigned rather than set through MarkHidden, which returns an error errcheck
	// would flag on a path where there is nothing honest to do with it.
	cmd.Flags().Lookup("hook-scan").Hidden = true
	return cmd
}

// spawnHookScan starts the scan in a detached child and returns immediately, so the
// hook never delays the session that fired it.
func spawnHookScan() {
	self, err := os.Executable()
	if err != nil {
		discard(err)
		return
	}
	discard(hookChild([]string{self, "ingest", "--quiet", "--hook-scan"}))
}

// runHookScan is the detached child: take the single-flight lock, scan, record the
// counters, exit 0 whatever happened.
func runHookScan() {
	paths, err := config.ResolvePaths()
	if err != nil {
		discard(err)
		return
	}
	// The same resolution as the interactive form, disposed of differently on
	// purpose: this path exits 0 in silence whatever happened (ADR-0016), so the
	// error goes to discard rather than to the session's terminal.
	claudeDir, err := config.ClaudeCodeDir()
	if err != nil {
		discard(err)
		return
	}
	_, err = activation.Trigger(paths, claudeDir)
	discard(err)
}
