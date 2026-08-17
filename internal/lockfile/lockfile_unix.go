//go:build unix

package lockfile

import (
	"errors"
	"os"
	"syscall"
)

// lockExclusive takes an exclusive advisory lock on f, blocking until any other
// holder releases it.
//
// flock rather than a lock file somebody has to delete: the kernel releases it
// when the descriptor closes, including when the process holding it is killed, so
// a crash cannot leave the state permanently blocked. It is per open file
// description, which is what makes it hold between processes and not only between
// goroutines.
func lockExclusive(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		// A signal is not a failure to lock: the Go runtime sends SIGURG to preempt
		// goroutines, so an interrupted wait is ordinary and the wait resumes.
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return err
	}
}
