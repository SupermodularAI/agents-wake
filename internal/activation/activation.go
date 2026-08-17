// Package activation owns explicit project consent and Wake-owned trigger setup.
package activation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/ingest"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

const eventsFile = "events.ndjson"

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
	err = addConsentedRepo(paths, id)
	if err != nil {
		return 0, err
	}
	if _, installErr := installHooks(paths, claudeDir, command); installErr != nil {
		return 0, installErr
	}
	events := store.New(filepath.Join(paths.DataDir, eventsFile))
	written, err := ingestHistory(repos, claudeDir, events)
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
	written, err := ingestHistory(repos, claudeDir, events)
	if err != nil {
		return written, err
	}
	return written, refreshInventory(paths, repos, claudeDir, root, events)
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
	removed, _, err := removeHooks(paths, claudeDir)
	if err != nil {
		return false, err
	}
	if purge {
		if err := os.RemoveAll(paths.DataDir); err != nil {
			return false, err
		}
	}
	return removed > 0, nil
}

func addConsentedRepo(paths config.Paths, id string) error {
	current, err := config.Load(paths)
	if err != nil {
		return err
	}
	repos, err := current.StringList("scan.repos")
	if err != nil {
		return err
	}
	if slices.Contains(repos, id) {
		return nil
	}
	_, err = config.Set(paths, "scan.repos", strings.Join(append(repos, id), ","))
	return err
}

func ingestHistory(repos *config.Repos, claudeDir string, destination *store.Store) (int, error) {
	written := 0
	err := filepath.WalkDir(filepath.Join(claudeDir, "projects"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		defer file.Close()
		result, ingestErr := ingest.ClaudeCode(file, func(cwd string) (record.Hash, bool) {
			identity, identifyErr := repos.Identify(cwd)
			return record.Hash(identity.ID), identifyErr == nil && identity.Matched
		}, record.NewNamer(repos.NameKey()), destination)
		if ingestErr == nil {
			written += result.Written
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return written, nil
	}
	return written, err
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
