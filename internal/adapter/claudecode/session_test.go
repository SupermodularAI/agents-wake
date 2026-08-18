package claudecode

import (
	"testing"
	"time"
)

var sessionEpoch = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

func TestSessionStateClosesASessionPastTheThreshold(t *testing.T) {
	state := &SessionState{}
	state.Observe("session-quiet", sessionEpoch, 0)

	if !state.Closed("session-quiet", Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(2 * time.Hour)}) {
		t.Fatal("Closed() = false, want true for a session silent past the threshold")
	}
}

func TestSessionStateKeepsASessionOpenInsideTheThreshold(t *testing.T) {
	state := &SessionState{}
	state.Observe("session-live", sessionEpoch, 0)

	if state.Closed("session-live", Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(30 * time.Minute)}) {
		t.Error("Closed() = true inside the threshold, want false")
	}
	// Exactly the threshold is still open: the comparison errs toward buffering,
	// because an interrupted record cannot be taken back (ADR-0015, ADR-0004).
	if state.Closed("session-live", Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(time.Hour)}) {
		t.Error("Closed() = true at exactly the threshold, want false")
	}
}

func TestSessionStateNeverClosesAnUnobservedSession(t *testing.T) {
	state := &SessionState{}
	state.Observe("session-seen", sessionEpoch, 0)

	if state.Closed("session-nobody-saw", Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(99 * time.Hour)}) {
		t.Fatal("Closed() = true for an unobserved session, want false: absence of evidence that a session is alive is not evidence it ended")
	}
}

func TestSessionStateNeverClosesWithoutAThreshold(t *testing.T) {
	state := &SessionState{}
	state.Observe("session-quiet", sessionEpoch, 0)

	for name, stale := range map[string]Staleness{
		"zero value":     {},
		"no clock":       {Timeout: time.Hour},
		"no timeout":     {Now: sessionEpoch.Add(99 * time.Hour)},
		"zero both ways": {Timeout: 0, Now: time.Time{}},
	} {
		if state.Closed("session-quiet", stale) {
			t.Errorf("Closed() = true with %s staleness, want false", name)
		}
		if open, offset := state.CursorFloor(stale); open != 1 || offset != 0 {
			t.Errorf("CursorFloor() with %s staleness = (%d, %d), want (1, 0)", name, open, offset)
		}
	}
}

func TestSessionStateKeepsTheLatestActivityRegardlessOfLineOrder(t *testing.T) {
	// Latest activity is a max, not last-wins: nothing in the transcript format
	// promises the entries are ordered, and an order-dependent fold could make two
	// scans of one transcript disagree about one session.
	state := &SessionState{}
	state.Observe("session-unordered", sessionEpoch.Add(2*time.Hour), 200)
	state.Observe("session-unordered", sessionEpoch, 0)

	if state.Closed("session-unordered", Staleness{Timeout: 2 * time.Hour, Now: sessionEpoch.Add(3 * time.Hour)}) {
		t.Fatal("Closed() = true, want false: the later observation is the session's last activity whichever order it arrived in")
	}
}

func TestSessionStateCursorFloorIsTheEarliestOpenSessionOffset(t *testing.T) {
	state := &SessionState{}
	state.Observe("session-closed", sessionEpoch, 0)
	state.Observe("session-open", sessionEpoch.Add(10*time.Hour), 120)
	state.Observe("session-open", sessionEpoch.Add(11*time.Hour), 400)

	open, offset := state.CursorFloor(Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(11 * time.Hour)})

	if open != 1 {
		t.Errorf("open sessions = %d, want 1", open)
	}
	if offset != 120 {
		t.Errorf("cursor floor = %d, want 120: the earliest line of the still-open session", offset)
	}
}

func TestSessionStateCursorFloorIsUnsetWhenEverySessionClosed(t *testing.T) {
	state := &SessionState{}
	state.Observe("session-one", sessionEpoch, 40)
	state.Observe("session-two", sessionEpoch.Add(time.Minute), 900)

	open, offset := state.CursorFloor(Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(9 * time.Hour)})

	if open != 0 || offset != 0 {
		t.Fatalf("CursorFloor() = (%d, %d), want (0, 0) when nothing is open", open, offset)
	}
}

// TestSessionCloseIsDecidableWithoutTheIngestPath is the ticket's acceptance
// criterion that the session-closed determination is callable from this package
// directly, independent of ingest and health. It builds values only — no store, no
// config, no health report, no ingest call — and T121's ADR-0023 consumer calls
// exactly these three methods rather than deriving a second staleness rule.
func TestSessionCloseIsDecidableWithoutTheIngestPath(t *testing.T) {
	state := &SessionState{}
	stale := Staleness{Timeout: 30 * time.Minute, Now: sessionEpoch.Add(time.Hour)}

	state.Observe("session-gone", sessionEpoch, 0)
	state.Observe("session-here", sessionEpoch.Add(59*time.Minute), 64)

	if !state.Closed("session-gone", stale) {
		t.Error("Closed(\"session-gone\") = false, want true")
	}
	if state.Closed("session-here", stale) {
		t.Error("Closed(\"session-here\") = true, want false")
	}
	if open, offset := state.CursorFloor(stale); open != 1 || offset != 64 {
		t.Errorf("CursorFloor() = (%d, %d), want (1, 64)", open, offset)
	}
}
