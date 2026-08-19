package activation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// errExecutableNotAbsolute is returned for a self path this build will not unlink.
// Absolute only, for the same reason hookCommandFor demands it: a relative path
// resolves against a working directory `uninstall` does not control, and the file it
// would then delete is not necessarily this binary. The message names the
// requirement, never the path.
var errExecutableNotAbsolute = errors.New("this build's own path is not absolute, so wake uninstall cannot identify the binary to remove; run the binary by its absolute path")

// UninstallPlan is every path `wake uninstall` deletes, resolved once so that the
// disclosure printed before the removal and the removal itself cannot disagree
// (ADR-0010: the command shows the exact paths it will modify).
//
// The four exported fields are exactly what the disclosure prints. They are this
// tool's own locations plus the harness's settings file — never a consented
// repository root or label, which are only ever read inside internal/config
// (ADR-0019 §7: count only, never paths).
type UninstallPlan struct {
	// SettingsFile is the harness's own file. Only Wake's marked hook entry is
	// removed from it; the file itself is never deleted.
	SettingsFile string
	// DataDir is the data root: the event spool, the derived inventory, the health
	// counters and the local project table.
	DataDir string
	// ConfigDir is the config root: config.toml, the local identity salt and the
	// lock files.
	ConfigDir string
	// Executable is the file this process was started from, and the last thing
	// removed.
	Executable string

	paths     config.Paths
	claudeDir string
}

// PlanUninstall resolves the full removal from the arguments alone, raising every
// refusal it can decide before anything has been disclosed or deleted — the same
// ordering Init uses for the refusals it can pre-check.
//
// The executable is symlink-resolved, matching hookCommandFor: an installation
// reached through a link has the real file recorded in its hook command, and
// unlinking the link would leave the binary the hook named still on disk. A path
// that cannot be resolved keeps its cleaned original rather than refusing —
// `uninstall` is the command a user reaches for to get Wake off the machine, and it
// must not be the one command an odd installation cannot run. Whatever is kept here
// is both what gets disclosed and what gets passed to the unlink, so the disclosure
// stays literally true either way.
//
// The settings file's shape is deliberately *not* pre-checked. `remove` fails on a
// settings document this build refuses to edit and `uninstall` fails the same way,
// at its first step, with nothing else yet removed and the binary still in place.
func PlanUninstall(paths config.Paths, claudeDir, executable string) (UninstallPlan, error) {
	if executable == "" || !filepath.IsAbs(executable) {
		return UninstallPlan{}, errExecutableNotAbsolute
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		resolved = filepath.Clean(executable)
	}
	return UninstallPlan{
		SettingsFile: filepath.Join(claudeDir, settingsFileName),
		DataDir:      paths.DataDir,
		ConfigDir:    paths.ConfigDir,
		Executable:   resolved,
		paths:        paths,
		claudeDir:    claudeDir,
	}, nil
}

// Remove performs the removal and reports whether a Wake-owned hook entry was one of
// the things removed — the same answer Uninstall gives, and what lets the caller print
// "was not installed" for a machine that was never init'ed rather than treating it as
// a fault.
//
// The order is the decision this method exists to hold, and it is three steps:
//
//  1. Uninstall with purge, which removes only Wake-marked hook entries — a group a
//     user edited is kept and counted, never guessed at (ADR-0010's ownership marker)
//     — and then the data root.
//  2. The config root, through internal/config, which owns the salt (ADR-0019). It
//     must come after step 1 and not before: the settings lock, config.lock and
//     projects.lock all live under the config root, so removing it first would pull
//     the lock file out from under a hold that is still open.
//  3. The binary, last, and only if the first two returned no error. A failure part
//     way through leaves an invokable `wake uninstall` for the user to retry; a
//     binary removed first would leave a half-removed tool that cannot finish the job.
//     Unlinking the running binary needs no special handling on either supported
//     platform (ADR-0021: darwin and linux) — the inode stays valid until this
//     process exits, which is why the success line can still be printed afterwards.
//
// An executable that is already gone is success, so a re-run after a partial failure
// converges rather than reporting a fault for work that is already done.
func (p UninstallPlan) Remove() (bool, error) {
	removed, err := Uninstall(p.paths, p.claudeDir, true)
	if err != nil {
		return removed, err
	}
	if configErr := config.RemoveConfigRoot(p.paths); configErr != nil {
		return removed, configErr
	}
	if removeErr := os.Remove(p.Executable); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		return removed, fmt.Errorf("removing %s: %w", p.Executable, removeErr)
	}
	return removed, nil
}
