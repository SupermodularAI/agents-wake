//go:build unix

package atomicfile

import (
	"errors"
	"syscall"
)

// unsupportedDirSync reports whether err means "this filesystem does not offer a
// durable directory sync" rather than "the sync failed".
//
// The distinction matters because refusing to publish is the alternative. tmpfs
// and several network filesystems answer fsync on a directory with EINVAL or
// ENOTSUP, and a WAKE_DIR on one of those is a legitimate installation — the
// caller would be told its state file could not be written when in fact it was.
// The weaker guarantee is the one that platform offers; publication is still
// atomic there, because the rename is.
func unsupportedDirSync(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EBADF) ||
		errors.Is(err, syscall.EISDIR)
}
