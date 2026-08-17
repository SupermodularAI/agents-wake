package config

import (
	"errors"
	"io/fs"
	"os"
)

// The three ways a sensitive file can be one this build refuses to use. They are
// sentinels because a caller names the file's role and this file names the fault,
// and neither of them may name the path: ADR-0007 fails closed without disclosing
// what it refused to read.
//
// Each is phrased as the tail of a sentence, the construction errPathNotAbsolute
// already uses, so "the repository-id salt" + "is not a regular file" reads as one
// message without either half quoting the file.
var (
	errNotARegularFile = errors.New("is not a regular file")
	errIsASymlink      = errors.New("is a symbolic link; it must be the file itself")
	errTooPermissive   = errors.New("is readable or writable by someone other than its owner")
)

// The three ways a state directory can be one this build refuses to write into.
var (
	errNotADirectory    = errors.New("is not a directory")
	errDirNotOwned      = errors.New("is not owned by the user running this command")
	errDirGroupWritable = errors.New("is writable by group or other")
)

// checkSensitiveFile reports whether path is a private regular file this build
// will read. A path that does not exist is reported as fs.ErrNotExist so a caller
// can tell "not yet" from "wrong" — loadOrCreateSalt's first run depends on it.
//
// Lstat, not Stat: following the link is exactly what must not happen. A salt or a
// project table replaced by a symlink is a redirection, and resolving it would
// read — or on the next publish, write — wherever it points, while every check
// below would still pass on the target.
//
// The mode test is a maximum rather than an equality. 0600 is what this build
// writes, and a file at 0400 is stricter than required rather than wrong; a file
// with any bit outside 0600 is one somebody other than its owner can reach, and
// for the salt that means the ids it keys are no longer one-way for them.
func checkSensitiveFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		// Unwrapped: fs.ErrNotExist is the control-flow signal the caller
		// branches on, and os.Lstat's error already names the file for the
		// remaining cases, which never reach a user.
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return errIsASymlink
	case !info.Mode().IsRegular():
		return errNotARegularFile
	case info.Mode().Perm()&^fs.FileMode(0o600) != 0:
		return errTooPermissive
	}
	return nil
}

// checkStateDir reports whether dir is a directory only its owner can write into.
// A directory that does not exist is not a fault: it has not been created yet and
// MkdirAll will create it at 0700.
//
// Only this directory, never its ancestors. /tmp is world-writable with the sticky
// bit and is a legitimate WAKE_DIR parent under a test, so walking up would refuse
// correct installations over a property this check cannot fix anyway. The
// directory holding the file is the one where the file can be replaced.
//
// Stat, not Lstat, unlike checkSensitiveFile: plan § 2.3 asks only for ownership
// and writability here, and users do legitimately symlink ~/.config, so the
// question is about the directory the path leads to.
func checkStateDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errNotADirectory
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errDirGroupWritable
	}
	// "Cannot answer" is not "no": a platform with no ownership to inspect must
	// not refuse an installation over a check it never made.
	if owned, known := ownedByCaller(info); known && !owned {
		return errDirNotOwned
	}
	return nil
}
