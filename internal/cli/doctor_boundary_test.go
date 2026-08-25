package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
)

// widenTheRecordedBoundary rewrites the boundary's root to its parent and leaves the
// digest exactly as it was — the hand edit the digest exists to catch. Mode 0600,
// because the table's own reader refuses anything looser.
func widenTheRecordedBoundary(t *testing.T, paths config.Paths) {
	t.Helper()
	raw, err := os.ReadFile(paths.ProjectsFile)
	if err != nil {
		t.Fatalf("reading the project table: %v", err)
	}
	var table map[string]any
	if decodeErr := json.Unmarshal(raw, &table); decodeErr != nil {
		t.Fatalf("decoding the project table: %v", decodeErr)
	}
	boundary, ok := table["global_root"].(map[string]any)
	if !ok {
		t.Fatalf("the recorded table holds no boundary: %s", raw)
	}
	recorded, ok := boundary["root"].(string)
	if !ok {
		t.Fatalf("the recorded boundary holds no root: %s", raw)
	}
	boundary["root"] = filepath.Dir(recorded)
	edited, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		t.Fatalf("encoding the project table: %v", err)
	}
	if err := os.WriteFile(paths.ProjectsFile, append(edited, '\n'), 0o600); err != nil {
		t.Fatalf("writing the project table: %v", err)
	}
}

// Acceptance item 7's first half. "No boundary set" and "a boundary is set and nothing
// has been discovered yet" are different states, and a single count of zero cannot
// tell them apart — which is the whole reason doctor prints a word as well as a
// number.
func TestDoctorDistinguishesNoBoundaryFromABoundaryWithNothingDiscovered(t *testing.T) {
	isolate(t)
	boundary := filepath.Join(t.TempDir(), "boundary")
	if err := os.MkdirAll(boundary, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	fresh, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(fresh, "global boundary: not set") {
		t.Errorf("a fresh install does not report an unset boundary:\n%s", fresh)
	}

	if out, initErr := run(t, "init", "-g", boundary); initErr != nil {
		t.Fatalf("init -g error = %v; output:\n%s", initErr, out)
	}
	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	for _, want := range []string{"global boundary: set", "global boundary repositories: 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "global boundary: not set") {
		t.Errorf("a recorded boundary is still reported as unset:\n%s", out)
	}
}

// Acceptance item 5's visible half. Fail-closed is right and silent fail-closed is
// not: a user whose repositories stopped being registered has to be able to find out
// that the boundary was refused rather than absent.
func TestDoctorReportsARefusedBoundary(t *testing.T) {
	paths := isolate(t)
	boundary := filepath.Join(t.TempDir(), "outer", "boundary")
	if err := os.MkdirAll(boundary, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if out, err := run(t, "init", "-g", boundary); err != nil {
		t.Fatalf("init -g error = %v; output:\n%s", err, out)
	}
	widenTheRecordedBoundary(t, paths)

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "global boundary: refused") {
		t.Errorf("a hand-widened boundary is not reported as refused:\n%s", out)
	}
}

// Acceptance item 7's second half: the two counters render, and the refused one moves
// the integration state — which is the difference between the two, made visible where
// a user can act on it.
func TestDoctorReportsTheBoundaryCounters(t *testing.T) {
	paths := isolate(t)
	if err := health.New(paths.HealthFile).RecordScan(health.Scan{
		At:              time.Now().UTC(),
		Transcripts:     4,
		EventsWritten:   6,
		BoundarySkipped: 2,
		BoundaryRefused: 1,
	}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	for _, want := range []string{
		"global boundary directories gone: 2",
		"global boundary registrations refused: 1",
		"integration: collects nothing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// Acceptance item 8, on the one path TestDoctorOutputNamesNoPathOrLabel cannot reach:
// that case runs on a fresh install, where the boundary section has no path to leak
// in the first place. This one records one and then makes the same claim.
func TestDoctorBoundaryOutputNamesNoPath(t *testing.T) {
	isolate(t)
	const marker = "unmistakable-boundary"
	boundary := filepath.Join(t.TempDir(), marker)
	if err := os.MkdirAll(boundary, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if out, err := run(t, "init", "-g", boundary); err != nil {
		t.Fatalf("init -g error = %v; output:\n%s", err, out)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if strings.Contains(out, marker) {
		t.Errorf("output names the boundary:\n%s", out)
	}
	// The strongest form of the check, for the reason its neighbour gives: nothing in
	// this output has a legitimate slash in it, so a slash is a path.
	if strings.Contains(out, "/") {
		t.Errorf("output carries a path separator:\n%s", out)
	}
}

// ADR-0010: `init` is the only operation that writes. The boundary section reads the
// salt to verify a digest, and reading it must not be the thing that creates it — a
// diagnostic that handed a fresh install an identity salt would have written the one
// secret in the system without being asked.
func TestDoctorCreatesNoSaltOnAFreshInstall(t *testing.T) {
	paths := isolate(t)

	if _, _, err := runSplit(t, "doctor"); err != nil {
		t.Fatalf("doctor error = %v", err)
	}

	if _, statErr := os.Stat(paths.SaltFile); !os.IsNotExist(statErr) {
		t.Errorf("Stat(the salt) = %v, want doctor to have created none", statErr)
	}
}
