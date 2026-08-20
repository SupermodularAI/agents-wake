package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
)

// saltLen is the salt's length in bytes (ADR-0019 §3). 32 bytes is HMAC-SHA256's
// block-appropriate key length and far beyond what the threat needs: the point
// is that a repository path — a tiny, guessable input space — cannot be
// enumerated back out of the id.
const saltLen = 32

// saltFileMode is the mode repo-salt is created with (ADR-0019 §4). It holds a
// secret, and it is the reason the identity in a delivered record is one-way
// rather than an encoding of a path.
const saltFileMode = fs.FileMode(0o600)

// errSaltWrongLength is returned for an existing salt file that is not saltLen
// bytes. It is a sentinel because the honest response is to stop, and a caller
// has to be able to tell that case from a missing file.
var errSaltWrongLength = errors.New("the salt file is the wrong length")

// loadOrCreateSalt returns the per-machine salt, generating it on first need.
//
// Three properties this function exists to hold:
//
// It never regenerates. Rotating the salt re-identifies the entire history, so it
// is a destructive, explicit operation (ADR-0019 §3) and never a side effect of a
// read. A file that exists and is the right length is used as it is.
//
// It fails closed on a file it does not understand. A salt of the wrong length is
// a truncated write or somebody else's file; using it would produce ids nothing
// else agrees with, and replacing it would silently re-identify every repository.
// The error states lengths only — the bytes are the secret. The same refusal
// covers a file it is not willing to read at all: a symlink standing in for the
// salt is a redirection, and a mode looser than 0600 means the key to every id
// already delivered is readable by someone else. Refusing is the answer in both
// cases, and refusing without naming the file or its contents.
//
// It loses races safely. Two first runs at once — a scan and a hook firing
// together — are resolved by the salt file appearing whole or not at all: the
// bytes are written to a temporary file in the same directory and that file is
// linked into place, so the losing creator re-reads the winner's complete salt
// rather than overwriting it or catching it half-written. Creating the file first
// and writing it second would be visible at zero bytes for as long as the write
// takes, and a loser reading it there fails closed on a wrong length — a
// legitimate first run reported as a corrupt one. os.Link, not os.Rename: rename
// would replace a salt the winner has already handed out, and link fails with
// fs.ErrExist instead, which is the signal to go and read theirs.
func loadOrCreateSalt(p Paths) ([]byte, error) {
	// Anything other than "not there yet" is the caller's answer: an existing
	// salt is returned as it is, and an unreadable or wrong-length one stops
	// here rather than being replaced.
	if salt, err := readSalt(p); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return salt, err
	}

	if err := os.MkdirAll(p.ConfigDir, configDirMode); err != nil {
		return nil, fmt.Errorf("creating %s: %w", p.ConfigDir, err)
	}

	fresh := make([]byte, saltLen)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("generating the repository-id salt: %w", err)
	}

	f, err := os.CreateTemp(p.ConfigDir, "repo-salt-*")
	if err != nil {
		return nil, fmt.Errorf("creating a temporary file in %s: %w", p.ConfigDir, err)
	}
	tmp := f.Name()

	if _, err := f.Write(fresh); err != nil {
		// A partial salt file would fail every later run by design, so the
		// failed attempt is removed rather than left to be found.
		return nil, errors.Join(fmt.Errorf("writing %s: %w", tmp, err), f.Close(), os.Remove(tmp))
	}
	// CreateTemp already opens at 0600; setting it explicitly keeps the mode a
	// stated property of this function rather than a property of the standard
	// library's default. The link below shares this file's inode, so this is the
	// mode repo-salt ends up with.
	if err := f.Chmod(saltFileMode); err != nil {
		return nil, errors.Join(fmt.Errorf("setting the mode of %s: %w", tmp, err), f.Close(), os.Remove(tmp))
	}
	// Before the link, not after: the salt is never regenerated, so a salt file
	// whose directory entry survived a power loss while its bytes did not is a
	// file this build will refuse for the rest of the machine's life — and every
	// id already derived from the real salt becomes unreachable.
	if err := f.Sync(); err != nil {
		return nil, errors.Join(fmt.Errorf("syncing %s: %w", tmp, err), f.Close(), os.Remove(tmp))
	}
	if err := f.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("closing %s: %w", tmp, err), os.Remove(tmp))
	}

	linkErr := os.Link(tmp, p.SaltFile)
	// The temporary file goes either way: on success it is a second name for a
	// file that now has its real one, and on failure it is a copy of the secret
	// that nothing would ever clean up.
	removeErr := os.Remove(tmp)

	switch {
	case errors.Is(linkErr, fs.ErrExist):
		// Someone else published a salt between the read and here. Theirs is the
		// one already hashed with, so it wins — and because it appeared whole,
		// reading it back cannot see a partial file.
		salt, readErr := readSalt(p)
		if err := errors.Join(readErr, removeErr); err != nil {
			return nil, err
		}
		return salt, nil
	case linkErr != nil:
		return nil, errors.Join(fmt.Errorf("creating %s: %w", p.SaltFile, linkErr), removeErr)
	case removeErr != nil:
		return nil, fmt.Errorf("removing %s: %w", tmp, removeErr)
	}
	// The link created a directory entry, and that entry has to be durable for the
	// same reason the bytes do: a salt that came back after a crash under a
	// different name than the one the ids were derived with is not recoverable. A
	// failure here joins the answer rather than being ignored — the caller has to
	// know the salt it is about to hash with may not be there next boot.
	if err := atomicfile.SyncDir(p.ConfigDir); err != nil {
		return nil, fmt.Errorf("syncing %s: %w", p.ConfigDir, err)
	}
	return fresh, nil
}

// readSalt reads an existing salt file. A missing file is reported as
// fs.ErrNotExist so the caller can tell "not yet" from "wrong".
//
// The file's type and mode are checked before it is opened. The salt is the one
// secret this tool holds — it is what makes a repository id one-way — so a file
// that is a symlink, is not a regular file, or is reachable by anyone other than
// its owner is one this build refuses rather than one it reads and carries on with.
// The role is a fixed literal here and the fault carries no path, so the message
// names neither the file nor a byte of it (plan §4.2).
func readSalt(p Paths) ([]byte, error) {
	if err := checkSensitiveFile(p.SaltFile); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("the repository-id salt %w", err)
	}
	salt, err := os.ReadFile(p.SaltFile)
	if err != nil {
		// Not wrapped with a message: fs.ErrNotExist is the control-flow
		// signal, and os.ReadFile's error already names the file.
		return nil, err
	}
	if len(salt) != saltLen {
		return nil, fmt.Errorf("the repository-id salt holds %d bytes, want %d: %w", len(salt), saltLen, errSaltWrongLength)
	}
	return salt, nil
}
