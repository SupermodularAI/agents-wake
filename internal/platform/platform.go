// Package platform declares the platform set wake supports and refuses the rest.
//
// ADR-0021 fixes that set at darwin and linux on amd64 and arm64 — exactly what
// CI's build matrix builds and .goreleaser.yaml publishes — and puts the refusal
// at process startup, before any file is touched. The comparison lives here
// rather than in cmd/wake or internal/cli so the entry path stays a call and a
// printed error (ADR-0001).
package platform

import (
	"fmt"
	"slices"
	"strings"
)

// OS returns the operating systems wake supports, in announcement order.
func OS() []string { return []string{"darwin", "linux"} }

// Arch returns the architectures wake supports, in announcement order.
//
// Nothing refuses on GOARCH — ADR-0021 makes the startup check one GOOS
// comparison, because a binary for the wrong architecture does not run at all.
// Arch exists so the announced set has a single source: matrix_test.go asserts
// CI's build matrix and .goreleaser.yaml against it.
func Arch() []string { return []string{"amd64", "arm64"} }

// Description names the whole supported set in one human-readable phrase.
func Description() string {
	return strings.Join(OS(), " and ") + " on " + strings.Join(Arch(), " and ")
}

// Check reports whether goos is a supported operating system, returning a
// refusal that names both the rejected platform and the supported set.
func Check(goos string) error {
	if slices.Contains(OS(), goos) {
		return nil
	}
	return fmt.Errorf("wake does not support %s: supported platforms are %s", goos, Description())
}
