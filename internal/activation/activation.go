// Package activation owns explicit project consent and Wake-owned trigger setup.
package activation

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/ingest"
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
	if err := addConsentedRepo(paths, id); err != nil {
		return 0, err
	}
	if err := installHooks(claudeDir); err != nil {
		return 0, err
	}
	return ingestHistory(repos, claudeDir, store.New(filepath.Join(paths.DataDir, eventsFile)))
}

// Ingest imports available transcripts for consented repositories only.
func Ingest(paths config.Paths, claudeDir string) (int, error) {
	repos, err := config.OpenRepos(paths)
	if err != nil {
		return 0, err
	}
	return ingestHistory(repos, claudeDir, store.New(filepath.Join(paths.DataDir, eventsFile)))
}

// Rebuild discards only the derived event spool before importing consented
// history again. Project consent, repository identities, and hooks remain.
func Rebuild(paths config.Paths, claudeDir string) (int, error) {
	if err := os.Remove(filepath.Join(paths.DataDir, eventsFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	return Ingest(paths, claudeDir)
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
	owned := json.RawMessage(`{"hooks":[{"type":"command","command":"` + hookCommand + `"}]}`)
	return json.Marshal(append(entries, owned))
}
