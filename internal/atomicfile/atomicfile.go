// Package atomicfile publishes a whole file so that a concurrent reader observes
// only the old contents or the new ones, and so that a publication reported as
// successful is one the data survives a power loss.
//
// Both halves matter and they are different properties. Writing in place is
// visible half-written: a reader that catches a truncated state file does not see
// fewer records, it sees a file that fails to parse. Writing to a temporary file
// in the same directory and renaming it makes the switch a single operation, so
// there is no moment at which the file is neither version.
//
// Durability is the half that is easy to leave out. A rename that has returned
// has changed a directory entry the filesystem may not have committed yet, and
// the temporary file's bytes may not be committed either — so a publication is
// only complete once the file has been synced before the rename and the directory
// after it. This package exists because the four call sites that republish local
// state each hand-rolled the temporary-file dance and none of them did that.
//
// It deliberately does no locking. Atomic publication and isolation between
// writers are separate concerns: a writer that decided what to write from an
// earlier read still erases a second writer's record, however atomic the rename.
// internal/lockfile owns that half, and a caller that republishes state needs
// both.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// dirMode is the mode a missing parent directory is created with. Local state is
// private to its owner, so a directory this package creates is too.
const dirMode fs.FileMode = 0o700

// tempPattern names the in-progress publications. A single shared prefix is what
// lets a caller — and a test — recognise a leftover as this package's rather than
// as somebody's data file.
const tempPattern = ".publish-*"

// Publish writes data as path, replacing whatever is there, so that a concurrent
// reader sees either the old file or the complete new one and a successful return
// means the new contents are durable.
//
// The temporary file is removed on every failure path. A leftover is a complete
// copy of whatever was being published, sitting next to the real file under a
// name nothing else would ever clean up — and for this tool that content is local
// state about real repositories.
func Publish(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	// The same directory as the destination, because a rename is only atomic
	// within one filesystem: a temporary file in the system temp directory could
	// be on another mount, where the rename degrades to a copy.
	f, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmp := f.Name()

	if _, err := f.Write(data); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", tmp, err), f.Close(), os.Remove(tmp))
	}
	// CreateTemp opens at 0600 regardless of the umask, so this only matters when
	// the caller asked for something else — which is what lets a publication
	// preserve the mode of a file another program owns. Chmod on the handle, not
	// the path, so it cannot land on a different file.
	if err := f.Chmod(mode); err != nil {
		return errors.Join(fmt.Errorf("setting the mode of %s: %w", tmp, err), f.Close(), os.Remove(tmp))
	}
	// Before the rename, not after: a rename that has been committed while the
	// bytes have not leaves the destination name pointing at a file whose contents
	// a power loss can still truncate to zero.
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("syncing %s: %w", tmp, err), f.Close(), os.Remove(tmp))
	}
	if err := f.Close(); err != nil {
		return errors.Join(fmt.Errorf("closing %s: %w", tmp, err), os.Remove(tmp))
	}

	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(fmt.Errorf("replacing %s: %w", path, err), os.Remove(tmp))
	}
	// After the rename the temporary name is gone, so there is nothing left to
	// remove on this last failure: the file is published and only its durability
	// is in question.
	if err := SyncDir(dir); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	return nil
}

// ModeOf returns path's current permission bits, or fallback when path does not
// exist or cannot be inspected. It is how a publication preserves the mode of a
// file another program owns instead of imposing one.
//
// A file this tool did not create is not this tool's to re-permission: forcing a
// mode onto it would be a side effect of writing one setting. Lstat rather than
// Stat, so following a link cannot pick up the mode of something else entirely —
// and anything Lstat reports as not a regular file falls back, because Publish
// replaces a path with a regular file and the bits of a link or a directory are not
// a mode a regular file can be published at. A symlink carries 0755 on darwin and
// 0777 on linux, so reporting its bits would hand a caller a settings file the whole
// machine can read, or on linux write.
func ModeOf(path string, fallback fs.FileMode) fs.FileMode {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fallback
	}
	return info.Mode().Perm()
}

// SyncDir asks the filesystem to make dir's entries durable, so a rename that has
// returned survives a power loss.
//
// A platform or filesystem with no such request is not a publication failure. Some
// filesystems refuse a directory fsync outright, and refusing to publish there
// would break correct installations over a guarantee that platform does not offer
// in the first place: see unsupportedDirSync.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if unsupportedDirSync(err) {
		err = nil
	}
	return errors.Join(err, f.Close())
}
