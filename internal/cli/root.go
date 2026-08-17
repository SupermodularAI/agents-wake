// Package cli parses arguments and prints output. No logic lives here — every
// command delegates to a package under internal/, so the work stays testable
// without stdin or stdout (plan §6.2, ADR-0001).
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
	"github.com/SupermodularAI/agents-wake/internal/version"
)

// newRootCmd builds the root command. It carries no subcommands yet: T001 is
// the scaffold, and every command in plan §7.3 arrives with the phase that
// implements it.
//
// Persistent flags are also deliberately absent. The first real one is the
// store-directory override, which belongs to internal/config (T002) — adding a
// flag here before the package that resolves it exists would put logic in the
// one place ADR-0001 says must not have any.
func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wake",
		Short: "Measure which agent primitives you actually use, across every harness",
		Long: "wake reads the storage your agent harnesses already write and answers which\n" +
			"primitives you use, which you pay for and never use, and which are quietly\n" +
			"broken. Everything stays on this machine.",
		Version: version.String(),
		// Reject stray positional arguments. Without this, a root command with
		// no subcommands accepts anything and exits 0 — `wake bogus` would look
		// like it worked, which is the wrong default for a tool whose output an
		// agent consumes non-interactively (plan §8).
		Args: cobra.NoArgs,
		// The scaffold has nothing to run, so bare `wake` prints help. Being
		// runnable also makes cobra render the Usage and Flags sections, which
		// it omits entirely for a non-runnable command.
		//
		// This is where plan §7.3's split lands once there is something to show:
		// a dashboard for a TTY, deterministic text when stdout is not one.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file, ok := cmd.OutOrStdout().(*os.File); ok && isTerminal(file) {
				return newServeCmd().Execute()
			}
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			entries, err := store.New(filepath.Join(paths.DataDir, "events.ndjson")).Entries(0)
			if err != nil {
				return err
			}
			records := make([]record.Record, 0, len(entries))
			for _, entry := range entries {
				records = append(records, entry.Record)
			}
			summary := metrics.Aggregate(records)
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "terminal invocations: %d\ndistinct sessions: %d\n", summary.Invocations, summary.Sessions)
			return err
		},
		// Usage on an error would bury the error itself.
		SilenceUsage: true,
	}
	cmd.SetVersionTemplate("{{.Version}}\n")
	for _, newCmd := range commands {
		cmd.AddCommand(newCmd())
	}
	return cmd
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Execute runs the root command and returns a process exit code.
func Execute() int {
	if err := newRootCmd().Execute(); err != nil {
		return 1
	}
	return 0
}
