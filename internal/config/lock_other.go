//go:build !unix

package config

import (
	"errors"
	"os"
	"runtime"
)

// lockExclusive reports that this platform has no lock this package knows how to
// take. The released targets are darwin and linux (the CI build matrix, and
// ADR-0001's curl installer), so this file exists to keep the failure loud on
// anything else: a registration that cannot be serialised against a second writer
// must be refused, not performed and silently lost.
func lockExclusive(*os.File) error {
	return errors.New("locking the repository table is not supported on " + runtime.GOOS)
}
