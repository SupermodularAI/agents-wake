package health

import "time"

// State is the one word ADR-0010 asks for, and it is exactly one of six.
//
// It is a closed set of constants rather than free text, so a state this package
// cannot represent is a compile error rather than a runtime string (ADR-0007 as
// applied to diagnostics). It is derived on every read and never written to the
// counter file: the file stays a file of ints and times.
type State string

const (
	StateCountersUnreadable State = "counters unreadable"
	StateHooksUnreadable    State = "hooks unreadable"
	StateNeverScanned       State = "never scanned"
	StateCollectsNothing    State = "collects nothing"
	StateCollectsZero       State = "collects zero"
	StateCollecting         State = "collecting"
)

// Diagnosis is everything doctor prints that is derived rather than counted: the one
// state word, and whether there is a scan time to render at all.
//
// ScanKnown carries the decision, so the printer above only formats: a counter file
// this build could not read has no scan time rather than a zero one, and rendering
// the zero time as a timestamp would date the last scan to year one. ADR-0001 puts
// that decision here — internal/cli parses and prints, and this is neither.
//
// It is a return value, not a field: nothing here is persisted, and the counter file
// stays a file of ints and times (see the package comment).
type Diagnosis struct {
	State State
	// ScanAt is the moment the last scan ran, meaningful only when ScanKnown.
	ScanAt    time.Time
	ScanKnown bool
}

// Diagnose derives the one word ADR-0010 asks for, and it is exactly one of six.
//
// "collects nothing" and "collects zero" are the pair that matters. A source that
// could not be read or could not be parsed means the numbers below are missing
// something and nobody knows how much; everything read and nothing found means the
// numbers are complete and the answer is zero. Reporting the first as the second is
// how `unused` would come to recommend removing something the user relies on.
//
// A refused project entry belongs in the first arm for the same reason: an entry
// this build will not resolve is attribution it could not perform, so every
// transcript belonging to that repository counted as holding nothing, and the
// numbers are missing all of it. It is also not a rare tamper case — it is what
// every project table written before match_mac became required looks like on its
// first scan, and the remedy (`wake init` in the repository) is undiscoverable if
// doctor calls the situation a complete count of zero.
//
// A refused call belongs in it for the same reason, and it is the arm that catches
// format drift: a primitive Wake found but could not name was invoked, and the
// numbers below are missing that invocation. Inferring a name would be worse than
// losing it (plan §3.3), so the drop is correct and reporting it is what keeps the
// drop honest — a harness renaming the field a primitive's identity lives in stops
// collection for that whole kind, and this is the only line that says so.
//
// Skipped is deliberately not in it. A transcript whose working directory belongs to
// no consented repository was read completely and collected nothing because consent
// says so, and an unterminated call is a number that is not final yet rather than
// one nobody could read (ADR-0015). Both are honest zeroes. Pending and interrupted
// calls are not in it either, and for the reason already stated there — an
// unterminated call is a number that is not final yet, and a call that resolved to
// interrupted is an invocation the store has, carrying the outcome that says it
// never finished (ADR-0015). Both are honest, and neither is a source nobody could
// read.
//
// A boundary registration that was refused belongs in the first arm, and it is the
// arm that catches the boundary's own failure mode: a directory the recorded global
// root encloses whose repository could not be registered has no identity, so every
// session in it counted as belonging to nothing, and the numbers are missing all of
// it. Most often the discovered root nests with one already recorded (ADR-0019 §5),
// and the remedy — consenting the inner root or the outer one, not both — is
// undiscoverable if this is reported as a complete count of zero.
//
// A boundary directory that is gone is deliberately not in it, for the reason Skipped
// is not: there is nothing left there to read, so nothing was lost by not registering
// it. Folding it in would put every machine that has ever deleted a project directory
// permanently into "collects nothing", since nothing about that directory will ever
// change.
//
// An ambiguous skill run is not in it either. The transcript was read completely and
// the collapse is a documented decision (ADR-0023's accepted limitation), not
// blindness: no transcript signal separates one slash-command run from two with no tool
// trace. Folding it in would put every session carrying a repeated slash command into
// "collects nothing", permanently, since nothing about that session will ever change.
//
// An input this build cannot read is its own state rather than an error: a
// diagnostic that failed in the situation it exists for is worse than one that says
// what it could not determine. Both unreadable states come first, because a number
// derived from an input nobody could read is not a number worth reading — and a
// settings file this build refuses is exactly what a user comes here after `wake
// init` told them to fix it.
//
// That is why the two read failures are parameters and the result is a state rather
// than an error: the caller has already tried and failed, and this reports what it
// could not determine instead of failing in turn. ADR-0016 keeps the hook-invoked
// scan silent, so doctor is the only surface that can say so.
func Diagnose(report Report, countersErr, hooksErr error) Diagnosis {
	diagnosis := Diagnosis{State: StateCollecting}
	if countersErr == nil && !report.Scan.At.IsZero() {
		diagnosis.ScanAt, diagnosis.ScanKnown = report.Scan.At, true
	}

	switch {
	case countersErr != nil:
		diagnosis.State = StateCountersUnreadable
	case hooksErr != nil:
		diagnosis.State = StateHooksUnreadable
	case report.Scan.At.IsZero():
		diagnosis.State = StateNeverScanned
	case report.Scan.Unreadable > 0 || report.Scan.ParseErrors > 0 || report.Scan.RefusedProjects > 0 || report.Scan.RefusedCalls > 0 || report.Scan.BoundaryRefused > 0:
		diagnosis.State = StateCollectsNothing
	case report.Scan.EventsWritten == 0:
		diagnosis.State = StateCollectsZero
	}
	return diagnosis
}
