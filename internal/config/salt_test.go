package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Acceptance item 9. Under the config root, not the data root: ADR-0015 requires
// the store to be disposable — "no backups, no fear of rm -rf" — and a salt in
// there would mean deleting the store silently re-identifies every repository.
func TestSaltFileIsCreated0600UnderTheConfigRoot(t *testing.T) {
	p := testPaths(t)

	salt, err := loadOrCreateSalt(p)
	if err != nil {
		t.Fatalf("loadOrCreateSalt() = %v", err)
	}
	if len(salt) != saltLen {
		t.Errorf("salt is %d bytes, want %d", len(salt), saltLen)
	}

	if want := filepath.Join(p.ConfigDir, "repo-salt"); p.SaltFile != want {
		t.Errorf("SaltFile = %q, want %q", p.SaltFile, want)
	}
	fi, err := os.Stat(p.SaltFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("repo-salt mode = %#o, want 0600", perm)
	}
	if fi.Size() != saltLen {
		t.Errorf("repo-salt is %d bytes on disk, want %d", fi.Size(), saltLen)
	}
	di, err := os.Stat(p.ConfigDir)
	if err != nil {
		t.Fatalf("stat the config root: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("config root mode = %#o, want 0700", perm)
	}

	// Nothing salt-shaped may exist under the data root, whatever it is called.
	if _, err := os.Lstat(p.DataDir); !errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(p.DataDir)
		if readErr != nil {
			t.Fatalf("reading the data root: %v", readErr)
		}
		for _, e := range entries {
			t.Errorf("the data root holds %q; the salt and nothing derived from it may live there", e.Name())
		}
	}
}

// Rotating the salt re-identifies every repository, so it is a destructive,
// explicit operation (ADR-0019 §3) — never something a second run does by
// itself.
func TestSecondRunReusesTheSaltRatherThanRegenerating(t *testing.T) {
	p := testPaths(t)

	first, err := loadOrCreateSalt(p)
	if err != nil {
		t.Fatalf("first loadOrCreateSalt() = %v", err)
	}
	before, err := os.Stat(p.SaltFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	second, err := loadOrCreateSalt(p)
	if err != nil {
		t.Fatalf("second loadOrCreateSalt() = %v", err)
	}
	after, err := os.Stat(p.SaltFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Error("the second run returned a different salt; every repository id would change")
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("repo-salt was rewritten (mtime %v -> %v); it must be read, not regenerated", before.ModTime(), after.ModTime())
	}
}

// Two machines must not share a salt by accident. This also catches a salt
// derived from anything predictable, such as the path or the hostname.
func TestEachConfigRootGetsItsOwnSalt(t *testing.T) {
	first, err := loadOrCreateSalt(testPaths(t))
	if err != nil {
		t.Fatalf("loadOrCreateSalt() = %v", err)
	}
	second, err := loadOrCreateSalt(testPaths(t))
	if err != nil {
		t.Fatalf("loadOrCreateSalt() = %v", err)
	}

	if bytes.Equal(first, second) {
		t.Error("two independent config roots produced the same salt; it is not random")
	}
}

// Fail closed. A short file is either a truncated write or something else's
// file; using it would produce ids nothing else agrees with, and replacing it
// would silently re-identify every repository. The message states lengths only —
// the bytes are the secret.
func TestAWrongLengthSaltFileIsAnErrorAndIsNotRegenerated(t *testing.T) {
	for _, content := range []string{"", "short", strings.Repeat("x", saltLen+1)} {
		t.Run(strconv.Itoa(len(content)), func(t *testing.T) {
			p := testPaths(t)
			if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
				t.Fatalf("creating the config root: %v", err)
			}
			if err := os.WriteFile(p.SaltFile, []byte(content), 0o600); err != nil {
				t.Fatalf("writing repo-salt: %v", err)
			}

			salt, err := loadOrCreateSalt(p)
			if err == nil {
				t.Fatalf("loadOrCreateSalt() = (%d bytes, nil), want an error", len(salt))
			}
			if !errors.Is(err, errSaltWrongLength) {
				t.Errorf("loadOrCreateSalt() = %v, want errSaltWrongLength", err)
			}
			if strings.Contains(err.Error(), content) && content != "" {
				t.Errorf("the error %q contains the file's bytes", err)
			}

			raw, err := os.ReadFile(p.SaltFile)
			if err != nil {
				t.Fatalf("reading repo-salt: %v", err)
			}
			if string(raw) != content {
				t.Error("repo-salt was rewritten; a salt is never replaced implicitly")
			}
		})
	}
}

// The ADR-0015 property, stated as a test: deleting the store costs the readable
// labels, which init restores, and keeps every identity intact.
func TestDeletingTheDataRootKeepsTheSalt(t *testing.T) {
	p := testPaths(t)

	before, beforeErr := loadOrCreateSalt(p)
	if beforeErr != nil {
		t.Fatalf("loadOrCreateSalt() = %v", beforeErr)
	}
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		t.Fatalf("creating the data root: %v", err)
	}
	if err := os.RemoveAll(p.DataDir); err != nil {
		t.Fatalf("removing the data root: %v", err)
	}

	after, afterErr := loadOrCreateSalt(p)
	if afterErr != nil {
		t.Fatalf("loadOrCreateSalt() after rm -rf = %v", afterErr)
	}
	if !bytes.Equal(before, after) {
		t.Error("the salt changed when the data root was deleted; every repository id would change with it")
	}
}

// Two first runs at once — a scan and a hook firing together — must not have one
// overwrite the other's salt. O_EXCL is what makes the loser re-read instead of
// winning. Run with -race, this is also the only concurrent path in the package.
func TestConcurrentFirstRunsAgreeOnOneSalt(t *testing.T) {
	p := testPaths(t)

	const runners = 8
	salts := make([][]byte, runners)
	errs := make([]error, runners)

	var wg sync.WaitGroup
	for i := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			salts[i], errs[i] = loadOrCreateSalt(p)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: loadOrCreateSalt() = %v", i, err)
		}
	}
	for i := 1; i < runners; i++ {
		if !bytes.Equal(salts[0], salts[i]) {
			t.Fatalf("runner %d got a different salt from runner 0; a concurrent creator overwrote one", i)
		}
	}
	// Seven of the eight runners lost the race; none of them may leave the salt
	// it generated behind.
	assertNoLeftoverSaltFiles(t, p)
}

// The other half of losing a race safely: the salt file's appearance has to be
// atomic. A loser reads the file the moment it exists, and a wrong-length read is
// fail-closed and permanent by design — so a file that is visible before its 32
// bytes are in it turns a legitimate first run into errSaltWrongLength. This
// watches the path a loser reads while a first run creates it, and asserts the
// file is never readable in a state readSalt rejects.
func TestTheSaltFileIsNeverVisibleAtAPartialLength(t *testing.T) {
	// Each attempt is one narrow window; a handful of them is what makes hitting
	// it likely rather than lucky.
	const attempts = 50

	for range attempts {
		p := testPaths(t)

		stop := make(chan struct{})
		var (
			wg      sync.WaitGroup
			partial error
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, err := readSalt(p)
				switch {
				case err == nil:
					// A complete salt is on disk; there is nothing left to catch.
					return
				case !errors.Is(err, os.ErrNotExist):
					// Exactly what a losing creator would have returned.
					partial = err
					return
				}
				select {
				case <-stop:
					return
				default:
				}
			}
		}()

		salt, err := loadOrCreateSalt(p)
		close(stop)
		wg.Wait()

		if err != nil {
			t.Fatalf("loadOrCreateSalt() = %v", err)
		}
		if len(salt) != saltLen {
			t.Fatalf("loadOrCreateSalt() returned %d bytes, want %d", len(salt), saltLen)
		}
		if partial != nil {
			t.Fatalf("a concurrent reader saw the salt file mid-creation: %v", partial)
		}
		assertNoLeftoverSaltFiles(t, p)
	}
}

// The salt is written somewhere else first and linked into place, so the config
// root must not be left holding a second copy of it: a stray file with the salt
// in it is the secret sitting where nothing would ever clean it up.
func assertNoLeftoverSaltFiles(t *testing.T, p Paths) {
	t.Helper()

	entries, err := os.ReadDir(p.ConfigDir)
	if err != nil {
		t.Fatalf("reading the config root: %v", err)
	}
	for _, e := range entries {
		if name := e.Name(); name != filepath.Base(p.SaltFile) && strings.HasPrefix(name, filepath.Base(p.SaltFile)) {
			t.Errorf("the config root still holds %q; it is a copy of the salt", name)
		}
	}
}

// Acceptance item 10, the part this file owns: the salt is not a setting, and it
// is not in the file users paste into bug reports.
func TestTheSaltIsNotWrittenIntoConfigToml(t *testing.T) {
	p := testPaths(t)

	salt, saltErr := loadOrCreateSalt(p)
	if saltErr != nil {
		t.Fatalf("loadOrCreateSalt() = %v", saltErr)
	}
	if _, err := Set(p, "ui.default_window", "7d"); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	raw, err := os.ReadFile(p.ConfigFile)
	if err != nil {
		t.Fatalf("reading config.toml: %v", err)
	}
	for _, form := range [][]byte{salt, []byte(hex.EncodeToString(salt))} {
		if bytes.Contains(raw, form) {
			t.Error("config.toml contains the salt")
		}
	}
}
