package config

import (
	"path/filepath"

	"github.com/SupermodularAI/agents-wake/internal/lockfile"
)

// withProjectsLock runs fn holding an exclusive lock on the resolution table,
// waiting for any other holder — in this process or another — to finish.
//
// Atomic publication and isolation between writers are different properties, and
// the table needs both. writeProjects gives the first: a reader sees the old table
// or the new one, never half of one. It cannot give the second, because a writer
// that decided what to write from a snapshot taken earlier republishes the whole
// table and erases anything recorded in between. ADR-0019 §9 requires the table to
// be append-only and makes a second writer part of the design — a hook-triggered
// scan runs while the user runs `wake init`, and two `wake init` runs in two
// repositories can overlap — so the read-modify-write has to be serialised, not
// merely atomic at the end.
//
// The lock is a separate file rather than projects.json itself: opening the table
// with O_CREATE would leave a zero-length file behind on a crash, and a
// zero-length table does not parse, which readProjects treats as a hard error by
// design. The lock file is created empty, holds nothing, and is never removed —
// unlinking it while another process holds it open would hand the next two writers
// two different locks.
//
// The mechanism itself lives in internal/lockfile: the spool and the derived
// inventory need the same serialisation over their own state files, and three
// copies of the same flock would be three places for it to drift.
func withProjectsLock(projectsFile string, fn func() error) error {
	return lockfile.WithLock(filepath.Join(filepath.Dir(projectsFile), projectsLockName), fn)
}

// withConfigLock runs fn holding an exclusive lock on config.toml, waiting for any
// other holder to finish.
//
// Set is a read-decode-modify-encode-write over the whole file, which is what
// keeps a key a newer build knows alive across a write — and also what makes two
// concurrent writes of two different keys lose one, silently, with both calls
// returning success. Serialising them is the only fix: atomicity at the end
// cannot help a writer that already decided what to write.
//
// Unexported because only this package writes config.toml.
func withConfigLock(configFile string, fn func() error) error {
	return lockfile.WithLock(filepath.Join(filepath.Dir(configFile), configLockName), fn)
}

// WithClaudeSettingsLock runs fn holding an exclusive lock on Claude Code's
// settings file, waiting for any other holder to finish.
//
// Exported, unlike the other two, because internal/activation is the package that
// writes settings.json and needs the same read-merge-republish serialisation over
// it: `wake init` and `wake remove` running together must not each publish a file
// decided before the other's edit.
//
// The lock file lives under Wake's config root rather than beside settings.json in
// ~/.claude/. Wake can only serialise Wake's own writers either way — another
// program editing that file is outside any lock this tool takes — and dropping a
// lock file into the harness's own directory would add a file `init` would then
// have to disclose under ADR-0010, for no gain.
func WithClaudeSettingsLock(configDir string, fn func() error) error {
	return lockfile.WithLock(filepath.Join(configDir, claudeSettingsLockName), fn)
}

// The three locks above are three distinct files, and none may be taken inside
// another hold of itself: flock is per open file description, so a second acquire
// of the same lock from inside the first deadlocks against its own holder. That is
// why the read-modify-write bodies in this package come in an unlocked *Locked
// form that the locked entry point wraps exactly once, rather than in helpers that
// each take the lock they need.
