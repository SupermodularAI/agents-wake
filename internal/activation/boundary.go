package activation

import (
	"errors"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// boundaryDiscovery collects the working directories one walk saw that the recorded
// global root encloses and no recorded entry matched.
//
// Observing is not registering (ADR-0032 §5): the set is collected on the derivation
// path and acted on only after the walk has finished. Registering inside the resolver
// would judge two events of one scan against two different tables — the first event of
// a repository against a table without it and the second against a table with it — and
// ADR-0019 §1 exists to make that impossible.
//
// A nil *boundaryDiscovery is a working collector that observes nothing, which is what
// the second walk is handed: whatever it sees, there is no third walk to act on it.
type boundaryDiscovery struct {
	repos *config.Repos
	seen  map[string]bool
	order []string
}

func newBoundaryDiscovery(repos *config.Repos) *boundaryDiscovery {
	return &boundaryDiscovery{repos: repos, seen: map[string]bool{}}
}

// observe records cwd when the recorded boundary strictly encloses it.
//
// With no boundary recorded WithinGlobalRoot is always false, so the set stays empty
// and there is exactly one walk in the common case. The check is a pure string
// operation over the snapshot — no stat, no git — so adding it to the derivation path
// costs the resolver nothing it was not already allowed to spend (ADR-0019 §1).
func (d *boundaryDiscovery) observe(cwd string) {
	if d == nil || d.seen[cwd] {
		return
	}
	if !d.repos.WithinGlobalRoot(cwd) {
		return
	}
	d.seen[cwd] = true
	d.order = append(d.order, cwd)
}

// pending returns what the walk observed, in the order it was first seen, so one scan
// registers the same directories in the same order however the walk is interleaved.
func (d *boundaryDiscovery) pending() []string {
	if d == nil {
		return nil
	}
	return d.order
}

// registerDiscovered registers each observed directory under the boundary and reports
// how the batch went.
//
// from is the caller's for the whole batch, so one scan writes one instant: two
// repositories discovered by the same walk must not disagree about when they were
// consented.
//
// Every failure is soft and counted. "Could not read means collects nothing, never an
// error that breaks a command" (plan §4.3) — a scan that stopped because one
// discovered directory could not be registered would lose the rest of the batch and
// the events it had already read. Counted rather than swallowed, because a silent
// refusal is indistinguishable from a machine with nothing to discover.
func registerDiscovered(repos *config.Repos, dirs []string, from time.Time) (registered, gone, refused int) {
	for _, dir := range dirs {
		_, err := repos.RegisterUnderGlobalRoot(dir, from)
		switch {
		case err == nil:
			registered++
		case errors.Is(err, config.ErrDiscoveredDirectoryGone):
			// An honest zero: there is nothing left there to read, so nothing was lost
			// by not registering it.
			gone++
		default:
			// A NestedRootError, a boundary that moved out from under the directory, an
			// entry this build could not read back. The sessions were readable and no
			// number carries them, which is why the counter joins doctor's "collects
			// nothing" arm.
			refused++
		}
	}
	return registered, gone, refused
}

// scanWithBoundary is the scan every entry point runs: walk, register what the
// boundary discovered, and walk again only when something became collectable.
//
// The second walk is what makes the boundary work on the scan that discovered the
// repository rather than on the next one. It is safe to repeat because every id is
// derived from the source event (ADR-0004), so the spool deduplicates and the re-read
// writes only what the newly recorded identities made collectable.
//
// That is a user-asked scan's story. On the hook-fired path the newly registered
// entries collect forward only from the instant just taken, and every event this walk
// can reach was in the file before it, so the re-read writes nothing and the pass buys
// nothing but the counters it replaces. It still runs, for one scan per newly
// discovered repository, rather than being gated on the scope: the sequencing is one
// rule for every entry point, and a walk that is a no-op for a real reason is easier to
// keep right than a walk that happens on one path and not another.
//
// Its counters replace the first walk's, because a walk is a complete pass over the
// source and its numbers describe that source — a partial merge would report two
// different windows as one. EventsWritten is the exception, and the only one: it is
// not a property of the source but of the work done, so the two walks' contributions
// are summed. Taking the second walk's alone would report zero events written for a
// scan that wrote plenty on the first, and doctor would say "collects zero".
// The two stale-spool counters are set here rather than inside either walk, for the
// reason the paragraph above gives: a walk's counters describe the source it read, and
// these describe the store it wrote into. Setting them at the one return point is also
// what keeps them from being dropped when the second walk's counters replace the
// first's.
func scanWithBoundary(paths config.Paths, repos *config.Repos, claudeDir string, events *store.Store, stale claudecode.Staleness, idle claudecode.Idleness, scope collectionScope) (int, health.Scan, error) {
	found, rebuilt, err := rebuildStaleSpool(events, scope)
	if err != nil {
		// A spool this build cannot read and could not replace. At is stamped so the
		// failed scan is reported as a scan that read nothing rather than as one that
		// never ran; the error itself is what the caller surfaces.
		return 0, health.Scan{At: time.Now().UTC()}, err
	}
	written, scan, err := scanBoundaryWalks(paths, repos, claudeDir, events, stale, idle, scope)
	scan.StaleRecords, scan.StaleRebuilt = found, rebuilt
	return written, scan, err
}

// rebuildStaleSpool reports how many records the spool holds from a record schema
// version this build does not read, and discards them — for the scan that can put them
// back, and only for that scan.
//
// This is the rebuild half of ADR-0015's "rebuild rather than migration". The drop
// half — record.Validate refusing a foreign version — is already true everywhere the
// spool is read, and on its own it is silent: store.Entries skips those lines the way
// it skips any line that does not decode, so every consumer reports the post-upgrade
// subset as the whole truth and every position over the spool shifts under the
// delivery watermark. Discarding is what turns that into one visible rebuild, and it is
// safe for the reason a rebuild always is: event ids are derived from the source event
// (ADR-0004), so re-deriving writes the same records back.
//
// "Re-deriving writes the same records back" is a claim about scope, which is why the
// discard is gated on it. ADR-0015's rescan is a rescan of the whole history, and the
// spool holds whatever the user backfilled with `wake ingest` — events from before each
// repository's recorded boundary. The hook-fired scan collects inside that boundary,
// because it is the one scan nobody asked for (ADR-0025), so discarding under it would
// delete history and re-derive a subset: a `wake ingest` backfill destroyed by a hook
// nobody typed, in a process ADR-0016 keeps silent. Widening that scan to the whole
// history instead is the same ADR's other prohibition — it would import the history a
// plain `init` declined. So the hook-fired scan leaves the spool exactly as it found it
// and reports the count; the drop and the rescan stay one operation, the pairing
// Rebuild has always had, and happen once on the next scan the user asks for.
//
// Leaving it means one scan does append onto a spool that still holds unreadable lines.
// On disk that costs those lines' bytes until the rebuild and nothing else: they decode
// for no consumer, contribute no id to the append index, and are dropped whole by the
// rebuild that follows. What it does cost is that store.Entries' numbering is not
// settled until the rebuild — the unreadable lines take their positions back when they
// become readable again — so a cursor over the spool must not record a position it took
// meanwhile. Remote delivery is the only such cursor and holds its watermark back for
// exactly as long as store.Stale reports lines it cannot read, which costs a re-send and
// never a record. It is the conservative half of the trade — forward collection keeps
// running, and nothing on disk is destroyed by a scan that could not put it back.
//
// The discard runs before the walk, not after, so a scan that does rebuild never
// appends onto a spool of mixed versions. A spool this build can read is left
// byte-identical by either scope.
//
// The read and the drop are two operations, so a concurrent scan can append between
// them and have its records dropped with the rest. That costs a re-derivation and
// never a record: both scans read the same harness history, and every id in it is
// the same id (ADR-0004). Holding the spool lock across the whole scan instead would
// deadlock against that scan's own Append, which is the reason Rebuild does not
// either.
func rebuildStaleSpool(events *store.Store, scope collectionScope) (found int, rebuilt bool, err error) {
	found, err = events.Stale()
	if err != nil || found == 0 || scope != wholeHistory {
		return found, false, err
	}
	if err := events.Discard(); err != nil {
		return found, false, err
	}
	return found, true, nil
}

func scanBoundaryWalks(paths config.Paths, repos *config.Repos, claudeDir string, events *store.Store, stale claudecode.Staleness, idle claudecode.Idleness, scope collectionScope) (int, health.Scan, error) {
	discovery := newBoundaryDiscovery(repos)
	written, scan, err := importHistory(repos, claudeDir, events, stale, idle, scope, discovery)
	if err != nil {
		return written, scan, err
	}
	pending := discovery.pending()
	if len(pending) == 0 {
		return written, scan, nil
	}

	// One instant for the batch, taken here rather than per registration.
	registered, gone, refused := registerDiscovered(repos, pending, time.Now().UTC())
	scan.BoundarySkipped, scan.BoundaryRefused = gone, refused
	if registered == 0 {
		return written, scan, nil
	}

	// The reopen ADR-0032 §5 names. The registrations above are already in this
	// *Repos, but re-reading is what picks up whatever another writer recorded in the
	// meantime, and it is the same snapshot rule the first walk ran under: one
	// resolver per walk, closed over one table.
	reopened, err := config.OpenRepos(paths)
	if err != nil {
		return written, scan, err
	}
	// nil collector: whatever this walk observes, there is no third walk to act on it,
	// and a directory it discovers is one the next scan picks up.
	second, secondScan, err := importHistory(reopened, claudeDir, events, stale, idle, scope, nil)
	if err != nil {
		return written + second, scan, err
	}
	secondScan.EventsWritten += scan.EventsWritten
	secondScan.BoundarySkipped, secondScan.BoundaryRefused = gone, refused
	return written + second, secondScan, nil
}
