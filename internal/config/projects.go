package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
)

// projectsVersion is the format version stamped on every write. A future format
// read as this one would silently produce different identities, with no error and
// nothing to migrate back from — the one failure the resolution table cannot
// recover from — so an unrecognised version stops the read instead.
const projectsVersion = 1

// idHexLen is the width of a repository id: 128 bits of HMAC-SHA256 as 32
// lowercase hex characters (ADR-0019 §8). It lives here because this is the file
// that validates the stored form; identity.go slices a hash to it.
const idHexLen = 32

// The modes projects.json and the data root are created with (ADR-0014). The
// file holds real repository paths and real labels, which is why the mode is
// asserted by a test rather than left to the process umask.
const (
	projectsFileMode = fs.FileMode(0o600)
	dataDirMode      = fs.FileMode(0o700)
)

// projectsFile is the on-disk resolution table.
//
// Since ADR-0019 it has two jobs, not one: it is still the map from a hashed id
// to a readable label, and it is now also what derivation resolves a working
// directory against. Its contents are therefore load-bearing for correctness,
// not only for display — while staying local, 0600, and never travelling.
type projectsFile struct {
	Version  int            `json:"version"`
	Projects []projectEntry `json:"projects"`
}

// projectEntry is one consented repository.
type projectEntry struct {
	// ID is the salted hash of Root: idHexLen lowercase hex characters.
	ID string `json:"id"`
	// Label is the readable name, for display only. It never leaves this
	// package.
	Label string `json:"label"`
	// Root is the canonical consented root — absolute, clean, and with symlinks
	// already resolved at registration.
	Root string `json:"root"`
	// Aliases are other spellings of Root that a harness may have recorded,
	// most often the pre-symlink path a process actually saw (/tmp/x where the
	// real path is /private/tmp/x).
	//
	// They exist because derivation may not touch the filesystem (ADR-0019 §1)
	// while §5 still requires symlinks to resolve to one identity. Recording the
	// spelling at registration, where the directory is present, is what lets
	// derivation stay a pure string operation and still fold both spellings onto
	// the same id.
	Aliases []string `json:"aliases,omitempty"`
	// CaseInsensitive records what the filesystem under Root did at
	// registration time. ADR-0019 §5 folds case only on a case-insensitive
	// filesystem — on ext4, ~/Dev and ~/dev are genuinely different directories
	// and folding would merge two repositories — and derivation cannot probe for
	// it, so the answer is recorded here instead.
	CaseInsensitive bool `json:"case_insensitive"`
}

// valid reports whether an entry is one this build is willing to resolve
// against.
//
// Failing closed here is the point (constraint 22): an entry that cannot be
// trusted is dropped and counted, never repaired. A repaired entry would resolve
// to a different identity than the one it was written with, which is
// indistinguishable from correct output.
func (e projectEntry) valid() bool {
	if !validID(e.ID) {
		return false
	}
	// A label containing a path separator is rejected by plan §3.4; it is also
	// how a label would start looking like the path it must never be.
	if e.Label == "" || strings.ContainsAny(e.Label, "/"+string(filepath.Separator)) {
		return false
	}
	if !validRoot(e.Root) {
		return false
	}
	for _, alias := range e.Aliases {
		if !validRoot(alias) {
			return false
		}
	}
	return true
}

// validID reports whether id has the exact shape ADR-0019 §8 fixes. Lower case
// only: two spellings of one id would be two ids downstream, where T003
// validates the same shape.
func validID(id string) bool {
	if len(id) != idHexLen {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// validRoot reports whether a recorded path is one derivation can match against:
// absolute, and already lexically clean. Cleaning it here instead would repair
// the entry rather than reject it, and the id was computed over whatever was
// recorded — so a root that needs cleaning is a root whose id no longer follows
// from it.
func validRoot(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

// readProjects reads the resolution table.
//
// A missing file is an empty table, not an error, and creates nothing: a fresh
// install has no table, and asking which repository a directory belongs to must
// not write one. A file that does not parse *is* an error — treating it as empty
// would hand every repository a new identity on the next scan — and the file is
// never rewritten from a failed parse.
//
// Entries that fail validation are dropped and counted. The count is returned so
// doctor can report it, since a silently shrinking table is the failure mode this
// design cannot otherwise see.
//
// The file's type and mode are checked before it is opened, for a different reason
// than the salt's: this table is not a secret, but it decides which repository an
// event belongs to. A symlink standing in for it points resolution — and the next
// republication — somewhere else, and a mode looser than 0600 means anyone else on
// the machine can rewrite that decision. The refusal names the file's role and
// never the file (plan §4.2).
func readProjects(path string) (projectsFile, int, error) {
	if err := checkSensitiveFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return projectsFile{Version: projectsVersion}, 0, nil
		}
		return projectsFile{}, 0, fmt.Errorf("the project table %w", err)
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return projectsFile{Version: projectsVersion}, 0, nil
	}
	if err != nil {
		return projectsFile{}, 0, fmt.Errorf("reading %s: %w", path, err)
	}

	var table projectsFile
	if err := json.Unmarshal(raw, &table); err != nil {
		return projectsFile{}, 0, fmt.Errorf("parsing %s: %s", path, parseFailure(err))
	}
	if table.Version != projectsVersion {
		return projectsFile{}, 0, fmt.Errorf("%s is table version %d; this build reads version %d", path, table.Version, projectsVersion)
	}

	kept := make([]projectEntry, 0, len(table.Projects))
	dropped := 0
	for _, entry := range table.Projects {
		if !entry.valid() {
			dropped++
			continue
		}
		kept = append(kept, entry)
	}
	table.Projects = kept
	return table, dropped, nil
}

// parseFailure renders why the table did not parse, without quoting any of it.
//
// The decoder's own message embeds the offending bytes, and the bytes in this
// file are repository paths (plan §4.2). A byte offset says where to look
// without saying what is there.
func parseFailure(err error) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("invalid JSON at byte %d", syntax.Offset)
	}
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		return fmt.Sprintf("the %q field has the wrong type", mismatch.Field)
	}
	return "invalid JSON"
}

// writeProjects replaces the table atomically and durably.
//
// Atomically because the table is what every identity resolves against: a
// half-written file is not a table with fewer repositories in it, it is a table
// that fails to parse, and the next scan would stop rather than guess. A reader
// therefore sees either the old table or the new one, never a partial one — and
// a write reported as successful is one a power loss cannot undo.
//
// Both properties come from atomicfile, including the removal of the in-progress
// temporary file on every failure path: a leftover .publish-* next to this file
// is a full copy of a table of real repository paths that nothing would ever
// clean up.
func writeProjects(path string, table projectsFile) error {
	table.Version = projectsVersion

	data, marshalErr := json.MarshalIndent(table, "", "  ")
	if marshalErr != nil {
		return fmt.Errorf("encoding the project table: %w", marshalErr)
	}
	data = append(data, '\n')

	return atomicfile.Publish(path, data, projectsFileMode)
}
