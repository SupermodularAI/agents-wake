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
//
// The mechanism itself lives in internal/lockfile: the spool and the derived
// inventory need the same serialisation over their own state files, and three
// copies of the same flock would be three places for it to drift.
func withProjectsLock(projectsFile string, fn func() error) error {
	return lockfile.WithLock(filepath.Join(filepath.Dir(projectsFile), projectsLockName), fn)
}
