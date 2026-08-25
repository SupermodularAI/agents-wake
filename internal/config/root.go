package config

import (
	"os"
	"os/exec"
	"strings"
)

// DiscoverRootForRegistration returns the repository root to record consent for,
// discovered from dir — or from the directory the command was invoked in when dir is
// empty, which is what `wake init` asks for.
//
// ceiling bounds the upward walk: a non-empty one is handed to git as
// GIT_CEILING_DIRECTORIES, so a toplevel at or above it is unreachable and the
// directory becomes its own root instead. An empty ceiling is unbounded. That is what
// lets a directory discovered under a recorded collection boundary be registered under
// the repository it belongs to without the walk escaping the boundary the user
// consented (ADR-0032 §1).
//
// It is reachable only from `wake init`'s registration step and must never be called
// from the derivation path — not from Identify, not from ConsentedRoot. ADR-0019 §1
// makes resolution a pure string operation over the recorded snapshot: no git, no
// os.Stat, nothing that reads the disk, because a derivation that shelled out would
// attribute the same event differently depending on what the working tree looked like
// at the time. §9 states the other half — `init` is the only operation that discovers
// and records a root — and this function is that discovery. ADR-0032 §2 narrows that
// to admit one more caller and no others: a working directory matched against a
// user-recorded global root reaches here through RegisterUnderGlobalRoot, which is
// registration rather than derivation, and it is registration that the resolver
// observing the directory deliberately does not perform.
// TestDiscoverRootForRegistrationIsNamedOnlyOnInitsPath is the mechanical guard, so a
// later caller added on the derivation path fails a test rather than a review.
//
// A directory that is not a git repository is accepted as its own root (ADR-0019 §5),
// which is why any git failure is a fallback to the directory rather than an error:
// refusing to activate outside a checkout would refuse the case of a person running
// an agent in a plain directory, and that person's usage is exactly what this tool is
// for. A directory that is not there is the one case the fallback must not cover, so
// existence is checked before git runs: consenting a path nothing can be read from
// would look successful and then report a complete pass over nothing.
//
// It normalizes nothing and hashes nothing. Register owns symlink resolution, the
// case-fold probe and the nested-root refusal (ADR-0019 §5), and a second
// normalization here would be a second identity rule — two places deciding what
// counts as the same repository is how an id stops being derivable from its root.
//
// It creates nothing on disk, matching ClaudeCodeDir: discovering where a repository
// starts is separate from writing anything into it. The errors it returns are
// os.Getwd's and errRootNotADirectory, neither of which names a path of wake's making
// (plan §4.2); git's own stderr is captured and discarded rather than surfaced, so a
// failure in a directory it cannot read cannot print that directory either.
func DiscoverRootForRegistration(dir, ceiling string) (string, error) {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = cwd
	}
	cleaned, err := lexicalClean(dir)
	if err != nil {
		return "", err
	}
	// Before git, deliberately. git in a directory that does not exist fails, and the
	// fallback below would then return the vanished path as its own root — an invented
	// root, which is the one thing this function may not produce.
	info, statErr := os.Stat(cleaned)
	if statErr != nil || !info.IsDir() {
		return "", errRootNotADirectory
	}

	cmd := exec.Command("git", "-C", cleaned, "rev-parse", "--show-toplevel")
	if ceiling != "" {
		// os.Environ rather than a bare slice: a caller — a test, most often — that
		// neutralised GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM has to keep doing so, or what
		// git answers here would depend on the machine's own configuration.
		cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+ceiling)
	}
	if output, gitErr := cmd.Output(); gitErr == nil {
		return strings.TrimSpace(string(output)), nil
	}
	return cleaned, nil
}
