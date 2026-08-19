package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSalt puts a well-formed salt at path, so a test that is about the file's
// type or mode fails for that reason and not for a wrong length.
func writeSalt(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("s", saltLen)), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// assertDisclosesNothing fails when a refusal's message names anything it must
// not: the path it refused, or a byte of what it refused to read. ADR-0007 fails
// closed without disclosing what it declined.
func assertDisclosesNothing(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want a refusal")
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(message, secret) {
			t.Errorf("error %q discloses %q", message, secret)
		}
	}
}

// A salt replaced by a symlink is a redirection: resolving it would read — and on
// the next first-run write, could write — wherever it points.
func TestOpenReposRejectsASaltThatIsASymlink(t *testing.T) {
	p := testPaths(t)
	elsewhere := filepath.Join(t.TempDir(), "their-salt")
	writeSalt(t, elsewhere)
	if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
		t.Fatalf("creating the config root: %v", err)
	}
	if err := os.Symlink(elsewhere, p.SaltFile); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := OpenRepos(p)

	assertDisclosesNothing(t, err, elsewhere, p.SaltFile, strings.Repeat("73", saltLen))
}

func TestOpenReposRejectsASaltThatIsNotARegularFile(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.SaltFile, 0o700); err != nil {
		t.Fatalf("creating a directory at the salt path: %v", err)
	}

	_, err := OpenRepos(p)

	assertDisclosesNothing(t, err, p.SaltFile)
	// A directory in the way must not become a salt beside it: refusing means
	// refusing, not routing around.
	info, statErr := os.Lstat(p.SaltFile)
	if statErr != nil || !info.IsDir() {
		t.Errorf("os.Lstat(salt) = (%v, %v), want the directory still there", info, statErr)
	}
}

// A salt readable by anyone else is a salt that no longer makes the id one-way for
// them, and every id already delivered was derived under it.
func TestOpenReposRejectsASaltMorePermissiveThan0600(t *testing.T) {
	for _, mode := range []os.FileMode{0o604, 0o640, 0o644, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			p := testPaths(t)
			writeSalt(t, p.SaltFile)
			if err := os.Chmod(p.SaltFile, mode); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}

			_, err := OpenRepos(p)

			assertDisclosesNothing(t, err, p.SaltFile, strings.Repeat("s", saltLen))
		})
	}
}

// The same three shapes for the project table. It is not a secret the way the salt
// is, but it decides which repository an event belongs to, so a file this build
// did not write is one it must not resolve against.
func TestOpenReposRejectsAProjectTableThatIsASymlink(t *testing.T) {
	p := testPaths(t)
	elsewhere := filepath.Join(t.TempDir(), "their-projects.json")
	if err := os.WriteFile(elsewhere, []byte(`{"version":1,"projects":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		t.Fatalf("creating the data root: %v", err)
	}
	if err := os.Symlink(elsewhere, p.ProjectsFile); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := OpenRepos(p)

	assertDisclosesNothing(t, err, elsewhere, p.ProjectsFile)
}

func TestOpenReposRejectsAProjectTableThatIsNotARegularFile(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.ProjectsFile, 0o700); err != nil {
		t.Fatalf("creating a directory at the table path: %v", err)
	}

	_, err := OpenRepos(p)

	assertDisclosesNothing(t, err, p.ProjectsFile)
}

func TestOpenReposRejectsAProjectTableMorePermissiveThan0600(t *testing.T) {
	for _, mode := range []os.FileMode{0o604, 0o640, 0o644, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			p := testPaths(t)
			writeProjectsJSON(t, p, `{"version":1,"projects":[]}`)
			if err := os.Chmod(p.ProjectsFile, mode); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}

			_, err := OpenRepos(p)

			assertDisclosesNothing(t, err, p.ProjectsFile)
		})
	}
}

// A state directory anyone in the group can write into is one where the salt or
// the table can be replaced between a read and the next.
func TestOpenReposRejectsAGroupWritableStateDirectory(t *testing.T) {
	for _, c := range []struct {
		name string
		mode os.FileMode
	}{
		{"group writable", 0o770},
		{"other writable", 0o707},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := testPaths(t)
			if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
				t.Fatalf("creating the data root: %v", err)
			}
			if err := os.Chmod(p.DataDir, c.mode); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(p.DataDir, 0o700) })

			_, err := OpenRepos(p)

			assertDisclosesNothing(t, err, p.DataDir)
		})
	}
}

// The check must not refuse the layout every normal install has. A fail-closed
// check that closes on the correct case is not a check, it is an outage.
func TestOpenReposAcceptsAPrivateStateDirectory(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))

	r, err := OpenRepos(p)
	if err != nil {
		t.Fatalf("OpenRepos() = %v, want the 0700 baseline to open", err)
	}
	if _, err := r.Register(root, "repo", time.Time{}); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	if _, err := OpenRepos(p); err != nil {
		t.Fatalf("OpenRepos() after a write = %v — the files this build wrote must satisfy its own check", err)
	}
}

// rewriteTable decodes projects.json, hands the entries to fn, and republishes at
// 0600. It is how a test produces a table only a hand edit could produce.
func rewriteTable(t *testing.T, p Paths, fn func(entries []projectEntry) []projectEntry) {
	t.Helper()
	raw, err := os.ReadFile(p.ProjectsFile)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var table projectsFile
	if unmarshalErr := json.Unmarshal(raw, &table); unmarshalErr != nil {
		t.Fatalf("Unmarshal() error = %v", unmarshalErr)
	}
	table.Projects = fn(table.Projects)
	edited, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(p.ProjectsFile, append(edited, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

// The id is the attribution. An entry whose id is not the one the salt and the
// recorded root derive is an entry somebody wrote by hand, and honouring it would
// let a hand-edited file decide which repository an event belongs to.
func TestATamperedProjectIdDoesNotChangeAttribution(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	mustRegister(t, openRepos(t, p), root, "repo")

	tampered := hexID('c')
	rewriteTable(t, p, func(entries []projectEntry) []projectEntry {
		entries[0].ID = tampered
		return entries
	})

	r := openRepos(t, p)
	got := mustIdentify(t, r, root)

	if got.Matched {
		t.Error("Matched = true, want false — a tampered entry must stop resolving")
	}
	if got.ID == tampered {
		t.Errorf("ID = %q, the hand-written id; the table decided the attribution", got.ID)
	}
	if r.DroppedEntries() < 1 {
		t.Errorf("DroppedEntries() = %d, want at least 1 — the refusal has to be visible", r.DroppedEntries())
	}
}

// Two entries claiming the same id, root or spelling are ambiguous, and every
// member of such a set is refused. Preferring one would be choosing an
// attribution, which is exactly what a hand edit must not be able to do.
//
// Both duplicates below carry the ids and the match digests their own contents
// really derive, so neither keyed check catches them and the ambiguity pass is what
// is under test. The case where a duplicate's id does not derive is the test below
// this one, and it has a different — weaker, and correct — outcome.
func TestADuplicateProjectEntryDoesNotChangeAttribution(t *testing.T) {
	for _, c := range []struct {
		name      string
		duplicate func(r *Repos, original projectEntry) projectEntry
	}{
		{"the entry recorded twice", func(_ *Repos, original projectEntry) projectEntry {
			return original
		}},
		{"a second repository claiming the first's root as an alias", func(r *Repos, original projectEntry) projectEntry {
			other := filepath.Join(filepath.Dir(original.Root), "other")
			return r.signed(projectEntry{ID: r.hashRoot(other), Label: "other", Root: other, Aliases: []string{original.Root}})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := testPaths(t)
			root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
			opened := openRepos(t, p)
			mustRegister(t, opened, root, "repo")

			rewriteTable(t, p, func(entries []projectEntry) []projectEntry {
				return append(entries, c.duplicate(opened, entries[0]))
			})

			r := openRepos(t, p)
			got := mustIdentify(t, r, root)

			if got.Matched {
				t.Error("Matched = true, want false — an ambiguous set must resolve to nothing")
			}
			if r.DroppedEntries() < 2 {
				t.Errorf("DroppedEntries() = %d, want at least 2 — every member of the set is refused", r.DroppedEntries())
			}
		})
	}
}

// An alias is a recorded spelling, so it decides attribution exactly as the root
// does — and it decides consent too, because ConsentedRoot answers with whichever
// spelling matched and discovery reads the tree under it. An id derived from the
// root alone says nothing about the aliases beside it, so a single otherwise
// legitimate entry with one hand-added alias would pass every other check: no
// spelling is duplicated, so the ambiguity pass sees nothing, and valid() asks only
// that an alias be absolute and clean.
func TestAHandAddedAliasDoesNotChangeAttributionOrWidenConsent(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	root := mkdirAll(t, filepath.Join(base, "repo"))
	unrelated := mkdirAll(t, filepath.Join(base, "unrelated"))
	consented := mustRegister(t, openRepos(t, p), root, "repo")

	rewriteTable(t, p, func(entries []projectEntry) []projectEntry {
		entries[0].Aliases = append(entries[0].Aliases, unrelated)
		return entries
	})

	r := openRepos(t, p)

	got := mustIdentify(t, r, unrelated)
	if got.Matched || got.ID == consented {
		t.Errorf("Identify(unrelated) = %+v, want Matched false and an id other than %q — the hand edit decided the attribution", got, consented)
	}
	allowed, err := r.ConsentedRoot(unrelated)
	if err != nil {
		t.Fatalf("ConsentedRoot() = %v", err)
	}
	if allowed != "" {
		t.Error("ConsentedRoot(unrelated) answered with a root, so discovery would read a tree the user never consented to")
	}
	if r.DroppedEntries() < 1 {
		t.Errorf("DroppedEntries() = %d, want at least 1 — the refusal has to be visible", r.DroppedEntries())
	}
}

// The alias check must refuse the hand edit, not aliases as such: the spelling
// Register itself records has to keep resolving.
func TestARegisteredAliasKeepsResolving(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	root := mkdirAll(t, filepath.Join(base, "repo"))
	link := filepath.Join(base, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	want := mustRegister(t, openRepos(t, p), link, "repo")

	r := openRepos(t, p)

	for _, cwd := range []string{root, link, filepath.Join(link, "sub")} {
		if got := mustIdentify(t, r, cwd); !got.Matched || got.ID != want {
			t.Errorf("Identify(%q) = %+v, want id %q with Matched true", cwd, got, want)
		}
	}
	if r.DroppedEntries() != 0 {
		t.Errorf("DroppedEntries() = %d, want 0 — this build must trust the aliases it recorded itself", r.DroppedEntries())
	}
}

// An entry appended by hand for a root that already has one cannot carry a derived
// id unless it copies the recorded one, so the id check refuses it on its own and
// the recorded entry keeps resolving. Attribution is unchanged either way, which is
// the property that matters — but the outcome differs from the ambiguous case, so
// it is asserted rather than assumed.
func TestASecondEntryForTheSameRootIsRefusedOnItsOwn(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	want := mustRegister(t, openRepos(t, p), root, "repo")

	rewriteTable(t, p, func(entries []projectEntry) []projectEntry {
		return append(entries, projectEntry{ID: hexID('d'), Label: "impostor", Root: entries[0].Root})
	})

	r := openRepos(t, p)
	got := mustIdentify(t, r, root)

	if !got.Matched || got.ID != want {
		t.Errorf("Identify() = %+v, want id %q with Matched true — the hand-written entry changed the attribution", got, want)
	}
	if r.DroppedEntries() < 1 {
		t.Errorf("DroppedEntries() = %d, want at least 1 — the refusal has to be visible", r.DroppedEntries())
	}
}

// Refusing on read is not enough: a later Register republishes the whole table,
// so a refused entry must not be carried back into the file it was refused from.
func TestRegisterDoesNotRepublishATamperedEntry(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	first := mkdirAll(t, filepath.Join(base, "first"))
	second := mkdirAll(t, filepath.Join(base, "second"))
	mustRegister(t, openRepos(t, p), first, "first")

	tampered := hexID('c')
	rewriteTable(t, p, func(entries []projectEntry) []projectEntry {
		entries[0].ID = tampered
		return entries
	})

	r := openRepos(t, p)
	secondID := mustRegister(t, r, second, "second")

	table, _, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	for _, entry := range table.Projects {
		if entry.ID == tampered {
			t.Error("the tampered entry was republished; a refused entry must not survive a write")
		}
	}
	if len(table.Projects) != 1 || table.Projects[0].ID != secondID {
		t.Errorf("the table holds %d entries; want only the newly registered one", len(table.Projects))
	}
	if got := mustIdentify(t, r, second); !got.Matched || got.ID != secondID {
		t.Errorf("Identify(second) = %+v, want id %q with Matched true", got, secondID)
	}
}
