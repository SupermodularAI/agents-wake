package health

import (
	"errors"
	"testing"
	"time"
)

// scannedAt is the fixed moment every case below uses for a report that has been
// scanned. A fixed time rather than time.Now(), so a failure reads the same twice.
var scannedAt = time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)

// The two unreadable arms come first because a number derived from an input nobody
// could read is not a number worth reading. This case has counters that would
// otherwise read as "collecting", so it pins the precedence rather than the arm.
func TestDiagnoseReportsCountersUnreadableBeforeAnythingElse(t *testing.T) {
	got := Diagnose(
		Report{Scan: Scan{At: scannedAt, EventsWritten: 4}},
		errors.New("corrupt"),
		errors.New("bad settings"),
	)

	if got.State != StateCountersUnreadable {
		t.Errorf("State = %q, want %q", got.State, StateCountersUnreadable)
	}
}

func TestDiagnoseReportsHooksUnreadableBeforeTheCounts(t *testing.T) {
	got := Diagnose(Report{Scan: Scan{At: scannedAt, EventsWritten: 4}}, nil, errors.New("bad settings"))

	if got.State != StateHooksUnreadable {
		t.Errorf("State = %q, want %q", got.State, StateHooksUnreadable)
	}
}

func TestDiagnoseReportsNeverScannedForAZeroScanTime(t *testing.T) {
	got := Diagnose(Report{}, nil, nil)

	if got.State != StateNeverScanned {
		t.Errorf("State = %q, want %q", got.State, StateNeverScanned)
	}
	if got.ScanKnown {
		t.Error("ScanKnown = true; a report nobody has scanned has no scan time")
	}
}

// ADR-0010's distinction, which is the whole reason the counters exist: a scan that
// could not read a source is not a scan that read everything and found nothing.
// Every case mirrors one pinned by internal/cli/doctor_test.go, so a divergence
// between this derivation and the frozen output shows up here first.
func TestDiagnoseDistinguishesCollectsNothingFromCollectsZero(t *testing.T) {
	for _, c := range []struct {
		name string
		scan Scan
		want State
	}{
		{"a source that could not be read", Scan{Unreadable: 1}, StateCollectsNothing},
		{"a source that could not be parsed", Scan{ParseErrors: 1}, StateCollectsNothing},
		{"a project entry this build refuses", Scan{Transcripts: 1, Skipped: 1, RefusedProjects: 1}, StateCollectsNothing},
		{"a call whose primitive name this build refuses", Scan{Transcripts: 1, RefusedCalls: 1}, StateCollectsNothing},
		{"every source read and none held an event", Scan{Transcripts: 3, Skipped: 3}, StateCollectsZero},
		{"events written", Scan{EventsWritten: 4}, StateCollecting},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.scan.At = scannedAt

			if got := Diagnose(Report{Scan: c.scan}, nil, nil); got.State != c.want {
				t.Errorf("State = %q, want %q", got.State, c.want)
			}
		})
	}
}

// A buffered call is a number that is not final yet, and a call that resolved to
// interrupted is an invocation the store has, carrying the outcome that says it
// never finished (ADR-0015). Neither is a source nobody could read, so neither may
// move the state into "collects nothing" — in either direction.
func TestDiagnoseDoesNotCallPendingOrInterruptedCallsLostCollection(t *testing.T) {
	for _, c := range []struct {
		name string
		scan Scan
		want State
	}{
		{"alongside events written", Scan{EventsWritten: 1, PendingCalls: 5, InterruptedCalls: 3}, StateCollecting},
		{"with nothing written", Scan{Transcripts: 1, PendingCalls: 5, InterruptedCalls: 3}, StateCollectsZero},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.scan.At = scannedAt

			got := Diagnose(Report{Scan: c.scan}, nil, nil)
			if got.State == StateCollectsNothing {
				t.Error("pending or interrupted calls were reported as lost collection")
			}
			if got.State != c.want {
				t.Errorf("State = %q, want %q", got.State, c.want)
			}
		})
	}
}

// The scan time the printer renders is a decision, not a format: a counter file this
// build could not read has no scan time rather than a zero one, and rendering the
// zero time as a timestamp would date the last scan to year one.
func TestDiagnoseCarriesTheScanTimeItKnows(t *testing.T) {
	scanned := Report{Scan: Scan{At: scannedAt, EventsWritten: 4}}

	got := Diagnose(scanned, nil, nil)
	if !got.ScanKnown {
		t.Error("ScanKnown = false for a report carrying a scan time")
	}
	if !got.ScanAt.Equal(scannedAt) {
		t.Errorf("ScanAt = %v, want %v", got.ScanAt, scannedAt)
	}

	refused := Diagnose(scanned, errors.New("corrupt"), nil)
	if refused.ScanKnown {
		t.Error("ScanKnown = true for a counter file nobody could read")
	}
	if !refused.ScanAt.IsZero() {
		t.Errorf("ScanAt = %v; a refused counter file must not hand the printer a stale timestamp", refused.ScanAt)
	}

	if never := Diagnose(Report{}, nil, nil); never.ScanKnown {
		t.Error("ScanKnown = true for a report nobody has scanned")
	}
}

// The closed-enum tripwire (ADR-0007 applied to diagnostics): a future arm inventing
// a seventh word fails here, rather than in a substring assertion on CLI output.
func TestDiagnoseOnlyEverReturnsAKnownState(t *testing.T) {
	known := map[State]bool{
		StateCountersUnreadable: true,
		StateHooksUnreadable:    true,
		StateNeverScanned:       true,
		StateCollectsNothing:    true,
		StateCollectsZero:       true,
		StateCollecting:         true,
	}

	reports := []Report{
		{},
		{Scan: Scan{At: scannedAt}},
		{Scan: Scan{At: scannedAt, Unreadable: 1}},
		{Scan: Scan{At: scannedAt, ParseErrors: 1}},
		{Scan: Scan{At: scannedAt, Transcripts: 1, Skipped: 1, RefusedProjects: 1}},
		{Scan: Scan{At: scannedAt, Transcripts: 1, RefusedCalls: 1}},
		{Scan: Scan{At: scannedAt, Transcripts: 3, Skipped: 3}},
		{Scan: Scan{At: scannedAt, EventsWritten: 4}},
		{Scan: Scan{At: scannedAt, EventsWritten: 1, PendingCalls: 5, InterruptedCalls: 3}},
		{Scan: Scan{At: scannedAt, EventsWritten: 1, BoundarySkipped: 2}},
		{Scan: Scan{At: scannedAt, EventsWritten: 1, BoundaryRefused: 2}},
	}
	failures := []error{nil, errors.New("refused")}

	for _, report := range reports {
		for _, countersErr := range failures {
			for _, hooksErr := range failures {
				if got := Diagnose(report, countersErr, hooksErr); !known[got.State] {
					t.Errorf("Diagnose(%+v, %v, %v).State = %q, which is not one of the six", report, countersErr, hooksErr, got.State)
				}
			}
		}
	}
}

// A registration the boundary could not complete is reported as its own number and
// does not blind the integration state.
//
// It is a standing fact about a directory rather than a source nobody could read: the
// same directory is re-observed and re-refused by every scan, nothing records that it
// was refused, and no command removes the entry it nests with. Folding it in would put
// a machine that is collecting normally into "collects nothing" permanently, which is
// the reason Skipped and an ambiguous skill run are not in it either. The counter is
// what says collection was lost there, and doctor prints it beside the boundary's own
// state.
func TestDiagnoseDoesNotLetARefusedBoundaryRegistrationBlindTheIntegrationState(t *testing.T) {
	got := Diagnose(Report{Scan: Scan{At: scannedAt, Transcripts: 2, EventsWritten: 4, BoundaryRefused: 1}}, nil, nil)
	if got.State != StateCollecting {
		t.Errorf("State = %q, want %q", got.State, StateCollecting)
	}
}

// A directory the boundary discovered and that is no longer there is an honest zero,
// for the reason Skipped is one: there is nothing left there to read, so nothing was
// lost by not registering it. Folding it in would put every machine that has ever
// deleted a project directory permanently into "collects nothing".
func TestDiagnoseDoesNotCallAVanishedBoundaryDirectoryLostCollection(t *testing.T) {
	got := Diagnose(Report{Scan: Scan{At: scannedAt, Transcripts: 2, EventsWritten: 4, BoundarySkipped: 3}}, nil, nil)
	if got.State != StateCollecting {
		t.Errorf("State = %q, want %q", got.State, StateCollecting)
	}
}
