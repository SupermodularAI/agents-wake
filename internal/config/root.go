package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitCallTimeout bounds every git call this package makes.
//
// `git rev-parse` reads a few files and prints two lines, so no honest answer is
// anywhere near this; what the deadline is for is the call that never returns. Since
// ADR-0044 §1 widened the candidate set, the probe is pointed at every unmatched
// working directory a scan sees — absolute paths read out of harness transcripts,
// which on the machine ADR-0044 measured included directories the user had long since
// forgotten. One of them living on a mount that no longer answers would otherwise hang
// the scan indefinitely, and "could not read" must mean "collects nothing", never an
// error that breaks a command (plan §4.3).
//
// Every call fails closed when it expires, because a deadline is indistinguishable
// from any other git failure here: the probe and the parent lookup answer nothing, and
// discovery falls back to the directory itself.
//
// A var rather than a const so a test can make the deadline unmeetable; nothing
// outside this package can reach it.
var gitCallTimeout = 10 * time.Second

// gitCommand builds a git invocation bounded by gitCallTimeout. The caller keeps the
// cancel func alive until the command has been run and its output read, which is what
// exec.CommandContext requires.
func gitCommand(args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCallTimeout)
	return exec.CommandContext(ctx, "git", args...), cancel
}

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

	cmd, cancel := gitCommand("-C", cleaned, "rev-parse", "--show-toplevel")
	defer cancel()
	if ceiling != "" {
		cmd.Env = boundedDiscoveryEnv(ceiling)
	}
	if output, gitErr := cmd.Output(); gitErr == nil {
		return strings.TrimSpace(string(output)), nil
	}
	return cleaned, nil
}

// discoverParentRepositoryForRegistration returns the spellings of the repository
// root that root is a linked git worktree of, or nil when it is not one.
//
// Registration only, and unexported on purpose. ADR-0019 §1 makes derivation a pure
// string operation over the recorded snapshot — no git, no os.Stat — and being
// unexported means no package outside this one can reach this at all;
// TestTheParentLookupIsNamedOnlyOnTheRegistrationPath keeps the in-package call
// sites to the registration step, the way
// TestDiscoverRootForRegistrationIsNamedOnlyOnInitsPath does for discovery.
//
// It registers nothing and consents nothing. ADR-0032 §2 narrowed §9 to exactly two
// root-discovery sites and this is neither: what comes back is a set of spellings
// the caller matches against entries *already* recorded. A parent matching none is a
// parent this machine has not consented, and the relation is then simply not
// recorded — hashing it would create stored data outside the consent boundary
// (ADR-0019 §9).
//
// It runs under scrubbedGitEnv for the reason that function gives: the hook-fired
// registration path inherits a session's environment, and a variable that re-points
// where git looks — GIT_COMMON_DIR above all, which --git-common-dir reports verbatim
// — would otherwise make a main checkout name a common directory nowhere near it.
// Containment is not enforced by the environment — it is enforced by the caller's
// requirement that the answer already be a recorded entry.
//
// Every failure answers nil: a directory that is not a repository, a git that is not
// installed, a bare main repository whose common directory names no working tree.
// Fail closed — no relation is always a safe answer, a wrong one re-points a
// repository's rows.
//
// It returns no error and therefore names no path in one, and git's own stderr is
// captured and discarded, as DiscoverRootForRegistration's is (plan §4.2).
func discoverParentRepositoryForRegistration(root string) []string {
	cmd, cancel := gitCommand("-C", root, "rev-parse", "--git-common-dir")
	defer cancel()
	cmd.Env = scrubbedGitEnv()
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parentSpellingsFromCommonDir(root, root, strings.TrimSpace(string(output)))
}

// parentSpellingsFromCommonDir turns git's --git-common-dir answer into the spellings
// of the repository a working tree is a linked worktree of, or nil when it is not one.
//
// from is the directory git was asked in, because git answers relative to it when it
// can. topLevel is the working tree the answer is about: a main checkout is its own
// common directory's parent, and comparing against `from` instead would read a main
// checkout's subdirectory as a worktree of the checkout it sits in.
//
// Shared by the two git questions this package asks about worktrees, so the two
// cannot drift about what counts as a linked worktree.
func parentSpellingsFromCommonDir(from, topLevel, common string) []string {
	if common == "" {
		return nil
	}
	// git answers relative to the directory -C moved it to when it can. Absolute
	// first, then clean, so the comparison below is against one spelling rule.
	if !filepath.IsAbs(common) {
		common = filepath.Join(from, common)
	}
	common = filepath.Clean(common)
	// A linked worktree's common directory is the main working tree's `.git`.
	// Anything else — a bare repository, a separated git directory — names no
	// working tree, and guessing one would invent a root.
	if filepath.Base(common) != ".git" {
		return nil
	}
	parent := filepath.Dir(common)
	// The main checkout is its own common directory's parent. Not a worktree.
	if parent == topLevel || !filepath.IsAbs(parent) {
		return nil
	}
	spellings := []string{parent}
	// The canonical spelling too: Register records the canonical root and may record
	// the offered one as an alias (ADR-0019 §5), so a parent consented through a
	// symlinked path has to be findable under either.
	if canonical, canonErr := canonicalRoot(parent); canonErr == nil && canonical != parent {
		spellings = append(spellings, canonical)
	}
	return spellings
}

// discoverLinkedWorktreeForRegistration answers, in one git call, whether dir sits
// inside a linked git worktree and — when it does — the worktree's own top level and
// the spellings of the repository it belongs to.
//
// Registration only, and unexported for the reason
// discoverParentRepositoryForRegistration is (ADR-0019 §1).
// TestTheWorktreeProbeIsNamedOnlyOnTheRegistrationPath is the mechanical guard.
//
// One call rather than two: ADR-0044 §2 allows a directory that is not a linked
// worktree at most one bounded probe for the question, and almost every directory a
// widened walk offers is not one. `git rev-parse` prints its answers in the order the
// options are given, so the first line is the top level and the second the common
// directory; any other shape answers nothing rather than being guessed at.
//
// It runs under scrubbedGitEnv, which is load-bearing here rather than defensive:
// this answer is what ADR-0044 §1 decides admission from, and its second bullet
// requires the repository a worktree resolves to be checked against the recorded table
// after git answers and never trusted from git's environment. An inherited
// GIT_CEILING_DIRECTORIES is the one git variable deliberately left alone: it can only
// make git find less, so the worst it costs is a refusal, and a refusal is the
// fail-closed answer.
//
// Every failure answers "", nil. It consents nothing: the caller checks the
// repository this names against the recorded table before anything is admitted
// (ADR-0044 §2). It returns no error and therefore names no path in one, and git's
// own stderr is captured and discarded (plan §4.2).
func discoverLinkedWorktreeForRegistration(dir string) (topLevel string, parent []string) {
	cmd, cancel := gitCommand("-C", dir, "rev-parse", "--show-toplevel", "--git-common-dir")
	defer cancel()
	cmd.Env = scrubbedGitEnv()
	output, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 {
		return "", nil
	}
	top := filepath.Clean(strings.TrimSpace(lines[0]))
	if !filepath.IsAbs(top) {
		return "", nil
	}
	spellings := parentSpellingsFromCommonDir(dir, top, strings.TrimSpace(lines[1]))
	if len(spellings) == 0 {
		return "", nil
	}
	return top, spellings
}

// boundedDiscoveryEnv is the environment a bounded discovery runs git in.
//
// A ceiling does not bound the environment: git documents that it "will not exclude
// ... a GIT_DIR set on the command line or in the environment", so with GIT_DIR or
// GIT_WORK_TREE set the toplevel git reports is the one they name and the ceiling is
// not consulted at all. scrubbedGitEnv is what removes them, along with every other
// GIT_ variable it does not name safe.
//
// Dropped here, where a ceiling was asked for, and not from DiscoverRootForRegistration's
// unbounded call. With no ceiling there is no boundary to escape and the directory
// the caller is standing in is the one being consented, so plain `wake init` keeps
// honouring the environment as it always has. Even so, this is a narrowing of the
// exposure and not the guarantee: what makes a discovered root safe is that
// RegisterUnderGlobalRoot checks it against the boundary afterwards, whatever git was
// persuaded to say.
func boundedDiscoveryEnv(ceiling string) []string {
	return append(scrubbedGitEnv(), "GIT_CEILING_DIRECTORIES="+ceiling)
}

// inheritableGitVars are the only GIT_-prefixed variables a git call this package
// makes may inherit. Everything else named GIT_ is dropped, whatever it is.
//
// GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM are kept because a caller — a test, most
// often — that neutralised them has to keep doing so, or what git answers would depend
// on the machine's own configuration. Neither can re-point a working tree: git honours
// core.worktree from a repository's own config and not from a global or system one
// (verified against git 2.50.1).
//
// GIT_CEILING_DIRECTORIES is kept because it can only make git find less, so the worst
// an inherited one costs is a refusal, and a refusal is the fail-closed answer.
// boundedDiscoveryEnv appends its own after this, which is what a bounded discovery
// runs under.
var inheritableGitVars = map[string]bool{
	"GIT_CONFIG_GLOBAL":       true,
	"GIT_CONFIG_SYSTEM":       true,
	"GIT_CEILING_DIRECTORIES": true,
}

// scrubbedGitEnv is os.Environ with every GIT_ variable removed except the three
// inheritableGitVars names.
//
// The rule is stated the safe way round on purpose. The calls that use this are the
// unattended ones — the scan a hook fires inherits the session's environment — and a
// variable that re-points where git looks makes git answer about the repository it
// names rather than the one the caller is asking about. Under ADR-0044 §1 that answer
// decides admission, so a variable this list forgets is a directory nobody consented
// gaining an entry. An enumeration of the dangerous variables is exactly how
// GIT_COMMON_DIR was missed: git documents it as making "non-worktree files that are
// normally in $GIT_DIR ... taken from this path instead", `rev-parse --git-common-dir`
// reports it verbatim, and it does so without disturbing --show-toplevel, so every
// check made against the discovered root still passes while the repository the answer
// names is entirely the environment's. Dropping the whole prefix instead means the
// next such variable — in a git that does not exist yet — is a refusal rather than a
// review miss.
//
// os.Environ rather than a bare slice: everything that is not git's own is the
// caller's, and unrelated to what git is being asked.
//
// Shared by both call sites so the two cannot drift about which variables a git call
// this package makes is allowed to inherit.
//
// The +1 of capacity is boundedDiscoveryEnv's append.
func scrubbedGitEnv() []string {
	environ := os.Environ()
	kept := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		name, _, found := strings.Cut(entry, "=")
		if found && strings.HasPrefix(name, "GIT_") && !inheritableGitVars[name] {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}
