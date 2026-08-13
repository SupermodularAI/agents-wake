package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AppName is the directory name under each root, and the name of the binary it
// belongs to (AGENTS.md § Apps & targets). It deliberately differs from the
// module name, so it is spelled once here rather than derived from either.
const AppName = "wake"

// EnvDataDir names the one environment variable this package honours. It
// overrides the data root only — never the config root, and therefore never
// config.toml or the repo-id salt (ADR-0014, ADR-0019 §3): the salt is what makes
// a repo id one-way, so a variable that moved it would silently re-identify
// every repo on the next run.
//
// This is the whole override surface. XDG_CONFIG_HOME, XDG_STATE_HOME and
// XDG_DATA_HOME are deliberately not read: no decision grants a second variable,
// and each one added is another way for the salt to move.
const EnvDataDir = "WAKE_DIR"

// ErrDataDirNotAbsolute is returned when EnvDataDir is set to a relative path.
// The data root is a documented public integration surface (ADR-0017), so
// resolving it against the process working directory would make the store's
// location depend on where the binary happened to be invoked from. The message
// names the variable and not its value, because the value is a path.
var ErrDataDirNotAbsolute = errors.New(EnvDataDir + " must be an absolute path")

// The file names inside the two roots. They live together because ResolvePaths
// is the one place that composes them, and because every read or write of
// repo-salt and projects.json stays inside this package (ADR-0019 consequences);
// a name spelled in another package would be the first crack in that.
const (
	configFileName   = "config.toml"
	saltFileName     = "repo-salt"
	projectsFileName = "projects.json"
	// projectsLockName is what a writer of projects.json holds while it reads,
	// merges and republishes the table. It is a lock and nothing else: always
	// empty, safe to delete, and it carries no path, label or id — so it is not
	// part of Paths, which is the surface other packages see.
	projectsLockName = "projects.lock"
)

// Paths is where every file this tool owns lives. One resolver owns the layout so
// that the config root and the data root can never drift apart: ADR-0010 rests on
// uninstall being able to remove one and keep the other unambiguously.
//
// The fields are the tool's own files, not any observed repository's — nothing
// here is a path read out of projects.json.
type Paths struct {
	// ConfigDir is ~/.config/wake — user-owned configuration, kept when the
	// data root is deleted.
	ConfigDir string
	// DataDir is ~/.local/state/wake, or EnvDataDir when it is set. The store
	// is not precious: it is safe to delete, which is why the salt is not here
	// (ADR-0015).
	DataDir string
	// ConfigFile is the TOML file the user edits. It may not exist; a missing
	// file means defaults, not an error.
	ConfigFile string
	// SaltFile holds the 32 random bytes the repo id is keyed with. Under the
	// config root, so deleting the data root cannot re-identify repos.
	SaltFile string
	// ProjectsFile is the local resolution table: hashed id, consented root and
	// label. It never travels (plan §3.4).
	ProjectsFile string
}

// ResolvePaths resolves the layout from the home directory and EnvDataDir, and
// reads nothing else from the environment.
//
// It creates nothing. Resolving where a file belongs is separate from creating
// it, so that a read of a missing config file can yield defaults without leaving
// a directory behind (acceptance item 2).
func ResolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolving the home directory: %w", err)
	}

	configDir := filepath.Join(home, ".config", AppName)
	dataDir := filepath.Join(home, ".local", "state", AppName)

	// Surrounding whitespace is trimmed before the emptiness check and then
	// kept trimmed: an unset variable and one set to "" or a stray newline (as
	// `export WAKE_DIR=$(cat somefile)` produces) mean the same thing, and a
	// directory whose name is a space is not a case worth honouring over that.
	if override := strings.TrimSpace(os.Getenv(EnvDataDir)); override != "" {
		if !filepath.IsAbs(override) {
			return Paths{}, ErrDataDirNotAbsolute
		}
		dataDir = filepath.Clean(override)
	}

	return Paths{
		ConfigDir:    configDir,
		DataDir:      dataDir,
		ConfigFile:   filepath.Join(configDir, configFileName),
		SaltFile:     filepath.Join(configDir, saltFileName),
		ProjectsFile: filepath.Join(dataDir, projectsFileName),
	}, nil
}
