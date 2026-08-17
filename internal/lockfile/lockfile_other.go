//go:build !unix

package lockfile

import (
	"errors"
	"os"
	"runtime"
)

// lockExclusive reports that this platform has no lock this package knows how to
// take. The released targets are darwin and linux (the CI build matrix, and
// ADR-0001's curl installer), so this file exists to keep the failure loud on
// anything else: a read-modify-write that cannot be serialised against a second
// writer must be refused, not performed and silently lost.
func lockExclusive(*os.File) error {
	return errors.New("advisory file locking is not supported on " + runtime.GOOS)
}

// lockExclusiveNow reports the same absence for the non-waiting acquire, and for
// the same reason: a caller must never read "this platform cannot lock" as "the
// lock is free".
func lockExclusiveNow(*os.File) (bool, error) {
	return false, errors.New("advisory file locking is not supported on " + runtime.GOOS)
}
