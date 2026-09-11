package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/record"
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
func resolveDiscoveryScope(cmd *cobra.Command, paths config.Paths) (inventory.Scope, record.Namer, error) {
	claudeDir, err := config.ClaudeCodeDir()
	if err != nil {
		return inventory.Scope{}, record.Namer{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return inventory.Scope{}, record.Namer{}, err
	}
	scope, names := activation.DiscoveryScope(paths, claudeDir, cwd)
	switch scope.Project {
	case inventory.ProjectUnconsented:
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Project-local primitives were not collected: this directory is not a consented repository. Run 'wake init' here to include it.")
	case inventory.ProjectUnresolved:
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Project-local primitives were not collected: the consented-repository table could not be read.")
	case inventory.ProjectConsented:
	}
	return scope, names, nil
}

// discoverAllRepos merges every consented repository's project-local
// primitives with the harness's global ones, so report and serve reflect every
// repo `wake init` has registered on this machine — not only the one the
// command happens to run in (plan §8, "served dashboard, navigation and
// filters"). resolveDiscoveryScope alone cannot do this: its Scope names at
// most one root, cwd's, which is right for the notice it prints but wrong for
// what the dashboard and report are meant to aggregate.
//
// Every harness, not only Claude Code. This discovery is what report and serve
// republish the snapshot from, so a Claude-Code-only one here silently drops the
// other harness's declared primitives: its MCP servers would render as unmatched
// on every row — invocations of a server nothing in the inventory accounts for —
// and a server it declares but never invokes would vanish from the unused list
// entirely, which is the one list that exists to name unused things.
func discoverAllRepos(paths config.Paths, claudeDir string) (inventory.Discovery, error) {
	roots, names, err := activation.AllRepoRoots(paths)
	if err != nil {
		return inventory.Discovery{}, err
	}
	// A config file this build cannot resolve leaves the path empty, which reads as
	// "this machine declares none" — not observed, never zero.
	openCodeConfig, configErr := config.OpenCodeConfigFile()
	if configErr != nil {
		openCodeConfig = ""
	}
	return inventory.Merge(
		inventory.ClaudeCodeAcrossRepos(claudeDir, roots, names),
		inventory.OpenCodeServers(openCodeConfig, names),
	), nil
}
