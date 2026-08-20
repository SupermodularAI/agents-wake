package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/selfupdate"
	"github.com/SupermodularAI/agents-wake/internal/version"
)

func init() { commands = append(commands, newUpdateCmd) }

// newFetcher is how update reaches curl. A variable for the same reason
// uninstall.go's selfPath is one: a test must drive the whole command without a
// network and without curl installed.
var newFetcher = func() (selfupdate.Fetcher, error) {
	fetcher, err := selfupdate.NewCurlFetcher()
	if err != nil {
		return nil, err
	}
	return fetcher, nil
}

// runningVersion is version.Version behind a function so a test can drive both a
// tagged build and an untagged one.
var runningVersion = func() string { return version.Version }

func newUpdateCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Install the newest release, or check whether one exists",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			running := runningVersion()
			// First, and before anything is constructed: a build outside the
			// release pipelines has no tag to compare against, and saying so is
			// the whole answer. Nothing reaches PATH or the network to produce it,
			// for --check and for the bare command alike (ADR-0026) — and
			// refusing to overwrite a locally built binary is not a failure, so
			// this exits 0.
			if selfupdate.IsUntagged(running) {
				_, err := fmt.Fprintln(out, "not a tagged build; nothing to compare against")
				return err
			}
			// The one prerequisite beyond the binary itself, reported as itself
			// rather than as an exec error.
			fetcher, err := newFetcher()
			if err != nil {
				return err
			}
			executable, err := selfPath()
			if err != nil {
				return err
			}
			updater := selfupdate.Updater{
				Fetch:      fetcher,
				GOOS:       runtime.GOOS,
				GOARCH:     runtime.GOARCH,
				Running:    running,
				Executable: executable,
			}

			if check {
				tag, latestErr := updater.Latest()
				if latestErr != nil {
					return latestErr
				}
				if selfupdate.Compare(running, tag) == selfupdate.StatusCurrent {
					_, err = fmt.Fprintf(out, "already on the latest release (%s)\n", tag)
					return err
				}
				_, err = fmt.Fprintf(out, "%s is available (running %s)\n", tag, running)
				return err
			}

			result, err := updater.Apply()
			if err != nil {
				return err
			}
			// Nothing was downloaded and nothing written, so the line says what
			// happened rather than claiming an install.
			if !result.Replaced {
				_, err = fmt.Fprintf(out, "already on the latest release (%s)\n", result.Tag)
				return err
			}
			_, err = fmt.Fprintf(out, "updated to %s\n", result.Tag)
			return err
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "report whether a newer release exists, without downloading it")
	return cmd
}
