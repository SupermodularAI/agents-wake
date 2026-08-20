// Package cli parses arguments and prints output. No logic lives here — every
// command delegates to a package under internal/, so the work stays testable
// without stdin or stdout (plan §6.2, ADR-0001).
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/platform"
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

// terminalEvents renders a count of terminal events with a noun that agrees with it.
//
// Shared by the two commands that report an import, so the same quantity is not
// spelled two ways one release apart. "1 terminal events" is the kind of detail that
// makes a number look machine-generated in a tool whose whole claim is that its
// numbers were derived carefully (ADR-0015 names the grain).
func terminalEvents(count int) string {
	if count == 1 {
		return "1 terminal event"
	}
	return fmt.Sprintf("%d terminal events", count)
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// ttyOutput decides whether a command may draw color, box-drawing, or an
// animated spinner on the strength of one thing only: is stdout a real
// terminal right now. There is no override — piped, redirected, or (in a
// test) writing to a bytes.Buffer, cmd.OutOrStdout() is not an *os.File and
// this returns false, which is what keeps every plain-text assertion in this
// package's tests exactly what it was before any renderer here learned to be
// pretty (ADR-0011, plan §7.3, §8).
func ttyOutput(cmd *cobra.Command) bool {
	file, ok := cmd.OutOrStdout().(*os.File)
	return ok && isTerminal(file)
}

// Execute runs the root command and returns a process exit code.
func Execute() int { return execute(runtime.GOOS, os.Stderr) }

// execute refuses an unsupported platform before the command tree runs, so a user
// on one is told at startup rather than midway through a locked read-modify-write
// (ADR-0021). goos and the stream are parameters because the refusal cannot
// otherwise be exercised from a supported platform's test run.
func execute(goos string, stderr io.Writer) int {
	if err := platform.Check(goos); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err := newRootCmd().Execute(); err != nil {
		return 1
	}
	return 0
}
