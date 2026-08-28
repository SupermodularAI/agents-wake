package health

import "time"

// State is the one word ADR-0010 asks for, and it is exactly one of six.
//
// It is a closed set of constants rather than free text, so a state this package
// cannot represent is a compile error rather than a runtime string (ADR-0007 as
// applied to diagnostics). It is derived on every read and never written to the
// counter file: the file stays a file of counts, flags and times.
type State string

const (
	StateCountersUnreadable State = "counters unreadable"
	StateHooksUnreadable    State = "hooks unreadable"
	StateNeverScanned       State = "never scanned"
	StateCollectsNothing    State = "collects nothing"
	StateCollectsZero       State = "collects zero"
	StateCollecting         State = "collecting"
)

// StoreRebuild is what the last scan left the spool's own readability in, and it is
// exactly one of three.
//
// It is separate from State because it is a different question with a different
// remedy: State says what the last scan managed to collect, this says whether the
// store it collected into still holds records this build cannot read. The needed
// value names the command, because the remedy is a scan the user has to ask for —
// the hook-fired one may not perform it (see Scan.StaleRebuilt) — and a state word
// nobody can act on is not a diagnosis.
type StoreRebuild string

const (
	StoreRebuildNotNeeded StoreRebuild = "not needed"
	StoreRebuildDone      StoreRebuild = "done"
	StoreRebuildNeeded    StoreRebuild = "run wake ingest --rebuild"
)

// Diagnosis is everything doctor prints that is derived rather than counted: the one
// state word, whether there is a scan time to render at all, and whether the spool
// itself still needs rebuilding.
//
// ScanKnown carries the decision, so the printer above only formats: a counter file
// this build could not read has no scan time rather than a zero one, and rendering
// the zero time as a timestamp would date the last scan to year one. ADR-0001 puts
// that decision here — internal/cli parses and prints, and this is neither.
//
// It is a return value, not a field: nothing here is persisted, and the counter file
// stays a file of counts, flags and times (see the package comment).
type Diagnosis struct {
	State State
	// ScanAt is the moment the last scan ran, meaningful only when ScanKnown.
	ScanAt    time.Time
	ScanKnown bool
	// StoreRebuild is the spool's own readability, which is a question about the store
	// rather than about the sources State describes.
	StoreRebuild StoreRebuild
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
// A subagent run refused for want of a name is not in it, and it is the counter that
// is closest to being. The run happened, nothing names it and no number carries it, so
// it is lost collection in exactly the sense a refused call is — but it is a standing
// fact about a transcript rather than something a scan found out. ADR-0036 §2 measures
// 2% of real subagent transcripts declaring no name at all and refuses to name them
// from the harness's documented default, so no Wake release makes that count fall; with
// no incremental cursor (T020, T102) every scan re-reads the whole history and refuses
// the same runs again. Folding it in would pin a machine writing thousands of records
// to "collects nothing" for good, which is the same reason a refused boundary
// registration and an ambiguous skill run are excluded. RefusedCalls keeps the arm, so
// what that arm exists to catch — a harness renaming the field a primitive's identity
// lives in — is unaffected, and doctor prints this counter on its own line whatever the
// state word says.
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
// Neither boundary counter is in it, and the refused one is the interesting case.
//
// A directory the recorded global root encloses whose repository could not be
// registered did lose collection: it has no identity, so every session in it counted
// as belonging to nothing. What keeps it out of this arm is that it is a standing fact
// rather than something a scan found out — every scan re-observes the same directory
// and refuses it again, nothing records that it was refused, and no command removes
// the entry it nests with (ADR-0019 §5 is the usual refusal). A state word driven by
// that counter could never change again, so a machine collecting normally would read
// as "collects nothing" for good, and a diagnosis that cannot change is not one. This
// is the same reason Skipped and an ambiguous skill run are excluded, and the counter
// is what reports the loss: doctor prints it beside the boundary's own state, which is
// where a fact about the boundary belongs rather than in the machine-wide word.
//
// A boundary directory that is gone is not in it either, for the reason Skipped is
// not: there is nothing left there to read, so nothing was lost by not registering it.
// Folding it in would put every machine that has ever deleted a project directory
// permanently into "collects nothing", since nothing about that directory will ever
// change.
//
// A record from another schema version is in the first arm, and only while it is
// still there. Every consumer reads the spool through store.Entries, which drops it
// the way it drops any line that does not decode, so an invocation that was collected
// is now carried by no number — the same shape as a source nobody could read, arrived
// at from the other end. The scan that discarded and re-derived it is the exception,
// and it is the same exception a refused project entry's remedy is: the state word
// must not contradict the events that scan just wrote, and the next scan reports zero.
// Unlike a refused boundary registration this is a state that changes, because the
// StoreRebuild line beside it names the command that changes it.
//
// An ambiguous skill run is not in it either. The transcript was read completely and
// the collapse is a documented decision (ADR-0023's accepted limitation), not
// blindness: no transcript signal separates one slash-command run from two with no tool
// trace. Folding it in would put every session carrying a repeated slash command into
// "collects nothing", permanently, since nothing about that session will ever change.
//
// A skipped typed invocation is not in it, and it is the clearest case of the rule
// rather than a borderline one. A tag naming something this machine has no primitive
// for is not lost collection at all — a typed CLI built-in was never Wake's to collect,
// so nothing was lost — which is why ADR-0036 §3 settles it as a skip on a counter of
// its own rather than on RefusedCalls. On top of that it is the common case, roughly
// 101 of 136 observed occurrences, and every scan re-reads the whole history and
// re-skips the same built-ins: folding it in would pin every machine to "collects
// nothing" permanently while it writes thousands of records. RefusedCalls keeps its own
// arm, so the drift signal that arm exists for — a harness renaming the field a
// primitive's identity lives in — is unaffected.
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
	diagnosis := Diagnosis{State: StateCollecting, StoreRebuild: storeRebuild(report.Scan)}
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
	case report.Scan.Unreadable > 0 || report.Scan.ParseErrors > 0 || report.Scan.RefusedProjects > 0 || report.Scan.RefusedCalls > 0:
		diagnosis.State = StateCollectsNothing
	case diagnosis.StoreRebuild == StoreRebuildNeeded:
		// Records in the store that no surface can read, and the scan that found them
		// could not put them back: the numbers below are missing all of them and
		// nobody knows how much. It is in this arm rather than beside the honest zero
		// for exactly the reason a refused project entry is — and unlike a refused
		// boundary registration, it is a state that changes, because the line beside
		// it names the command that changes it.
		diagnosis.State = StateCollectsNothing
	case report.Scan.EventsWritten == 0:
		diagnosis.State = StateCollectsZero
	}
	return diagnosis
}

// storeRebuild reads the two stale-spool counters as the one question a user can act
// on: does the store still hold records this build cannot read?
func storeRebuild(scan Scan) StoreRebuild {
	switch {
	case scan.StaleRecords == 0:
		return StoreRebuildNotNeeded
	case scan.StaleRebuilt:
		return StoreRebuildDone
	default:
		return StoreRebuildNeeded
	}
}
