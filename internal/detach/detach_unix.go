//go:build unix

package detach

import "syscall"

// supported reports that this platform can start a detached child.
func supported() error { return nil }

// detachAttr asks for a new session, which is what ADR-0016's "own process group"
// requires.
//
// Setsid rather than only Setpgid: it gives the child a new session and a new
// process group, and detaches it from the controlling terminal. A child in a new
// process group but the same session can still be reached by a signal sent to the
// session, and the terminal closing is exactly that.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
