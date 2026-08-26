package claudecode

import (
	"testing"
	"time"
)

var sessionEpoch = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

func TestSessionStateClosesASessionPastTheThreshold(t *testing.T) {
	state := &SessionState{}
	state.Observe(0, "session-quiet", sessionEpoch, 0)

	if !state.Closed("session-quiet", Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(2 * time.Hour)}) {
		t.Fatal("Closed() = false, want true for a session silent past the threshold")
	}
}

func TestSessionStateKeepsASessionOpenInsideTheThreshold(t *testing.T) {
	state := &SessionState{}
	state.Observe(0, "session-live", sessionEpoch, 0)

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
	state.Observe(0, "session-seen", sessionEpoch, 0)

	if state.Closed("session-nobody-saw", Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(99 * time.Hour)}) {
		t.Fatal("Closed() = true for an unobserved session, want false: absence of evidence that a session is alive is not evidence it ended")
	}
}

func TestSessionStateNeverClosesWithoutAThreshold(t *testing.T) {
	state := &SessionState{}
	state.Observe(0, "session-quiet", sessionEpoch, 0)

	for name, stale := range map[string]Staleness{
		"zero value":     {},
		"no clock":       {Timeout: time.Hour},
		"no timeout":     {Now: sessionEpoch.Add(99 * time.Hour)},
		"zero both ways": {Timeout: 0, Now: time.Time{}},
	} {
		if state.Closed("session-quiet", stale) {
			t.Errorf("Closed() = true with %s staleness, want false", name)
		}
		if open := state.OpenSessions(stale); open != 1 {
			t.Errorf("OpenSessions() with %s staleness = %d, want 1", name, open)
		}
		if open, offset := state.SourceFloor(0, stale); !open || offset != 0 {
			t.Errorf("SourceFloor(0) with %s staleness = (%t, %d), want (true, 0)", name, open, offset)
		}
	}
}

func TestSessionStateKeepsTheLatestActivityRegardlessOfLineOrder(t *testing.T) {
	// Latest activity is a max, not last-wins: nothing in the transcript format
	// promises the entries are ordered, and an order-dependent fold could make two
	// scans of one transcript disagree about one session.
	state := &SessionState{}
	state.Observe(0, "session-unordered", sessionEpoch.Add(2*time.Hour), 200)
	state.Observe(0, "session-unordered", sessionEpoch, 0)

	if state.Closed("session-unordered", Staleness{Timeout: 2 * time.Hour, Now: sessionEpoch.Add(3 * time.Hour)}) {
		t.Fatal("Closed() = true, want false: the later observation is the session's last activity whichever order it arrived in")
	}
}

func TestSessionStateCursorFloorIsTheEarliestOpenSessionOffset(t *testing.T) {
	state := &SessionState{}
	state.Observe(0, "session-closed", sessionEpoch, 0)
	state.Observe(0, "session-open", sessionEpoch.Add(10*time.Hour), 120)
	state.Observe(0, "session-open", sessionEpoch.Add(11*time.Hour), 400)

	stale := Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(11 * time.Hour)}

	if open := state.OpenSessions(stale); open != 1 {
		t.Errorf("open sessions = %d, want 1", open)
	}
	open, offset := state.SourceFloor(0, stale)
	if !open {
		t.Fatal("SourceFloor(0) reports no open session, want one")
	}
	if offset != 120 {
		t.Errorf("cursor floor = %d, want 120: the earliest line of the still-open session", offset)
	}
}

func TestSessionStateCursorFloorIsUnsetWhenEverySessionClosed(t *testing.T) {
	state := &SessionState{}
	state.Observe(0, "session-one", sessionEpoch, 40)
	state.Observe(0, "session-two", sessionEpoch.Add(time.Minute), 900)

	stale := Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(9 * time.Hour)}

	if open := state.OpenSessions(stale); open != 0 {
		t.Errorf("OpenSessions() = %d, want 0 when nothing is open", open)
	}
	if open, offset := state.SourceFloor(0, stale); open || offset != 0 {
		t.Fatalf("SourceFloor(0) = (%t, %d), want (false, 0) when nothing is open", open, offset)
	}
}

// TestSessionStateKeepsOffsetsPerSource is constraint 5: liveness folds across
// every source of a session, but an offset only ever means something inside the
// source it came from. A walk-wide min over offsets from different files is a
// number about nothing.
func TestSessionStateKeepsOffsetsPerSource(t *testing.T) {
	state := &SessionState{}
	state.Observe(0, "session-split", sessionEpoch, 500)
	state.Observe(1, "session-split", sessionEpoch, 10)

	stale := Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(time.Minute)}

	if open, offset := state.SourceFloor(0, stale); !open || offset != 500 {
		t.Errorf("SourceFloor(0) = (%t, %d), want (true, 500): source 0's own earliest line", open, offset)
	}
	if open, offset := state.SourceFloor(1, stale); !open || offset != 10 {
		t.Errorf("SourceFloor(1) = (%t, %d), want (true, 10)", open, offset)
	}
	if open := state.OpenSessions(stale); open != 1 {
		t.Errorf("OpenSessions() = %d, want 1: two sources, one session", open)
	}
}

// TestSessionStateHoldsASourceFloorForASessionOpenElsewhere is AC 5: the floor of
// a file whose own lines all look quiet is still held while another file shows the
// same session active (ADR-0023 §5 read through ADR-0036).
func TestSessionStateHoldsASourceFloorForASessionOpenElsewhere(t *testing.T) {
	state := &SessionState{}
	state.Observe(0, "session-split", sessionEpoch, 64)
	state.Observe(1, "session-split", sessionEpoch.Add(10*time.Hour), 900)

	stale := Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(10 * time.Hour)}

	if state.Closed("session-split", stale) {
		t.Fatal("Closed() = true, want false: source 1 shows the session active")
	}
	if open, offset := state.SourceFloor(0, stale); !open || offset != 64 {
		t.Errorf("SourceFloor(0) = (%t, %d), want (true, 64): the quiet source's floor is held too", open, offset)
	}
}

func TestSessionStateNeverClosesABlindSession(t *testing.T) {
	state := &SessionState{}
	state.Observe(0, "session-blind", sessionEpoch, 32)
	state.MarkBlind("session-blind")

	stale := Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(99 * time.Hour)}

	if state.Closed("session-blind", stale) {
		t.Error("Closed() = true for a blind session, want false")
	}
	if open := state.OpenSessions(stale); open != 1 {
		t.Errorf("OpenSessions() = %d, want 1: a blind session stays open", open)
	}
	if open, offset := state.SourceFloor(0, stale); !open || offset != 32 {
		t.Errorf("SourceFloor(0) = (%t, %d), want (true, 32)", open, offset)
	}
}

// TestSessionStateBlindnessIsPerSession is constraint 11: one unreadable line
// must not disable the staleness rule for every session on the machine.
func TestSessionStateBlindnessIsPerSession(t *testing.T) {
	state := &SessionState{}
	state.Observe(0, "session-blind", sessionEpoch, 0)
	state.Observe(0, "session-clear", sessionEpoch, 100)
	state.MarkBlind("session-blind")

	stale := Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(99 * time.Hour)}

	if state.Closed("session-blind", stale) {
		t.Error("Closed(\"session-blind\") = true, want false")
	}
	if !state.Closed("session-clear", stale) {
		t.Error("Closed(\"session-clear\") = false, want true: another session's blindness is not this one's")
	}
}

func TestSessionStateNeverMarksAnUnobservedSessionBlind(t *testing.T) {
	state := &SessionState{}
	state.MarkBlind("session-nobody-saw")

	stale := Staleness{Timeout: time.Hour, Now: sessionEpoch.Add(99 * time.Hour)}

	if open := state.OpenSessions(stale); open != 0 {
		t.Fatalf("OpenSessions() = %d, want 0: marking an unobserved session blind must not observe it", open)
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

	state.Observe(0, "session-gone", sessionEpoch, 0)
	state.Observe(0, "session-here", sessionEpoch.Add(59*time.Minute), 64)

	if !state.Closed("session-gone", stale) {
		t.Error("Closed(\"session-gone\") = false, want true")
	}
	if state.Closed("session-here", stale) {
		t.Error("Closed(\"session-here\") = true, want false")
	}
	if open := state.OpenSessions(stale); open != 1 {
		t.Errorf("OpenSessions() = %d, want 1", open)
	}
	if open, offset := state.SourceFloor(0, stale); !open || offset != 64 {
		t.Errorf("SourceFloor(0) = (%t, %d), want (true, 64)", open, offset)
	}
}
