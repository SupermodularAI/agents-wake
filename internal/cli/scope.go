package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
)

// resolveDiscoveryScope resolves the consent boundary for primitive discovery and
// reports it when project-local discovery is withheld.
//
// report and serve share it because acceptance covers them identically. It only
// composes and prints: the resolution is activation.DiscoveryScope's and the
// discovery split is inventory's (ADR-0001).
//
// The notice is a state word and a next step. It names no path and no repository
// label (plan §3.4, ADR-0019 §7), and it goes to stderr so a non-TTY stdout stays
// the deterministic text plan §7.3 promises.
func resolveDiscoveryScope(cmd *cobra.Command, paths config.Paths) (inventory.Scope, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return inventory.Scope{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return inventory.Scope{}, err
	}
	scope := activation.DiscoveryScope(paths, filepath.Join(home, ".claude"), cwd)
	switch scope.Project {
	case inventory.ProjectUnconsented:
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Project-local primitives were not collected: this directory is not a consented repository. Run 'wake init' here to include it.")
	case inventory.ProjectUnresolved:
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Project-local primitives were not collected: the consented-repository table could not be read.")
	case inventory.ProjectConsented:
	}
	return scope, nil
}
