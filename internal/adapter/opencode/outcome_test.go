package opencode

import (
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

func TestCompletedIsOK(t *testing.T) {
	outcome, terminal, known := statusOutcome("completed")
	if outcome != record.OutcomeOK || !terminal || !known {
		t.Fatalf("statusOutcome(completed) = (%q, %t, %t), want (ok, true, true)", outcome, terminal, known)
	}
}

func TestErrorIsError(t *testing.T) {
	outcome, terminal, known := statusOutcome("error")
	if outcome != record.OutcomeError || !terminal || !known {
		t.Fatalf("statusOutcome(error) = (%q, %t, %t), want (error, true, true)", outcome, terminal, known)
	}
}

func TestPendingAndRunningAreNotTerminal(t *testing.T) {
	for _, status := range []string{"pending", "running"} {
		outcome, terminal, known := statusOutcome(status)
		if outcome != "" || terminal || !known {
			t.Errorf("statusOutcome(%q) = (%q, %t, %t), want (\"\", false, true)", status, outcome, terminal, known)
		}
	}
}

func TestAnUnmeasuredStatusIsUnknownAndNotTerminal(t *testing.T) {
	// A status this build has not measured is neither terminal nor an error: it is
	// a family nobody has seen, so it stays unemitted and is counted as blindness
	// rather than absorbed (ADR-0005, plan §3.3).
	for _, status := range []string{"cancelled", "timeout", "", "COMPLETED", "completed "} {
		outcome, terminal, known := statusOutcome(status)
		if known || terminal || outcome != "" {
			t.Errorf("statusOutcome(%q) = (%q, %t, %t), want (\"\", false, false)", status, outcome, terminal, known)
		}
	}
}

func TestTheMappingIsAPositiveSet(t *testing.T) {
	// The guard against a later default: arm that guesses. Exactly four inputs are
	// recognised, and every other string in the domain is not.
	recognised := map[string]bool{"completed": true, "error": true, "pending": true, "running": true}
	for _, status := range []string{
		"completed", "error", "pending", "running",
		"ok", "success", "failed", "failure", "done", "aborted", "denied",
		"complete", "errored", "pending_approval", "Running", "0", "null",
	} {
		_, _, known := statusOutcome(status)
		if known != recognised[status] {
			t.Errorf("statusOutcome(%q) known = %t, want %t", status, known, recognised[status])
		}
	}
}
