package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// openCodeDirName is the one place opencode's own directory name is spelled, for
// claudeCodeDirName's reason: a second spelling of it would be a second resolver.
const openCodeDirName = "opencode"

// openCodeStoreName is the file opencode keeps its history in.
const openCodeStoreName = "opencode.db"

// EnvOpenCodeConfig names the variable opencode itself relocates its config file
// with. Reading the harness's own environment adds no key to wake's config
// surface (ADR-0014), and is what keeps the answer true rather than merely
// plausible — a resolver that ignored it would report "collects zero" for a
// machine it never looked at, which is the failure doctor exists to make
// impossible.
const EnvOpenCodeConfig = "OPENCODE_CONFIG"

// EnvXDGDataHome and EnvXDGConfigHome are the base-directory variables opencode
// resolves its own store and config under. They are the harness's mechanism, read
// here for EnvOpenCodeConfig's reason and on the same terms: wake's own files are
// governed by ADR-0014 and Paths, and neither moves because of these.
const (
	EnvXDGDataHome   = "XDG_DATA_HOME"
	EnvXDGConfigHome = "XDG_CONFIG_HOME"
)

// ErrOpenCodeConfigNotAbsolute is returned when EnvOpenCodeConfig holds a relative
// path, for ErrClaudeConfigDirNotAbsolute's reason: the detached hook scan's
// working directory is arbitrary (ADR-0016), so resolving a relative path would
// read whichever directory the binary happened to be invoked from. The message
// names the variable and never its value, because the value is a path.
var ErrOpenCodeConfigNotAbsolute = errors.New(EnvOpenCodeConfig + " must be an absolute path")

// OpenCodeStore returns the path to opencode's SQLite store:
// $XDG_DATA_HOME/opencode/opencode.db, or ~/.local/share/opencode/opencode.db.
//
// It is a resolver and not a field of Paths, for ClaudeCodeDir's reason: Paths is
// where every file this tool owns lives, and a foreign harness's store in that
// struct would blur the line ADR-0010's unambiguous uninstall rests on.
//
// It creates nothing and it stats nothing: whether a store is there is the
// reader's question, and answering it here would make two commands disagree about
// what "installed" means.
func OpenCodeStore() (string, error) {
	data, err := xdgDir(EnvXDGDataHome, filepath.Join(".local", "share"))
	if err != nil {
		return "", err
	}
	return filepath.Join(data, openCodeDirName, openCodeStoreName), nil
}

// OpenCodeConfigFile returns the path to opencode's own configuration file, or ""
// when this machine has none.
//
// $OPENCODE_CONFIG wins outright — it is how opencode itself relocates the file,
// and the variable names one file rather than a directory. Otherwise the first of
// <config>/opencode/opencode.jsonc and <config>/opencode/opencode.json that
// exists, in that order: the .jsonc spelling is the documented one and the .json
// spelling is the one a machine with a strict editor ends up with.
//
// "" is a real answer and not a failure: a machine with no configuration file
// declared no MCP servers, which the caller renders as an absence rather than as
// a zero (ADR-0046).
func OpenCodeConfigFile() (string, error) {
	if relocated := strings.TrimSpace(os.Getenv(EnvOpenCodeConfig)); relocated != "" {
		if !filepath.IsAbs(relocated) {
			return "", ErrOpenCodeConfigNotAbsolute
		}
		return filepath.Clean(relocated), nil
	}
	config, err := xdgDir(EnvXDGConfigHome, ".config")
	if err != nil {
		return "", err
	}
	for _, name := range []string{"opencode.jsonc", "opencode.json"} {
		candidate := filepath.Join(config, openCodeDirName, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", nil
}

// xdgDir resolves one XDG base directory: the variable when it holds an absolute
// path, else the home-relative fallback. A relative value is ignored rather than
// refused, because that is what the base-directory specification itself says to
// do with one — unlike EnvOpenCodeConfig, which is opencode's own override and
// whose relative value would silently point at the wrong file.
//
// The error names the home directory as a concept and never its value, matching
// ClaudeCodeDir's wrap (plan §4.2).
func xdgDir(variable, fallback string) (string, error) {
	if set := strings.TrimSpace(os.Getenv(variable)); filepath.IsAbs(set) {
		return filepath.Clean(set), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving the home directory: %w", err)
	}
	return filepath.Join(home, fallback), nil
}
