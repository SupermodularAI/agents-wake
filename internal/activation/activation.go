// Package activation owns explicit project consent and Wake-owned trigger setup.
package activation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

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
// and imports available Claude Code history.
//
// executable is the path this process was started from, and resolving a hook
// command out of it is Init's first act — before consent is recorded, before
// config.toml is touched and before the settings file is opened. An installation
// that cannot host a hook command therefore writes nothing at all, which is
// stronger than rejecting it before modifying the settings: a consent record for a
// repository whose trigger was never installed is a repository that silently
// collects only what a manual `wake ingest` picks up.
func Init(paths config.Paths, root, claudeDir, executable string) (int, error) {
	command, err := hookCommandFor(executable)
	if err != nil {
		return 0, err
	}
	repos, err := config.OpenRepos(paths)
	if err != nil {
		return 0, err
	}
	id, err := repos.Register(root, filepath.Base(root))
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
	written, scan, err := ingestHistory(repos, claudeDir, events)
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
func Ingest(paths config.Paths, claudeDir string) (int, error) {
	repos, err := config.OpenRepos(paths)
	if err != nil {
		return 0, err
	}
	root, err := os.Getwd()
	if err != nil {
		return 0, fmt.Errorf("resolving current directory: %w", err)
	}
	events := store.New(filepath.Join(paths.DataDir, eventsFile))
	written, scan, err := ingestHistory(repos, claudeDir, events)
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
func Trigger(paths config.Paths, claudeDir string) (bool, error) {
	return lockfile.TryWithLock(filepath.Join(paths.DataDir, ingestLockName), func() error {
		_, err := Ingest(paths, claudeDir)
		return err
	})
}

// Rebuild discards only the derived event spool before importing consented
// history again. Project consent, repository identities, and hooks remain.
func Rebuild(paths config.Paths, claudeDir string) (int, error) {
	if err := os.Remove(filepath.Join(paths.DataDir, eventsFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
	if recordErr := health.New(paths.HealthFile).RecordHooks(health.Hooks{At: time.Now().UTC(), Removed: removed, KeptOwned: keptOwned}); recordErr != nil {
		return false, recordErr
	}
	if purge {
		if err := os.RemoveAll(paths.DataDir); err != nil {
			return false, err
		}
	}
	return removed > 0, nil
}

// ingestHistory imports every reachable transcript and reports both what it wrote
// and what it could not do.
//
// The counters exist because every failure on this path is deliberately swallowed:
// a directory it cannot walk and a file it cannot open are both "collects nothing"
// rather than an error that breaks the command (plan §4.3). Swallowed and
// uncounted, though, they are indistinguishable from a machine with no history —
// which is the confusion ADR-0010 asks doctor to end.
func ingestHistory(repos *config.Repos, claudeDir string, destination *store.Store) (int, health.Scan, error) {
	written := 0
	scan := health.Scan{At: time.Now().UTC(), RefusedProjects: repos.DroppedEntries()}
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
		result, ingestErr := ingest.ClaudeCode(file, func(cwd string) (record.Hash, bool) {
			identity, identifyErr := repos.Identify(cwd)
			return record.Hash(identity.ID), identifyErr == nil && identity.Matched
		}, record.NewNamer(repos.NameKey()), destination)
		if ingestErr != nil {
			scan.ParseErrors++
			return nil
		}
		scan.ParseErrors += result.Malformed
		if result.Parsed == 0 {
			// Read successfully and yielded no terminal event — most often because
			// its working directory belongs to no consented repository, sometimes
			// because every call in it is still unterminated (ADR-0015). Either way
			// it is a clean zero, not a failure, and the two must not share a counter.
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
