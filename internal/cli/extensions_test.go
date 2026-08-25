// The seams' own tests: that each is registered exactly once, and that what they
// add to `doctor` is what a fresh install actually sees.
package cli

import (
	"testing"
)

// An exact count, not "at least". A duplicate registration is the failure this test
// exists for: it would print a doctor section twice and spawn two detached flushes per
// scan, and neither is loud enough to notice without an assertion.
//
// Two sections since DG-75 — the collection boundary's and delivery's — and the number
// is spelled out rather than derived, so a feature adding a third has to say so here.
func TestEachSeamIsRegisteredExactlyOnce(t *testing.T) {
	if len(diagnosisSections) != 2 {
		t.Errorf("registered %d diagnosis sections, want exactly 2", len(diagnosisSections))
	}
	if len(afterScan) != 1 {
		t.Errorf("registered %d post-scan hooks, want exactly 1", len(afterScan))
	}
}

// ADR-0030 replaces ADR-0012's artefact claim with a behavioural one, and this
// is that claim at the unit level: a fresh install reports delivery disabled on
// every line doctor prints about it. A Contains assertion cannot witness the
// whole of it — an extra line would pass every one of them — so the whole of
// stdout is compared.
//
// The whole of stdout means the boundary section too, which is why this case is the one
// that has to change when a section is added. Section order is registration order,
// which follows the order the toolchain hands this package's files to the compiler —
// today, sorted, so doctor_boundary.go precedes doctor_remote.go. That is a toolchain
// detail rather than a language guarantee, which is exactly why the order is pinned by
// this snapshot: a build that ordered them differently fails here rather than shipping
// a doctor whose sections moved.
//
// isolateRemote rather than isolate, because isolate does not clear
// WAKE_REMOTE_AUTHORIZATION: a developer who exports it would otherwise see
// `remote credential: set` and a failure that says nothing about the code.
func TestDoctorOutputOnAFreshInstall(t *testing.T) {
	isolateRemote(t)

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
		"records from an earlier schema version: 0\n" +
		"refused project entries: 0\n" +
		"global boundary directories gone: 0\n" +
		"global boundary registrations refused: 0\n" +
		"refused calls: 0\n" +
		"pending calls: 0\n" +
		"interrupted calls: 0\n" +
		"ambiguous skill runs: 0\n" +
		"store rebuild: not needed\n" +
		"integration: never scanned\n" +
		"global boundary: not set\n" +
		"global boundary repositories: 0\n" +
		"remote endpoint: not configured\n" +
		"remote credential: not configured\n" +
		"remote delivery: off\n" +
		"remote last flush: never\n" +
		"remote delivered through: 0\n" +
		"remote pending: 0\n"

	if stdout != want {
		t.Errorf("doctor output changed:\ngot:\n%s\nwant:\n%s", stdout, want)
	}
}
