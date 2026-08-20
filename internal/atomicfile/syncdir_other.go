//go:build !unix

package atomicfile

// unsupportedDirSync reports that this platform has no directory sync this
// package knows how to ask for. The released targets are darwin and linux (as
// internal/lockfile's own fallback records), so nothing here is reached on a
// supported build.
//
// Unlike locking, the absence is tolerated rather than refused: a lock that
// cannot be taken makes a read-modify-write unsafe, whereas a directory sync that
// cannot be requested only weakens durability. The rename still keeps the
// publication atomic, which is the property a reader depends on.
func unsupportedDirSync(error) bool { return true }
