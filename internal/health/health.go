// Package health persists what Wake's last scan and last hook change managed to
// do, as counts and timestamps and nothing else.
//
// It exists so a scan that could not read a source is distinguishable from a scan
// that read everything and found nothing (ADR-0010's "collects nothing" versus
// "collects zero"), without the hook-invoked path having to report anything:
// ADR-0016 requires that path to exit 0 in silence, so the signal has to be left
// somewhere `doctor` can read it later.
//
// Every field is an int or a time. There is no free-text field, and there is no
// field that could hold one: `doctor` output is what people paste into issues, so a
// counter carries a count and never a line, a path, or a label (ADR-0019 §7,
// ADR-0007 applied to diagnostics). A test asserts the field types, because the
// temptation a later change will feel is to add "and here is why" as a string.
//
// The file is derived and non-precious. It lives under the data root, and deleting
// the data root stays safe (ADR-0014): what is lost is one scan's worth of
// diagnostics, which the next scan replaces.
package health

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
	"github.com/SupermodularAI/agents-wake/internal/lockfile"
)

// reportVersion is stamped on every write. A file from a future format read as this
// one would report counters that mean something else, so an unrecognised version
// stops the read instead.
//
// Bumped to 2 when the scan gained the pending and interrupted counters (T114): a
// version-1 file read as this format would report 0 for two counters nobody
// measured, which is the "collects zero for a state nobody measured" failure this
// package's comment forbids. The file is derived and non-precious (ADR-0014), so
// refusing it costs one scan's diagnostics.
const reportVersion = 2

// reportFileMode is the mode the counter file is written with. It holds no path and
// no label, but it is state about this user's machine and the rest of the local
// layout is 0600.
const reportFileMode = fs.FileMode(0o600)

// errInvalidReport is returned for a counter file this build cannot read. It is a
// refusal rather than a zero Report for the reason the package comment gives: an
// unreadable file reported as all-zeroes renders "collects zero" for a state nobody
// measured.
var errInvalidReport = errors.New("invalid health report")

// Report is the whole file: what the last scan did, and what the last hook change
// did. The two are kept apart because they are different events — a scan does not
// invalidate what `init` recorded about the hooks, and vice versa.
type Report struct {
	Version int   `json:"version"`
	Scan    Scan  `json:"scan"`
	Hooks   Hooks `json:"hooks"`
}

// Scan is what the last import managed to do.
//
// Replaced wholesale on every scan, not accumulated: a count that only ever grew
// would make one historical read failure mark every later clean scan as dirty,
// destroying the distinction this package exists to draw.
type Scan struct {
	At              time.Time `json:"at"`
	Transcripts     int       `json:"transcripts"`
	Unreadable      int       `json:"unreadable"`
	ParseErrors     int       `json:"parse_errors"`
	Skipped         int       `json:"skipped"`
	EventsWritten   int       `json:"events_written"`
	RefusedProjects int       `json:"refused_projects"`
	// RefusedCalls counts primitive invocations a reader found but could not name:
	// the invocation happened and no number carries it. It is separate from
	// ParseErrors, which counts lines that were unusable, and from Skipped, which is
	// an honest zero — this one is collection that was lost, and it is what a
	// harness renaming the field a primitive's identity lives in looks like.
	RefusedCalls int `json:"refused_calls"`
	// PendingCalls counts tool calls the last scan found unterminated whose session is
	// still inside the staleness window: a number that is not final yet, not collection
	// that was lost (ADR-0015). It is deliberately not one of integrationState's
	// "collects nothing" reasons.
	PendingCalls int `json:"pending_calls"`
	// InterruptedCalls counts calls resolved to outcome interrupted because their
	// session had gone quiet past the threshold. A call that resolves this way is a
	// fact worth surfacing rather than lost collection: the invocation is in the store,
	// carrying the outcome that says it never finished.
	//
	// Like every counter here it describes the window the last scan read, not the work
	// that scan newly did — and until an incremental cursor lands (T020, T102) that
	// window is the whole history, so a scan that resolved nothing new still reports
	// what earlier scans resolved. Reading it as "this scan interrupted N calls" would
	// overstate it.
	InterruptedCalls int `json:"interrupted_calls"`
}

// Hooks is what the last `init` or `remove` managed to do. KeptOwned is the partial
// state the ticket asks to surface: a group carrying Wake's marker that `remove`
// deliberately left alone because somebody had edited it.
type Hooks struct {
	At        time.Time `json:"at"`
	Installed int       `json:"installed"`
	Removed   int       `json:"removed"`
	KeptOwned int       `json:"kept_owned"`
}

// Store owns the counter file at one local path and the lock that serializes a
// read-modify-write of it.
type Store struct {
	path     string
	lockPath string
}

// New creates a Store over path. It creates nothing until a Record call: resolving
// where the counters live is separate from having any.
func New(path string) *Store {
	// A sidecar beside the file, matching internal/store and internal/inventory: the
	// lock must be a separate always-empty file, because opening the counter file
	// itself with O_CREATE would leave a zero-length file behind on a crash and a
	// zero-length file does not parse.
	return &Store{path: path, lockPath: path + ".lock"}
}

// RecordScan replaces the scan counters and leaves the hook counters as they are.
func (s *Store) RecordScan(scan Scan) error {
	return s.update(func(report *Report) { report.Scan = scan })
}

// RecordHooks replaces the hook counters and leaves the scan counters as they are.
func (s *Store) RecordHooks(hooks Hooks) error {
	return s.update(func(report *Report) { report.Hooks = hooks })
}

// update applies mutate to the stored report and republishes it.
//
// The read and the write happen inside one hold of the lock because the file is
// republished whole: a scan recording its counters from a report read before a
// concurrent `wake remove` recorded its own would erase the hook counters, and a
// trigger firing while the user runs `remove` is exactly that race.
//
// A file this build cannot read is replaced rather than refused, which is the
// opposite of what Read does and deliberately so. Reading it as zeroes would render
// "collects zero" for a state nobody measured; refusing to write over it would make
// an unparseable diagnostics file stop `init`, `ingest` and `remove` — none of which
// the counters are a precondition for — with a recovery (delete the file) that
// nothing tells the user about. The file is derived and non-precious (ADR-0014), and
// the caller replaces the half it owns wholesale, so nothing that was still readable
// is lost.
func (s *Store) update(mutate func(*Report)) error {
	return lockfile.WithLock(s.lockPath, func() error {
		report, err := s.read()
		if err != nil && !errors.Is(err, errInvalidReport) {
			// Everything else — a permission error, a directory in the file's place —
			// is a fault of the local layout rather than of the file's contents, and
			// replacing what could not be read is not this package's call to make.
			return err
		}
		// read leaves report zero when it refuses the file's contents, which is the
		// report a fresh install has and the one the next scan fills in.
		mutate(&report)
		report.Version = reportVersion

		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding the health report: %w", err)
		}
		return atomicfile.Publish(s.path, append(data, '\n'), reportFileMode)
	})
}

// Read returns the stored counters.
//
// A missing file is a zero Report and no error: a fresh install has none, and
// `doctor` renders that as "never scanned". A file this build cannot read is an
// error, for the reason errInvalidReport gives.
func (s *Store) Read() (Report, error) {
	return s.read()
}

// read is Read without the lock, so update can call it inside one. flock is per
// open file description, so a helper that took the lock itself would deadlock
// against its own caller.
//
// Reading without the lock is safe on its own: the file is published by rename, so
// a reader sees the old complete file or the new one, never half of either.
func (s *Store) read() (Report, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return Report{}, nil
	}
	if err != nil {
		return Report{}, fmt.Errorf("reading the health report: %w", err)
	}

	var report Report
	if err := json.Unmarshal(raw, &report); err != nil {
		return Report{}, errInvalidReport
	}
	if report.Version != reportVersion {
		return Report{}, errInvalidReport
	}
	return report, nil
}
