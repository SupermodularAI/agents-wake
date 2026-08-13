package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
// The error states lengths only — the bytes are the secret.
//
// It loses races safely. Two first runs at once — a scan and a hook firing
// together — are resolved by O_EXCL: the creator that loses re-reads the winner's
// salt rather than overwriting it. Without that, one of the two would write a
// salt the other had already hashed with.
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

	f, err := os.OpenFile(p.SaltFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, saltFileMode)
	if errors.Is(err, fs.ErrExist) {
		// Someone else created it between the read and here. Their salt is the
		// one already hashed with, so it wins.
		return readSalt(p)
	}
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", p.SaltFile, err)
	}

	if _, err := f.Write(fresh); err != nil {
		// A partial salt file would fail every later run by design, so the
		// failed attempt is removed rather than left to be found.
		return nil, errors.Join(fmt.Errorf("writing %s: %w", p.SaltFile, err), f.Close(), os.Remove(p.SaltFile))
	}
	if err := f.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("closing %s: %w", p.SaltFile, err), os.Remove(p.SaltFile))
	}
	return fresh, nil
}

// readSalt reads an existing salt file. A missing file is reported as
// fs.ErrNotExist so the caller can tell "not yet" from "wrong".
func readSalt(p Paths) ([]byte, error) {
	salt, err := os.ReadFile(p.SaltFile)
	if err != nil {
		// Not wrapped with a message: fs.ErrNotExist is the control-flow
		// signal, and os.ReadFile's error already names the file.
		return nil, err
	}
	if len(salt) != saltLen {
		return nil, fmt.Errorf("%s holds %d bytes, want %d: %w", p.SaltFile, len(salt), saltLen, errSaltWrongLength)
	}
	return salt, nil
}
