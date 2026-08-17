//go:build !unix

package detach

import (
	"errors"
	"runtime"
	"syscall"
)

// supported reports that this platform has no detached child this package knows how
// to start. The released targets are darwin and linux (the CI build matrix, and
// ADR-0001's curl installer), so this file exists to keep the failure loud on
// anything else.
//
// Loud rather than degraded, unlike atomicfile's directory sync: an attached child
// is not a weaker version of a detached one. It would be killed when the terminal
// closes, so the session whose end triggered the scan is the one whose events go
// missing — silently, and only on that platform.
func supported() error {
	return errors.New("a detached child process is not supported on " + runtime.GOOS)
}

// detachAttr is never reached, since supported refuses first. It exists so this
// file compiles as the counterpart of the unix one.
func detachAttr() *syscall.SysProcAttr { return nil }
