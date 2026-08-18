package config

import (
	"os"
	"os/exec"
	"strings"
)

// DiscoverRootForRegistration returns the repository root to record consent for,
// discovered from the directory the command was invoked in.
//
// It is reachable only from `wake init`'s registration step and must never be called
// from the derivation path — not from Identify, not from ConsentedRoot, and not from
// ingest. ADR-0019 §1 makes resolution a pure string operation over the recorded
// snapshot: no git, no os.Stat, nothing that reads the disk, because a derivation
// that shelled out would attribute the same event differently depending on what the
// working tree looked like at the time. §9 states the other half — `init` is the only
// operation that discovers and records a root — and this function is that discovery.
// TestDiscoverRootForRegistrationIsNamedOnlyOnInitsPath is the mechanical guard, so a
// later caller added on the derivation path fails a test rather than a review.
//
// A directory that is not a git repository is accepted as its own root (ADR-0019 §5),
// which is why any git failure is a fallback to the working directory rather than an
// error: refusing to activate outside a checkout would refuse the case of a person
// running an agent in a plain directory, and that person's usage is exactly what this
// tool is for.
//
// It normalizes nothing and hashes nothing. Register owns symlink resolution, the
// case-fold probe and the nested-root refusal (ADR-0019 §5), and a second
// normalization here would be a second identity rule — two places deciding what
// counts as the same repository is how an id stops being derivable from its root.
//
// It creates nothing on disk, matching ClaudeCodeDir: discovering where a repository
// starts is separate from writing anything into it. The only error it returns is
// os.Getwd's, which names no path of wake's making (plan §4.2); git's own stderr is
// captured and discarded rather than surfaced, so a failure in a directory it cannot
// read cannot print that directory either.
func DiscoverRootForRegistration() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if output, gitErr := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); gitErr == nil {
		return strings.TrimSpace(string(output)), nil
	}
	return cwd, nil
}
