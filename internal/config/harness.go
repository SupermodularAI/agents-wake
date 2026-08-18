package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// claudeCodeDirName is the one place the harness's own directory name is spelled.
//
// It is unexported and no variable and no config key relocates it: honouring a
// relocation variable would be a decision of its own (ADR-0014, and the ticket's
// Out of scope), and until one is made, six commands agreeing on one name is the
// property worth having.
const claudeCodeDirName = ".claude"

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
// The whole resolution is os.UserHomeDir plus one join. No environment variable
// relocates it (ADR-0014) and there is no platform branch (ADR-0021): the same
// home-relative name on every supported system is what makes six commands
// agreeing on one directory checkable rather than merely intended.
//
// The error names the home directory as a concept and never its value, matching
// ResolvePaths' wrap — an error message is exactly where this kind of promise
// usually leaks (plan §4.2).
func ClaudeCodeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving the home directory: %w", err)
	}
	return filepath.Join(home, claudeCodeDirName), nil
}
