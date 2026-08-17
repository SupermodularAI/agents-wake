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
	configFileName     = "config.toml"
	saltFileName       = "repo-salt"
	projectsFileName   = "projects.json"
	primitivesFileName = "primitives.json"
	healthFileName     = "health.json"
	// The three lock names. Each is what a writer of one state file holds while it
	// reads, merges and republishes that file, and they are three distinct files on
	// purpose: a writer of the project table must never wait on a writer of
	// config.toml, or a hook-triggered scan and a `wake init` in another repository
	// would serialise against each other for no reason.
	//
	// None of them is a field of Paths. They are locks and nothing else: always
	// empty, safe to delete, and carrying no path, label or id — whereas Paths is
	// the surface other packages see and the list `init` discloses.
	//
	// claudeSettingsLockName guards ~/.claude/settings.json even though it lives
	// here rather than beside that file. Wake can only serialise Wake's own writers
	// either way, and dropping a lock file into the harness's own directory would
	// add a file `init` would then have to disclose under ADR-0010.
	projectsLockName       = "projects.lock"
	configLockName         = "config.lock"
	claudeSettingsLockName = "claude-settings.lock"
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
	// PrimitivesFile is the derived inventory of locally available primitives and
	// their aggregate activity. It contains no paths or configuration content.
	PrimitivesFile string
	// HealthFile holds what the last scan and the last hook change managed to do,
	// as counts and timestamps only — no path, no label, no line of any
	// transcript. It is what makes "collects nothing" distinguishable from
	// "collects zero" after a hook-invoked scan that is required to exit in
	// silence (ADR-0010, ADR-0016).
	//
	// Under the data root, because it is derived and non-precious: deleting the
	// data root has to stay safe (ADR-0014), and losing these counters costs one
	// scan's worth of diagnostics and nothing else.
	HealthFile string
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
		ConfigDir:      configDir,
		DataDir:        dataDir,
		ConfigFile:     filepath.Join(configDir, configFileName),
		SaltFile:       filepath.Join(configDir, saltFileName),
		ProjectsFile:   filepath.Join(dataDir, projectsFileName),
		PrimitivesFile: filepath.Join(dataDir, primitivesFileName),
		HealthFile:     filepath.Join(dataDir, healthFileName),
	}, nil
}
