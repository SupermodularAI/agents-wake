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

// lockExclusiveNow takes an exclusive advisory lock on f without waiting, and
// reports whether it got one.
//
// "Somebody else holds it" is an ordinary outcome here rather than an error: the
// caller that finds the lock held has nothing to add by repeating what the holder
// is already doing, which is what distinguishes a single-flight attempt from the
// queue lockExclusive forms.
func lockExclusiveNow(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		// As in lockExclusive: a preemption signal is not a failure to lock.
		case errors.Is(err, syscall.EINTR):
			continue
		// EWOULDBLOCK and EAGAIN are the same value on both released targets, but
		// both are spelled so the reader does not have to know that.
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			return false, nil
		case err != nil:
			return false, err
		}
		return true, nil
	}
}
