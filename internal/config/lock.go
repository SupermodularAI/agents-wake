package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// withProjectsLock runs fn holding an exclusive lock on the resolution table,
// waiting for any other holder — in this process or another — to finish.
//
// Atomic publication and isolation between writers are different properties, and
// the table needs both. writeProjects gives the first: a reader sees the old table
// or the new one, never half of one. It cannot give the second, because a writer
// that decided what to write from a snapshot taken earlier republishes the whole
// table and erases anything recorded in between. ADR-0019 §9 requires the table to
// be append-only and makes a second writer part of the design — under --all the
// ingest sweep registers roots while init may be registering one — so the
// read-modify-write has to be serialised, not merely atomic at the end.
//
// The lock is a separate file rather than projects.json itself: opening the table
// with O_CREATE would leave a zero-length file behind on a crash, and a
// zero-length table does not parse, which readProjects treats as a hard error by
// design. The lock file is created empty, holds nothing, and is never removed —
// unlinking it while another process holds it open would hand the next two writers
// two different locks.
func withProjectsLock(projectsFile string, fn func() error) error {
	dir := filepath.Dir(projectsFile)
	if err := os.MkdirAll(dir, dataDirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, projectsLockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, projectsFileMode)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	if err := lockExclusive(f); err != nil {
		// Refusing to register is the right failure: a filesystem that cannot lock
		// (some network mounts) cannot make the merge below safe either, and a
		// registration that looked successful and then vanished is the outcome
		// ADR-0019 exists to prevent.
		return errors.Join(fmt.Errorf("locking %s: %w", path, err), f.Close())
	}
	// Closing releases the lock, and the kernel closes the descriptor if this
	// process dies — so a crash mid-registration cannot leave the table locked, the
	// way a lock somebody has to remove afterwards would.
	return errors.Join(fn(), f.Close())
}
