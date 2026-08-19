// Package activation owns explicit project consent and Wake-owned trigger setup.
package activation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
	"github.com/SupermodularAI/agents-wake/internal/ingest"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/lockfile"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

const eventsFile = "events.ndjson"

// ingestLockName is the single-flight lock for the hook-invoked scan. It is a
// different file from the spool's own append lock, and the two answer different
// questions: this one keeps two scans from both running, the other keeps two appends
// from interleaving. A scan that finds this held has nothing to add by repeating
// what the holder is already doing (ADR-0016).
const ingestLockName = "ingest.lock"

// Init records consent, adds Wake's trigger without replacing existing hooks,
// and imports available Claude Code history only when full is set.
//
// Every refusal that can be decided from the arguments alone is raised first —
// before consent is recorded, before config.toml is touched and before the settings
// file is opened. An installation that cannot host Wake's trigger therefore writes
// nothing at all, which is stronger than rejecting it before modifying the
// settings: a consent record for a repository whose trigger was never installed is
// a repository that silently collects only what a manual `wake ingest` picks up.
//
// Two things are pre-checkable, and both are checked here. executable is the path
// this process was started from, and a hook command is resolved out of it.
// claudeDir holds the settings file, and every shape this build refuses to edit —
// a non-regular file, a link resolving to nothing, and each of the document shapes
// its decoding rejects — is decided by claudeDir alone, so leaving any of them to
// be raised from inside installHooks would raise it after Register and AddToList
// had already written.
//
// full gates the historical import (ADR-0024): the same root is consented, the same
// refusals are raised first, the same trigger is written, and the same disclosure is
// owed either way. Without it, collection starts forward from this call — and that is
// recorded rather than merely intended, because the trigger this call installs runs in
// a process that was never told (ADR-0025, and see Trigger). The history is still there
// to be had whenever the user asks for it: `wake ingest` or a later `init --full`
// recovers exactly the same records, since every id is derived from the source event
// (ADR-0004) and the cursor is an optimisation rather than a record of what has been
// seen (ADR-0015).
//
// No scan record is written on the default path: RecordScan replaces the scan counters
// wholesale, so a zero-valued health.Scan would report "collects zero" for a state
// nobody measured and erase what an earlier import did find — the distinction ADR-0010
// asks doctor to keep.
func Init(paths config.Paths, root, claudeDir, executable string, full bool) (int, error) {
	command, err := hookCommandFor(executable)
	if err != nil {
		return 0, err
	}
	if settingsErr := checkSettingsShape(claudeDir); settingsErr != nil {
		return 0, settingsErr
	}
	repos, err := config.OpenRepos(paths)
	if err != nil {
		return 0, err
	}
	// The boundary is recorded at registration, before a single event is read, and it
	// is the only durable trace of what this call promised: without it the trigger this
	// command installs would walk the whole history at the next session start and import
	// exactly what the disclosure said it would not (ADR-0024, ADR-0025). Under --full
	// there is no boundary to record — the user asked for everything — and registering
	// with none also clears one an earlier plain init left.
	boundary := time.Now().UTC()
	if full {
		boundary = time.Time{}
	}
	id, err := repos.Register(root, filepath.Base(root), boundary)
	if err != nil {
		return 0, err
	}
	if _, listErr := config.AddToList(paths, "scan.repos", id); listErr != nil {
		return 0, listErr
	}
	installed, err := installHooks(paths, claudeDir, command)
	if err != nil {
		return 0, err
	}
	counters := health.New(paths.HealthFile)
	if recordErr := counters.RecordHooks(health.Hooks{At: time.Now().UTC(), Installed: installed}); recordErr != nil {
		return 0, recordErr
	}

	events := store.New(filepath.Join(paths.DataDir, eventsFile))
	if !full {
		// No walk, and deliberately no scan record either. RecordScan replaces the
		// scan counters wholesale (internal/health), so a zero-valued health.Scan
		// would render "collects zero" for a state nobody measured — the distinction
		// ADR-0010 asks doctor to keep — and would erase what an earlier --full or
		// `wake ingest` actually found.
		//
		// Not calling the import is only half of forward-only; the other half is the
		// boundary recorded above, which is what the trigger's own scan honours. The
		// boundary is not a cursor and does not stand in for one: it says what the user
		// consented to, never what has been seen, so re-scanning stays safe for the
		// reason it always was (ADR-0004, ADR-0015).
		return 0, refreshInventory(paths, repos, claudeDir, root, events)
	}
	written, scan, err := importHistory(repos, claudeDir, events, staleness(paths), wholeHistory)
	// The counters are recorded whether the scan succeeded or not: a partial
	// activation — hooks written, history import failed — is reported through
	// doctor rather than repaired silently, and the counters are the report.
	if recordErr := counters.RecordScan(scan); recordErr != nil && err == nil {
		err = recordErr
	}
	if err != nil {
		return written, err
	}
	return written, refreshInventory(paths, repos, claudeDir, root, events)
}

// Ingest imports available transcripts for consented repositories only.
//
// It is the form the user asks for — `wake ingest`, `wake ingest --rebuild`, and the
// import inside `wake init --full` — so it imports everything the harness holds for a
// consented repository, including what predates the repository's registration. That
// is the backfill route a forward-only `init` names in its own output, and it is why
// `init --full` and a plain `init` followed by `wake ingest` leave byte-identical
// stores.
//
// ADR-0024's forward-only default is about what happens when nobody asked, which is
// Trigger's scan and not this one.
func Ingest(paths config.Paths, claudeDir string) (int, error) {
	return ingestScoped(paths, claudeDir, wholeHistory)
}

// ingestScoped is Ingest with the scope decision left to the caller, so the one
// difference between a scan the user asked for and a scan a hook fired is the scope
// and nothing else: same walk, same counters, same inventory refresh.
func ingestScoped(paths config.Paths, claudeDir string, scope collectionScope) (int, error) {
	repos, err := config.OpenRepos(paths)
	if err != nil {
		return 0, err
	}
	root, err := os.Getwd()
	if err != nil {
		return 0, fmt.Errorf("resolving current directory: %w", err)
	}
	events := store.New(filepath.Join(paths.DataDir, eventsFile))
	written, scan, err := importHistory(repos, claudeDir, events, staleness(paths), scope)
	if recordErr := health.New(paths.HealthFile).RecordScan(scan); recordErr != nil && err == nil {
		err = recordErr
	}
	if err != nil {
		return written, err
	}
	return written, refreshInventory(paths, repos, claudeDir, root, events)
}

// Trigger is the scan the Claude Code hook causes, and it is single-flight: a
// trigger that finds another one already scanning skips its own scan and reports
// that it did nothing.
//
// Skipping rather than queueing is the point (ADR-0016: concurrent session-ends
// must not "each run a full independent scan"). It is safe because every id is
// derived from the source event, so re-scanning the same history writes nothing
// twice (ADR-0004), and because the cursor is an optimisation rather than a record
// of what has been seen (ADR-0015) — which is what lets ADR-0016 say a trigger "can
// be arbitrarily unreliable without ever producing a wrong number". Whatever this
// run skips, the next SessionStart picks up.
//
// It is also the one scan nobody asked for, so it is the one that honours each
// repository's recorded collection boundary (ADR-0024, ADR-0025). A repository
// consented by a plain `wake init` was promised that its existing history would not be
// imported; this scan runs from the hook that same command installed, in a process
// that was never told, and importing the history here would undo the promise one
// session later without anyone typing anything. An explicit `wake ingest` is the user
// asking, and imports everything.
func Trigger(paths config.Paths, claudeDir string) (bool, error) {
	return lockfile.TryWithLock(filepath.Join(paths.DataDir, ingestLockName), func() error {
		_, err := ingestScoped(paths, claudeDir, consentedWindow)
		return err
	})
}

// Rebuild discards only the derived event spool before importing consented
// history again. Project consent, repository identities, and hooks remain.
//
// The spool is dropped through the store rather than removed here, because the
// removal has to be exclusive with a concurrent append: a hook-triggered scan that
// holds the spool's descriptor open goes on writing into an inode this function
// unlinked, and those bytes reach no reader (T004's bar: the store is readable while
// being written). The store owns that lock, so the drop is its operation.
//
// It waits rather than refusing: an append's critical section is one buffered write,
// and blocking keeps `wake ingest --rebuild` free of a failure mode a user would have
// to retry past. It takes no other lock — not ingest.lock, which only Trigger holds
// and whose non-blocking single-flight must keep letting a hook child skip and exit
// 0 in silence (ADR-0016). Trigger takes ingest.lock before the spool lock and this
// takes only the spool lock, so the two orders cannot cycle. The lock is released
// before Ingest runs; holding it across the re-ingest would deadlock against that
// ingest's own Append.
func Rebuild(paths config.Paths, claudeDir string) (int, error) {
	// The spool is dropped first, so a lock failure returns before the primitives
	// snapshot is removed — never a half-dropped state.
	if err := store.New(filepath.Join(paths.DataDir, eventsFile)).Discard(); err != nil {
		return 0, err
	}
	if err := os.Remove(paths.PrimitivesFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	return Ingest(paths, claudeDir)
}

// Uninstall removes only Wake's Claude Code hooks. It deliberately keeps local
// data unless purge is requested, so removing automation never destroys history.
func Uninstall(paths config.Paths, claudeDir string, purge bool) (bool, error) {
	removed, keptOwned, err := removeHooks(paths, claudeDir)
	if err != nil {
		return false, err
	}
	// removed > 0 alongside the error, not false: the hooks are already gone by the
	// time the counters are written, and a diagnostics write that fails for a reason
	// of its own — an unwritable data root — must not report that nothing happened.
	// The error still surfaces; what it must not do is contradict the work.
	if recordErr := health.New(paths.HealthFile).RecordHooks(health.Hooks{At: time.Now().UTC(), Removed: removed, KeptOwned: keptOwned}); recordErr != nil {
		return removed > 0, recordErr
	}
	if purge {
		if err := os.RemoveAll(paths.DataDir); err != nil {
			return false, err
		}
	}
	return removed > 0, nil
}

// staleness resolves ADR-0015's staleness rule for one scan: the configured
// threshold, and the instant to compare last activity against.
//
// scan.stale_call_timeout is the only threshold read, and it governs both the
// interrupted emission and the session-close determination the reader shares with
// its other caller — ADR-0023 §3: "no second threshold is introduced".
// session.idle_timeout is a different tunable (ADR-0014's session-end inference,
// for the session grain's ended_at) and is deliberately not read here.
//
// A config file this build cannot read, or a value it cannot use, leaves the rule
// disabled rather than guessing a threshold: emitting interrupted too early is
// permanent (ADR-0015 rejects upsert, ADR-0004 deduplicates), so a scan that cannot
// read its own threshold buffers instead. The value is uncalibrated and provisional
// (ADR-0014), which is why nothing derived from it is reported as a duration.
func staleness(paths config.Paths) claudecode.Staleness {
	settings, err := config.Load(paths)
	if err != nil {
		return claudecode.Staleness{}
	}
	timeout, usable, err := settings.Duration("scan.stale_call_timeout")
	if err != nil || !usable {
		return claudecode.Staleness{}
	}
	return claudecode.Staleness{Timeout: timeout, Now: time.Now().UTC()}
}

// collectionScope decides which of a consented repository's events one scan may
// import.
//
// The two exist because ADR-0024's forward-only default has to hold for the scan the
// hook fires, not only for `init` itself. Which of them a scan uses is decided by who
// asked for it, never by what it finds: a user typing `wake ingest` has asked for the
// history, and a trigger has asked for nothing.
type collectionScope int

const (
	// consentedWindow honours each repository's recorded boundary: a repository
	// consented by a plain `wake init` collects forward from that instant only. The
	// scan still walks and still counts everything it reads — the boundary excludes
	// events, it does not narrow the walk — so doctor's numbers stay about the source
	// rather than about the filter.
	consentedWindow collectionScope = iota
	// wholeHistory imports every event the harness holds for a consented repository,
	// which is what `wake init --full`, `wake ingest` and `wake ingest --rebuild` are.
	wholeHistory
)

// resolverFor builds the consent answer one scan resolves every event against
// (ADR-0010, ADR-0024): which repository the event's working directory belongs to,
// and whether an event at that instant is one the repository consented to collect.
//
// One resolver per scan, closed over one *Repos snapshot, so every event in a scan is
// judged against the same table and the same boundaries (ADR-0019 §1). The id is
// returned even when the answer is no: the reader ignores it, and computing it
// unconditionally keeps this function a pure question about consent.
func resolverFor(repos *config.Repos, scope collectionScope) claudecode.Resolver {
	return func(cwd string, at time.Time) (record.Hash, bool) {
		identity, err := repos.Identify(cwd)
		if err != nil || !identity.Matched {
			return record.Hash(identity.ID), false
		}
		if scope == wholeHistory {
			return record.Hash(identity.ID), true
		}
		// An event exactly at the boundary is inside it: the boundary is the instant
		// collection began, not the instant after.
		from := repos.CollectsFrom(identity.ID)
		return record.Hash(identity.ID), !at.Before(from)
	}
}

// importHistory is ingestHistory behind a variable so a test can assert that a
// plain `wake init` never enters it. An empty result is not that assertion — a walk
// that found nothing and a walk that never ran produce the same number — and
// ADR-0024's decision is about the walk, not about the count. Same device, and the
// same reason, as internal/cli's hookChild.
var importHistory = ingestHistory

// ingestHistory imports every reachable transcript and reports both what it wrote
// and what it could not do.
//
// The counters exist because every failure on this path is deliberately swallowed:
// a directory it cannot walk and a file it cannot open are both "collects nothing"
// rather than an error that breaks the command (plan §4.3). Swallowed and
// uncounted, though, they are indistinguishable from a machine with no history —
// which is the confusion ADR-0010 asks doctor to end.
func ingestHistory(repos *config.Repos, claudeDir string, destination *store.Store, stale claudecode.Staleness, scope collectionScope) (int, health.Scan, error) {
	written := 0
	scan := health.Scan{At: time.Now().UTC(), RefusedProjects: repos.DroppedEntries()}
	resolve := resolverFor(repos, scope)
	err := filepath.WalkDir(filepath.Join(claudeDir, "projects"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// "Not there" arrives by this same route as "could not be read":
			// filepath.WalkDir reports even the root's own stat error through the
			// callback. A machine with no Claude Code history is a clean zero, so
			// only the errors that are not absence count as unreadable — otherwise
			// every fresh install would report a source it failed to read.
			if !errors.Is(walkErr, fs.ErrNotExist) {
				scan.Unreadable++
			}
			return nil
		}
		if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		scan.Transcripts++

		file, openErr := os.Open(path)
		if openErr != nil {
			scan.Unreadable++
			return nil
		}
		defer file.Close()
		result, ingestErr := ingest.ClaudeCode(file, resolve, record.NewNamer(repos.NameKey()), stale, destination)
		if ingestErr != nil {
			scan.ParseErrors++
			return nil
		}
		scan.ParseErrors += result.Malformed
		// A call the reader could not name is collection that was lost: the primitive
		// was invoked, the line was perfectly readable, and no number carries it.
		// Counted here so doctor can say so — a harness renaming the field a
		// primitive's identity lives in would otherwise stop collection in silence
		// while doctor still reported "collecting" (plan §3.3, §12).
		scan.RefusedCalls += result.Refused
		// Two different facts, deliberately two counters. Pending is a call whose
		// session may still be running — transient, and not a fault. Interrupted is a
		// call whose session went quiet past the threshold, so the invocation is now
		// in the store carrying the outcome that says it never finished (ADR-0015).
		// Neither is lost collection, so neither joins doctor's "collects nothing" arm.
		scan.PendingCalls += result.Pending
		scan.InterruptedCalls += result.Interrupted
		if result.Parsed == 0 && result.Refused == 0 {
			// Read successfully and yielded no terminal event — most often because
			// its working directory belongs to no consented repository, sometimes
			// because every call in it is still unterminated and not yet stale
			// (ADR-0015) — a transcript whose stale calls did resolve has parsed
			// records and is not skipped. Either way it is a clean zero, not a
			// failure, and the two must not share a counter. A transcript whose every
			// call was refused is deliberately not one of those: doctor reports
			// Skipped as an honest zero, and that transcript is the opposite of one.
			scan.Skipped++
		}
		scan.EventsWritten += result.Written
		written += result.Written
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		// No projects directory at all: nothing was there to read, which is a clean
		// zero rather than something unreadable.
		return written, scan, nil
	}
	return written, scan, err
}

// DiscoveryScope resolves which Claude Code discovery paths cwd may read.
//
// Consent is the boundary wake init established (ADR-0010): this resolves it and
// nothing else — it never registers a root, never invokes git, and never stats
// the filesystem to decide (ADR-0019 §1, §9). It fails closed: a consent answer
// that cannot be produced withholds project-local discovery rather than
// defaulting to it, and the unconsented fallback id Identify returns is never
// read, so it cannot be persisted (ADR-0019 §9).
// It returns the name key alongside the scope because both come from the same
// consent boundary and discovery needs both (ADR-0020). A scope that could not be
// resolved answers with the zero Namer, which refuses to digest anything: an error
// path must not widen what gets persisted.
func DiscoveryScope(paths config.Paths, claudeDir, cwd string) (inventory.Scope, record.Namer) {
	repos, err := config.OpenRepos(paths)
	if err != nil {
		return inventory.Scope{ClaudeDir: claudeDir, Project: inventory.ProjectUnresolved}, record.Namer{}
	}
	return discoveryScope(repos, claudeDir, cwd)
}

func discoveryScope(repos *config.Repos, claudeDir, cwd string) (inventory.Scope, record.Namer) {
	names := record.NewNamer(repos.NameKey())
	// The consented root, not cwd: consent was given for a repository, and a
	// command run in a subdirectory of one must not scope discovery to that
	// subdirectory — it would collect part of the repository's primitives and then
	// report a complete pass over them (ADR-0019 §1).
	root, err := repos.ConsentedRoot(cwd)
	if err != nil {
		return inventory.Scope{ClaudeDir: claudeDir, Project: inventory.ProjectUnresolved}, names
	}
	if root == "" {
		return inventory.Scope{ClaudeDir: claudeDir, Project: inventory.ProjectUnconsented}, names
	}
	return inventory.Scope{ClaudeDir: claudeDir, Root: root, Project: inventory.ProjectConsented}, names
}

func refreshInventory(paths config.Paths, repos *config.Repos, claudeDir, root string, events *store.Store) error {
	scope, names := discoveryScope(repos, claudeDir, root)
	return inventory.New(paths.PrimitivesFile).Refresh(events, inventory.ClaudeCodeInScope(scope, names))
}
