// Package activation owns explicit project consent and Wake-owned trigger setup.
package activation

import (
	"encoding/json"
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

const (
	eventsFile  = "events.ndjson"
	hookCommand = "wake ingest --quiet"
)

// Init records consent, adds Wake's trigger without replacing existing hooks,
// and imports available Claude Code history.
func Init(paths config.Paths, root, claudeDir string) (int, error) {
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
	err = installHooks(claudeDir)
	if err != nil {
		return 0, err
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
	removed, err := removeHooks(claudeDir)
	if err != nil {
		return false, err
	}
	if purge {
		if err := os.RemoveAll(paths.DataDir); err != nil {
			return false, err
		}
	}
	return removed, nil
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
		}, destination)
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
func DiscoveryScope(paths config.Paths, claudeDir, cwd string) inventory.Scope {
	repos, err := config.OpenRepos(paths)
	if err != nil {
		return inventory.Scope{ClaudeDir: claudeDir, Project: inventory.ProjectUnresolved}
	}
	return discoveryScope(repos, claudeDir, cwd)
}

func discoveryScope(repos *config.Repos, claudeDir, cwd string) inventory.Scope {
	identity, err := repos.Identify(cwd)
	if err != nil {
		return inventory.Scope{ClaudeDir: claudeDir, Project: inventory.ProjectUnresolved}
	}
	if !identity.Matched {
		return inventory.Scope{ClaudeDir: claudeDir, Project: inventory.ProjectUnconsented}
	}
	return inventory.Scope{ClaudeDir: claudeDir, Root: cwd, Project: inventory.ProjectConsented}
}

func refreshInventory(paths config.Paths, repos *config.Repos, claudeDir, root string, events *store.Store) error {
	return inventory.New(paths.PrimitivesFile).Refresh(events, inventory.ClaudeCodeInScope(discoveryScope(repos, claudeDir, root)))
}

func installHooks(claudeDir string) error {
	path := filepath.Join(claudeDir, "settings.json")
	settings := map[string]json.RawMessage{}
	if raw, readErr := os.ReadFile(path); readErr == nil {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return errors.New("cannot parse Claude Code settings")
		}
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return readErr
	}

	hooks := map[string]json.RawMessage{}
	if raw, found := settings["hooks"]; found && json.Unmarshal(raw, &hooks) != nil {
		return errors.New("cannot parse Claude Code hooks")
	}
	for _, event := range []string{"SessionStart", "SessionEnd"} {
		updated, err := appendHook(hooks[event])
		if err != nil {
			return err
		}
		hooks[event] = updated
	}
	encodedHooks, err := json.Marshal(hooks)
	if err != nil {
		return err
	}
	settings["hooks"] = encodedHooks
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func appendHook(raw json.RawMessage) (json.RawMessage, error) {
	entries := []json.RawMessage{}
	if len(raw) != 0 && json.Unmarshal(raw, &entries) != nil {
		return nil, errors.New("cannot parse Claude Code hook event")
	}
	for _, entry := range entries {
		var group struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		}
		if json.Unmarshal(entry, &group) != nil {
			return nil, errors.New("cannot parse Claude Code hook group")
		}
		if slices.ContainsFunc(group.Hooks, func(hook struct {
			Command string `json:"command"`
		}) bool {
			return hook.Command == hookCommand
		}) {
			return json.Marshal(entries)
		}
	}
	owned := json.RawMessage(`{"wake":true,"hooks":[{"type":"command","command":"` + hookCommand + `"}]}`)
	return json.Marshal(append(entries, owned))
}

func removeHooks(claudeDir string) (bool, error) {
	path := filepath.Join(claudeDir, "settings.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	settings := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &settings) != nil {
		return false, errors.New("cannot parse Claude Code settings")
	}
	hooks := map[string]json.RawMessage{}
	if rawHooks, found := settings["hooks"]; !found {
		return false, nil
	} else if json.Unmarshal(rawHooks, &hooks) != nil {
		return false, errors.New("cannot parse Claude Code hooks")
	}

	removed := false
	for event, rawGroups := range hooks {
		groups := []json.RawMessage{}
		if json.Unmarshal(rawGroups, &groups) != nil {
			return false, errors.New("cannot parse Claude Code hook event")
		}
		kept := make([]json.RawMessage, 0, len(groups))
		for _, group := range groups {
			if wakeHook(group) {
				removed = true
				continue
			}
			kept = append(kept, group)
		}
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		encoded, marshalErr := json.Marshal(kept)
		if marshalErr != nil {
			return false, marshalErr
		}
		hooks[event] = encoded
	}
	if !removed {
		return false, nil
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		encoded, marshalErr := json.Marshal(hooks)
		if marshalErr != nil {
			return false, marshalErr
		}
		settings["hooks"] = encoded
	}
	encoded, marshalErr := json.MarshalIndent(settings, "", "  ")
	if marshalErr != nil {
		return false, marshalErr
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

func wakeHook(raw json.RawMessage) bool {
	var group struct {
		Wake  bool `json:"wake"`
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if json.Unmarshal(raw, &group) != nil {
		return false
	}
	if group.Wake {
		return true
	}
	return slices.ContainsFunc(group.Hooks, func(hook struct {
		Command string `json:"command"`
	}) bool {
		return hook.Command == hookCommand
	})
}
