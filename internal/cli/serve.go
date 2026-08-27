package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/repolabel"
	"github.com/SupermodularAI/agents-wake/internal/store"
	"github.com/SupermodularAI/agents-wake/internal/style"
	"github.com/SupermodularAI/agents-wake/internal/ui"
)

func init() { commands = append(commands, newServeCmd) }

// dashboardPort is the loopback port the dashboard binds. A variable, not a
// constant, so a test can point it at a port it controls; there is no flag,
// because the port is part of the URL the README documents.
var dashboardPort = 8080

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "serve", Short: "Serve the local dashboard", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return runServe(cmd, dashboardPort)
	}}
	return cmd
}

// runServe resolves paths, scope and state, then binds, then announces. Every
// step that can fail happens before the URL is printed, so the message appears
// only once the dashboard is listening.
func runServe(cmd *cobra.Command, port int) error {
	paths, err := config.ResolvePaths()
	if err != nil {
		return err
	}
	// Before the URL: what the dashboard will and will not contain is part of
	// reading it, and a notice printed after the address reads as an afterthought.
	scope, _, err := resolveDiscoveryScope(cmd, paths)
	if err != nil {
		return err
	}
	events := store.New(filepath.Join(paths.DataDir, "events.ndjson"))
	primitives := inventory.New(paths.PrimitivesFile)
	// Assigned, not redeclared: err is read again below, and a shadowed copy here
	// would make the later reads ambiguous to a reader and to govet.
	if err = style.WithSpinner(cmd.OutOrStdout(), ttyOutput(cmd), "Refreshing primitive inventory", func() error {
		discovery, discoverErr := discoverAllRepos(paths, scope.ClaudeDir)
		if discoverErr != nil {
			return discoverErr
		}
		return primitives.Refresh(events, discovery)
	}); err != nil {
		return err
	}
	listener, err := ui.Listen(port)
	if err != nil {
		return err
	}
	// From the bound listener's own address, so the message can only name a port
	// something is actually listening on.
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Serving dashboard at http://"+listener.Addr().String())
	// Resolved once, here: internal/cli is the only layer holding
	// config.Paths, and the dashboard never reads the file itself.
	return ui.Serve(listener, events, primitives, repolabel.Labels(config.ProjectLabels(paths)))
}
