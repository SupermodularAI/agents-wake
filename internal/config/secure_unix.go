//go:build unix

package config

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByCaller reports whether info's owner is the user running this process, and
// whether this platform could answer.
//
// Two return values rather than one because plan § 2.3 asks for the check "where
// the platform allows" it: a build that cannot inspect ownership must not report
// "not owned", which would refuse every installation on that platform. "Cannot
// answer" and "no" are different outcomes and the caller treats them differently.
func ownedByCaller(info fs.FileInfo) (owned, known bool) {
	// The two-value assertion is required, not stylistic: .golangci.yml sets
	// errcheck's check-type-assertions, and a Sys() that is not a *Stat_t is
	// precisely the "cannot answer" case rather than a panic.
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	return int(st.Uid) == os.Getuid(), true
}
