//go:build !remote

// The assertion that is about what the *default* build does not have, kept with
// the build that has that property — the arrangement
// internal/config/registry_default_test.go already uses for the same reason.
package cli

import (
	"strings"
	"testing"
)

// TestDefaultBuildHasNoRemoteCommand is ADR-0012 made mechanical.
//
// The claim the default binary makes is "it contains no network code". A `remote`
// line in `wake --help` — even one that only reported being unsupported — turns
// that into something a reader has to verify rather than something they can see,
// and the whole point of compiling delivery out rather than configuring it off is
// that the absence is visible.
func TestDefaultBuildHasNoRemoteCommand(t *testing.T) {
	out, err := run(t, "--help")
	if err != nil {
		t.Fatalf("wake --help error = %v", err)
	}
	if strings.Contains(out, "remote") {
		t.Errorf("the default build's help names remote delivery:\n%s", out)
	}
	if _, err := run(t, "remote"); err == nil {
		t.Error("wake remote succeeded in the default build, want an unknown-command error")
	}
}
