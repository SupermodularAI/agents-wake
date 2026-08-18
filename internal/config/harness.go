package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// claudeCodeDirName is the one place the harness's own directory name is spelled.
//
// It is the *default* name, not the only possible one: EnvClaudeConfigDir below
// replaces the whole home-relative directory when the user has set it. Unexported
// because a second spelling of it is a second resolver, which is exactly what
// having one function for this is meant to make impossible.
const claudeCodeDirName = ".claude"

// EnvClaudeConfigDir names the variable Claude Code itself relocates its directory
// with. Claude Code resolves its own user-scope root as CLAUDE_CONFIG_DIR or, when
// that is unset, ~/.claude — and everything wake reads moves with it: settings.json
// (the hooks), projects/*.jsonl (the transcripts) and the globally installed
// primitives.
//
// This is not a wake override and not a wake config key. ADR-0014 governs where
// wake's own files live and deliberately keeps that surface small; it says nothing
// about where a foreign harness keeps its files, and reading the harness's own
// environment adds no key to wake's surface. What it does is keep the answer true:
// a resolver that ignored this variable would point every command at a directory
// Claude Code never writes, and each one would then report collecting *zero*
// rather than collecting nothing — the failure ADR-0010 asks doctor to make
// impossible, arriving through the resolver instead of through a reader.
const EnvClaudeConfigDir = "CLAUDE_CONFIG_DIR"

// ErrClaudeConfigDirNotAbsolute is returned when EnvClaudeConfigDir holds a
// relative path.
//
// Claude Code resolves such a value against its own process's working directory.
// Wake's differs, and the detached hook scan's is arbitrary (ADR-0016), so
// resolving it here would make the harness's directory depend on where the binary
// happened to be invoked from — a read of the wrong place, and on `wake init` a
// write to it. Refusing says so; guessing would not. The message names the
// variable and not its value, because the value is a path.
var ErrClaudeConfigDirNotAbsolute = errors.New(EnvClaudeConfigDir + " must be an absolute path")

// ClaudeCodeDir returns the directory Claude Code keeps its settings file, its
// session transcripts and its globally installed primitives in.
//
// It is deliberately not a field of Paths. Paths is where every file *this tool*
// owns lives, and ADR-0010 rests on uninstall being able to remove one of wake's
// roots and keep the other unambiguously — a foreign harness's directory in that
// struct would blur the line that makes the removal unambiguous.
//
// It creates nothing: resolving where the harness's files are is separate from
// creating them, so a command run in a home with no Claude Code install leaves
// that home as it found it.
//
// The whole resolution is EnvClaudeConfigDir, or os.UserHomeDir plus one join. The
// one variable read is the harness's own relocation mechanism rather than an
// override wake invents, and there is no platform branch (ADR-0021): the same
// answer on every supported system is what makes six commands agreeing on one
// directory checkable rather than merely intended.
//
// The error names the home directory as a concept and never its value, matching
// ResolvePaths' wrap — an error message is exactly where this kind of promise
// usually leaks (plan §4.2).
func ClaudeCodeDir() (string, error) {
	// Trimmed before the emptiness check and then kept trimmed, the way ResolvePaths
	// treats WAKE_DIR: an unset variable and one set to "" or a stray newline (as
	// `export CLAUDE_CONFIG_DIR=$(cat somefile)` produces) mean the same thing, and
	// Claude Code's own fallback treats the empty string as unset too.
	if relocated := strings.TrimSpace(os.Getenv(EnvClaudeConfigDir)); relocated != "" {
		if !filepath.IsAbs(relocated) {
			return "", ErrClaudeConfigDirNotAbsolute
		}
		return filepath.Clean(relocated), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving the home directory: %w", err)
	}
	return filepath.Join(home, claudeCodeDirName), nil
}
