// Package lockfile serializes a read-modify-write over one local state file
// behind an exclusive advisory lock on a sidecar file.
//
// Atomic publication and isolation between writers are different properties, and
// state this tool republishes whole needs both: a writer that decided what to
// write from a snapshot taken earlier erases anything a second writer recorded in
// between, however atomic its final rename was.
//
// The lock is a separate, always-empty file because opening the state file itself
// with O_CREATE would leave a zero-length file behind on a crash, and a
// zero-length state file does not parse. The lock file holds nothing and is never
// removed — unlinking it while another process holds it open would hand the next
// two writers two different locks.
package lockfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	fileMode fs.FileMode = 0o600
	dirMode  fs.FileMode = 0o700
)

// WithLock runs fn while holding an exclusive advisory lock on path, waiting for
// any other holder — in this process or another — to release it. Closing the file
// releases the lock, and the kernel closes the descriptor if the process dies, so
// a crash cannot leave the lock held by nobody.
func WithLock(path string, fn func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	if lockErr := lockExclusive(file); lockErr != nil {
		// Refusing is the right failure: a filesystem that cannot lock (some
		// network mounts) cannot make the read-modify-write safe either, and a
		// write that looked successful and then vanished is the outcome this
		// package exists to prevent.
		return errors.Join(fmt.Errorf("locking %s: %w", path, lockErr), file.Close())
	}
	return errors.Join(fn(), file.Close())
}
