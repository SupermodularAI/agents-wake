//go:build !remote

// The default build's half of the seam, asserted with the build that has the
// property — the arrangement remote_absent_test.go and
// internal/config/registry_default_test.go already use.
package cli

import (
	"testing"
)

// Both seams are empty in the default binary. This is ADR-0012's claim at the
// level the seam introduces it: a slice a tagged file appends to is only
// harmless while nothing in the default build appends to it.
func TestDefaultBuildRegistersNoExtensions(t *testing.T) {
	if len(diagnosisSections) != 0 {
		t.Errorf("the default build registered %d diagnosis sections, want 0", len(diagnosisSections))
	}
	if len(afterScan) != 0 {
		t.Errorf("the default build registered %d post-scan hooks, want 0", len(afterScan))
	}
}

// Ticket acceptance: default-build doctor output is identical before and after
// this ticket. A Contains assertion cannot witness "byte for byte" — an extra
// line would pass every one of them — so the whole of stdout is compared.
func TestDefaultBuildDoctorOutputIsUnchanged(t *testing.T) {
	isolate(t)

	stdout, stderr, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if stderr != "" {
		t.Errorf("doctor wrote to stderr: %q", stderr)
	}

	const want = "hooks installed: 0\n" +
		"hooks removed: 0\n" +
		"owned hook groups kept: 0\n" +
		"last scan: never\n" +
		"transcripts: 0\n" +
		"unreadable sources: 0\n" +
		"parse errors: 0\n" +
		"skipped transcripts: 0\n" +
		"events written: 0\n" +
		"refused project entries: 0\n" +
		"refused calls: 0\n" +
		"pending calls: 0\n" +
		"interrupted calls: 0\n" +
		"ambiguous skill runs: 0\n" +
		"integration: never scanned\n"

	if stdout != want {
		t.Errorf("doctor output changed:\ngot:\n%s\nwant:\n%s", stdout, want)
	}
}
