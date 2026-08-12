package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// withRegistered appends a command for the duration of one test and restores the
// registry afterwards, so tests do not leak commands into each other. Ten later
// tickets rely on this mechanism, so it is worth proving before any of them does.
func withRegistered(t *testing.T, use string) {
	t.Helper()
	saved := commands
	t.Cleanup(func() { commands = saved })
	commands = append(commands, func() *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: "probe command registered by a test",
			RunE:  func(cmd *cobra.Command, _ []string) error { return nil },
		}
	})
}

func TestRegisteredCommandIsAttachedToRoot(t *testing.T) {
	withRegistered(t, "probe")

	var found bool
	for _, c := range newRootCmd().Commands() {
		if c.Name() == "probe" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("a command appended to the registry was not attached to the root command")
	}
}

func TestRegisteredCommandIsDispatchable(t *testing.T) {
	withRegistered(t, "probe")

	out, err := run(t, "probe")
	if err != nil {
		t.Fatalf("running the registered command failed: %v (output: %s)", err, out)
	}
}

func TestRegisteredCommandAppearsInHelp(t *testing.T) {
	withRegistered(t, "probe")

	out, _ := run(t)
	if !strings.Contains(out, "probe") {
		t.Errorf("registered command is missing from help output:\n%s", out)
	}
}

// Root-level Args validation must survive having subcommands attached: cobra
// dispatches a matching subcommand before applying the root's own Args, and an
// unknown name must still be an error rather than a silent success.
func TestUnknownArgumentStillFailsWithCommandsRegistered(t *testing.T) {
	withRegistered(t, "probe")

	if _, err := run(t, "bogus"); err == nil {
		t.Fatal("expected an error for an unknown argument, got nil")
	}
}
