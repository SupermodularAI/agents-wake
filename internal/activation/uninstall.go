package activation

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

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
	// Launcher is the symbolic link the command was invoked through, when the
	// invocation did not name the file itself. Empty for both documented
	// installations, which put a real file on PATH.
	Launcher string

	paths     config.Paths
	claudeDir string
}

// PlanUninstall resolves the full removal from the arguments alone, raising every
// refusal it can decide before anything has been disclosed or deleted — the same
// ordering Init uses for the refusals it can pre-check.
//
// The executable is symlink-resolved, matching hookCommandFor: an installation
// reached through a link has the real file recorded in its hook command, and
// unlinking the link alone would leave the binary the hook named still on disk. The
// link is kept as Launcher and removed as well, because the alternative is a `wake`
// on PATH resolving to nothing — an invocation that fails with "no such file" rather
// than "command not found", which is worse than either, and is not the "nothing
// Wake-related remains" this command promises. A path that cannot be resolved keeps
// its cleaned original rather than refusing — `uninstall` is the command a user
// reaches for to get Wake off the machine, and it must not be the one command an odd
// installation cannot run. Whatever is kept here is both what gets disclosed and what
// gets passed to the unlink, so the disclosure stays literally true either way.
//
// The settings file's shape is pre-checked, for the reason Init pre-checks the same
// shapes: a document this build refuses to edit fails the first removal step, and
// raised from there the refusal arrives after the disclosure has already told the user
// four paths are about to be deleted. Raised here it arrives before anything has been
// promised, and it is restated in this command's own words rather than the shared
// sentinel's.
func PlanUninstall(paths config.Paths, claudeDir, executable string) (UninstallPlan, error) {
	if executable == "" || !filepath.IsAbs(executable) {
		return UninstallPlan{}, errExecutableNotAbsolute
	}
	if settingsErr := checkSettingsRemovable(claudeDir); settingsErr != nil {
		return UninstallPlan{}, refusedBeforeRemoving(settingsErr)
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
		Launcher:     launcherFor(executable, resolved),
		paths:        paths,
		claudeDir:    claudeDir,
	}, nil
}

// launcherFor returns the link the command was invoked through, or "" when the
// invocation named a file rather than a link to one.
//
// Lstat decides it, not a comparison of the two paths: EvalSymlinks resolves every
// component, so a plain file under a symlinked parent — /tmp on darwin, which is a
// link to /private/tmp — also resolves to a different spelling, and disclosing that
// as a second path to delete would be one path named twice.
func launcherFor(executable, resolved string) string {
	info, err := os.Lstat(executable)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 || filepath.Clean(executable) == resolved {
		return ""
	}
	return filepath.Clean(executable)
}

// checkSettingsRemovable raises every refusal the settings document's shape decides
// for a removal, and keeps nothing it read.
//
// It is removeHooks' own read, performed early and thrown away, exactly as
// checkSettingsShape is installHooks'. Every event key is examined rather than only
// hookEvents, because that is what removeHooks sweeps: ownership is decided by the
// marker, so a group an earlier build registered somewhere this one does not is still
// Wake's to remove — and still a shape this build can refuse.
//
// Nothing is carried across to the removal. removeHooks decides what to write from
// the bytes it reads while holding the settings lock, so a document read out here
// would be a stale premise dressed as a decision. A file that changes shape in
// between is refused by that read exactly as it would have been without this one,
// with nothing removed when it happens.
func checkSettingsRemovable(claudeDir string) error {
	doc, err := readSettings(claudeDir)
	if err != nil {
		return err
	}
	for _, event := range slices.Sorted(maps.Keys(doc.hooks)) {
		if _, groupsErr := doc.groups(event); groupsErr != nil {
			return groupsErr
		}
	}
	return nil
}

// refusedBeforeRemoving restates a settings-shape refusal as `uninstall`'s own, and
// leaves every other error alone.
//
// Two things the shared sentinel cannot say. That nothing has been removed: the
// disclosure is worded as a deletion about to happen, so a bare refusal on the way
// past it leaves the reader unsure whether part of it happened. And which command to
// run once the file is repaired — `wake uninstall`, never `wake init`, which is the
// command that installs what the user just asked to have removed.
//
// It is a refusal rather than a step skipped because Wake's hook entry has to come
// out first: removing the binary while the harness still calls it at session start
// would leave a hook that fails every session, recorded in a file this build has just
// said it will not edit.
func refusedBeforeRemoving(err error) error {
	if !refusedTheSettingsShape(err) {
		return err
	}
	return fmt.Errorf("%w; nothing has been removed, because Wake's hook entry has to come out of the settings file first — repair the file and run wake uninstall again", err)
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
//  3. The binary — the link it was invoked through first, then the file — last, and
//     only if the first two returned no error. A failure part
//     way through leaves an invokable `wake uninstall` for the user to retry; a
//     binary removed first would leave a half-removed tool that cannot finish the job.
//     The link goes before the file for the same reason: a link removed after a failed
//     unlink of the file would leave a working binary that PATH no longer reaches,
//     whereas a file still in place is reachable by the path this command disclosed.
//     Unlinking the running binary needs no special handling on either supported
//     platform (ADR-0021: darwin and linux) — the inode stays valid until this
//     process exits, which is why the success line can still be printed afterwards.
//
// A step-1 refusal of the settings document is restated as this command's own, because
// the shared sentinel says neither that nothing has been removed nor which command to
// run next. PlanUninstall pre-checks those shapes, so reaching one here means the file
// changed underneath — and the answer for the user is the same either way.
//
// An executable that is already gone is success, so a re-run after a partial failure
// converges rather than reporting a fault for work that is already done.
func (p UninstallPlan) Remove() (bool, error) {
	removed, err := Uninstall(p.paths, p.claudeDir, true)
	if err != nil {
		return removed, refusedBeforeRemoving(err)
	}
	if configErr := config.RemoveConfigRoot(p.paths); configErr != nil {
		return removed, configErr
	}
	for _, path := range []string{p.Launcher, p.Executable} {
		if path == "" {
			continue
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return removed, fmt.Errorf("removing %s: %w", path, removeErr)
		}
	}
	return removed, nil
}
