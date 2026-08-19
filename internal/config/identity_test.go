package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tempRealDir returns a fresh temporary directory with every symlink already
// resolved.
//
// t.TempDir() on macOS hands back a path under /var, which is itself a symlink to
// /private/var. Using it raw would make every test that registers a root also
// exercise the alias path, so a test asserting "the recorded root is the one I
// registered" would fail on macOS for a reason that has nothing to do with what
// it is testing. The tests that want the symlink case build the link explicitly.
func tempRealDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temporary directory: %v", err)
	}
	return dir
}

// mkdirAll creates a directory the test needs to exist, since registration runs
// inside a live directory.
func mkdirAll(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	return path
}

func openRepos(t *testing.T, p Paths) *Repos {
	t.Helper()
	r, err := OpenRepos(p)
	if err != nil {
		t.Fatalf("OpenRepos() = %v", err)
	}
	return r
}

func mustRegister(t *testing.T, r *Repos, root, label string) string {
	t.Helper()
	id, err := r.Register(root, label, time.Time{})
	if err != nil {
		t.Fatalf("Register(%q, %q) = %v", root, label, err)
	}
	return id
}

func mustIdentify(t *testing.T, r *Repos, cwd string) Identity {
	t.Helper()
	got, err := r.Identify(cwd)
	if err != nil {
		t.Fatalf("Identify(%q) = %v", cwd, err)
	}
	return got
}

// hexID is a well-formed id for a hand-written table: the shapes below are ones
// Register would never produce, which is the point of writing them by hand.
func hexID(b byte) string {
	return strings.Repeat(string(b), idHexLen)
}

// derivedID returns the id the salt under p derives for root.
//
// A hand-written table has to carry real ids to test anything but the refusal: the
// table is verified against the salt, so an entry whose id is not the one its root
// derives stops resolving. A test about longest-prefix matching or case folding
// would otherwise be asserting that a refused entry does not resolve, which it does
// not test and which another test covers. Calling this also creates the salt, so the
// ids and the later OpenRepos agree.
func derivedID(t *testing.T, p Paths, root string) string {
	t.Helper()
	return openRepos(t, p).hashRoot(root)
}

// derivedMAC returns the match digest the salt under p derives for entry.
//
// A hand-written table needs it for the same reason it needs derivedID: the digest
// is verified against the salt, so an entry carrying the wrong one — or none — stops
// resolving, and a test about longest-prefix matching or case folding would be
// asserting that a refused entry does not resolve. The tests that want an entry only
// a hand edit could produce leave it out on purpose.
func derivedMAC(t *testing.T, p Paths, entry projectEntry) string {
	t.Helper()
	return openRepos(t, p).matchMAC(entry)
}

// Acceptance item 11, second half, and ADR-0019 §8: 128 bits as 32 lowercase hex
// characters. T003 validates the same shape downstream, so two spellings of one
// id would be two ids there.
func TestIDIs32HexCharacters(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	root := mkdirAll(t, filepath.Join(base, "repo"))
	r := openRepos(t, p)

	registered := mustRegister(t, r, root, "repo")
	fallback := mustIdentify(t, r, filepath.Join(base, "never-registered")).ID

	for _, id := range []string{registered, fallback} {
		if len(id) != idHexLen {
			t.Errorf("id %q is %d characters, want %d", id, len(id), idHexLen)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Errorf("id %q is not hex: %v", id, err)
		}
		if id != strings.ToLower(id) {
			t.Errorf("id %q is not lower case", id)
		}
	}
}

// Acceptance item 11, first half. The salt is what makes the id one-way: a repo
// path is a tiny, guessable input space (ADR-0019 § An unsalted path hash is not
// one-way), so two machines must not agree by construction.
func TestTwoDifferentSaltsProduceDifferentIDsForTheSameRoot(t *testing.T) {
	base := tempRealDir(t)
	root := mkdirAll(t, filepath.Join(base, "repo"))

	ids := make([]string, 0, 2)
	for range 2 {
		// A fresh HOME is a fresh config root, and therefore a fresh salt.
		p := testPaths(t)
		ids = append(ids, mustRegister(t, openRepos(t, p), root, "repo"))
	}

	if ids[0] == ids[1] {
		t.Errorf("both salts produced %q; the id does not depend on the salt", ids[0])
	}
	for _, id := range ids {
		if len(id) != idHexLen {
			t.Errorf("id %q is %d characters, want %d", id, len(id), idHexLen)
		}
	}
}

// Acceptance item 6, and the consent hole ADR-0019 exists to close: a session
// started in a subdirectory of a consented repo must not hash differently from
// the repo the user opted into, because the failure is silent — the consented
// repo collects nothing and nothing reports it.
func TestSubdirectoryHashesIdenticallyToTheRoot(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	r := openRepos(t, p)
	want := mustRegister(t, r, root, "repo")

	for _, cwd := range []string{
		root,
		filepath.Join(root, "internal"),
		filepath.Join(root, "internal", "config"),
		filepath.Join(root, "a", "b", "c", "d"),
	} {
		got := mustIdentify(t, r, cwd)
		if got.ID != want || !got.Matched {
			t.Errorf("Identify(%q) = %+v, want id %q with Matched true", cwd, got, want)
		}
	}
}

// Acceptance item 7, first half. Longest-prefix is what makes resolution unique
// for a table that (through a hand-written file, or a future format) holds two
// roots one inside the other.
func TestResolutionPicksTheLongestMatchingRoot(t *testing.T) {
	p := testPaths(t)
	outer, inner := derivedID(t, p, "/a"), derivedID(t, p, "/a/b")
	writeProjectsJSON(t, p, `{
  "version": 1,
  "projects": [
    {"id": "`+outer+`", "label": "outer", "root": "/a", "match_mac": "`+derivedMAC(t, p, projectEntry{Root: "/a"})+`"},
    {"id": "`+inner+`", "label": "inner", "root": "/a/b", "match_mac": "`+derivedMAC(t, p, projectEntry{Root: "/a/b"})+`"}
  ]
}
`)
	r := openRepos(t, p)

	for _, c := range []struct {
		cwd  string
		want string
	}{
		{"/a", outer},
		{"/a/x", outer},
		{"/a/b", inner},
		{"/a/b/c", inner},
		{"/a/bc", outer},
	} {
		got := mustIdentify(t, r, c.cwd)
		if got.ID != c.want || !got.Matched {
			t.Errorf("Identify(%q) = %+v, want id %q with Matched true", c.cwd, got, c.want)
		}
	}
}

// Acceptance item 7, second half, and ADR-0019 §5: refusing a nested init is
// what keeps the set of roots mutually non-nested, which is what makes
// longest-prefix resolution unique rather than ambiguous.
func TestRegisteringANestedRootIsRefused(t *testing.T) {
	for _, c := range []struct {
		name      string
		first     []string
		second    []string
		wantOuter bool
	}{
		{"the new root is inside a consented one", []string{"outer"}, []string{"outer", "inner"}, false},
		{"the new root contains a consented one", []string{"outer", "inner"}, []string{"outer"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := testPaths(t)
			base := tempRealDir(t)
			first := mkdirAll(t, filepath.Join(base, filepath.Join(c.first...)))
			second := mkdirAll(t, filepath.Join(base, filepath.Join(c.second...)))
			r := openRepos(t, p)
			mustRegister(t, r, first, "first")
			before := readFileOrFail(t, p.ProjectsFile)

			id, err := r.Register(second, "second", time.Time{})
			var nested *NestedRootError
			if !errors.As(err, &nested) {
				t.Fatalf("Register(%q) = (%q, %v), want a *NestedRootError", second, id, err)
			}
			if nested.Outer != c.wantOuter {
				t.Errorf("NestedRootError.Outer = %v, want %v", nested.Outer, c.wantOuter)
			}
			if nested.EnclosingID == "" {
				t.Error("NestedRootError.EnclosingID is empty; T071 has nothing to resolve a name from")
			}
			if got := readFileOrFail(t, p.ProjectsFile); got != before {
				t.Error("a refused registration rewrote projects.json")
			}
		})
	}
}

// The refusal message travels to a terminal and into bug reports. It names the
// enclosing repository by hashed id only — never its path, any element of it, or
// its label (plan §3.4, constraint 21).
func TestNestedRootErrorNamesOnlyTheHashedID(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	outer := mkdirAll(t, filepath.Join(base, "unmistakable-outer"))
	inner := mkdirAll(t, filepath.Join(outer, "unmistakable-inner"))
	r := openRepos(t, p)
	enclosing := mustRegister(t, r, outer, "unmistakable-label")

	_, err := r.Register(inner, "second", time.Time{})
	if err == nil {
		t.Fatal("Register() = nil, want a *NestedRootError")
	}
	message := err.Error()

	if !strings.Contains(message, enclosing) {
		t.Errorf("the message %q does not name the enclosing id %q", message, enclosing)
	}
	for _, secret := range []string{outer, inner, "unmistakable-outer", "unmistakable-inner", "unmistakable-label"} {
		if strings.Contains(message, secret) {
			t.Errorf("the message %q leaks %q", message, secret)
		}
	}
}

// Acceptance item 5, the half a filesystem-dependent implementation fails: the
// recorded directory may have been moved or deleted long before the log is
// scanned, and the id still has to be the one already written into the store
// (ADR-0004's re-scan safety).
func TestIDIsStableWhenTheFilesystemChanges(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	sub := mkdirAll(t, filepath.Join(root, "sub"))
	r := openRepos(t, p)
	want := mustRegister(t, r, root, "repo")

	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("removing the repository root: %v", err)
	}

	for _, cwd := range []string{root, sub} {
		got := mustIdentify(t, r, cwd)
		if got.ID != want || !got.Matched {
			t.Errorf("Identify(%q) after the directory was deleted = %+v, want id %q with Matched true", cwd, got, want)
		}
	}
}

// Acceptance item 5's "and across processes". All of the state is on disk, so two
// independent Repos values reading the same roots is the honest in-process
// equivalent of two runs.
func TestIDIsStableAcrossOpenRepos(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	want := mustRegister(t, openRepos(t, p), root, "repo")

	for range 3 {
		got := mustIdentify(t, openRepos(t, p), filepath.Join(root, "sub"))
		if got.ID != want || !got.Matched {
			t.Errorf("a second OpenRepos resolved %+v, want id %q with Matched true", got, want)
		}
	}
}

// ADR-0019 §1: derivation never touches the filesystem. Doing so would make the
// id depend on the state of the disk at scan time rather than on the event — the
// alternative the ADR explicitly rejected — so this is the mechanical guard
// against an EvalSymlinks creeping onto the derivation path.
func TestIdentifyTouchesNoFilesystem(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	r := openRepos(t, p)
	id := mustRegister(t, r, root, "repo")

	table := readFileOrFail(t, p.ProjectsFile)
	salt := readFileOrFail(t, p.SaltFile)
	configEntries := dirNames(t, p.ConfigDir)
	dataEntries := dirNames(t, p.DataDir)

	matched := mustIdentify(t, r, filepath.Join(root, "sub"))
	unmatched := mustIdentify(t, r, "/somewhere/never-consented")
	if matched.ID != id || !matched.Matched {
		t.Errorf("Identify(root/sub) = %+v, want id %q with Matched true", matched, id)
	}

	if got := readFileOrFail(t, p.ProjectsFile); got != table {
		t.Error("Identify rewrote projects.json")
	}
	if got := readFileOrFail(t, p.SaltFile); got != salt {
		t.Error("Identify rewrote repo-salt")
	}
	if got := strings.Join(dirNames(t, p.ConfigDir), ","); got != strings.Join(configEntries, ",") {
		t.Errorf("the config root now holds %v, want %v", got, configEntries)
	}
	if got := strings.Join(dirNames(t, p.DataDir), ","); got != strings.Join(dataEntries, ",") {
		t.Errorf("the data root now holds %v, want %v", got, dataEntries)
	}

	// The strong form: with both roots gone, derivation still answers, which it
	// could not do if it read anything from them.
	for _, dir := range []string{p.ConfigDir, p.DataDir} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("removing %s: %v", dir, err)
		}
	}
	if got := mustIdentify(t, r, filepath.Join(root, "sub")); got != matched {
		t.Errorf("Identify with both roots deleted = %+v, want %+v", got, matched)
	}
	if got := mustIdentify(t, r, "/somewhere/never-consented"); got != unmatched {
		t.Errorf("Identify with both roots deleted = %+v, want %+v", got, unmatched)
	}
	for _, dir := range []string{p.ConfigDir, p.DataDir} {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("os.Lstat(%q) = %v, want ErrNotExist — Identify created something", dir, err)
		}
	}
}

// Constraint 18: derivation reports whether it matched a consented root, because
// an unmatched cwd produces a permanently directory-grain record and T103 has to
// be able to count them (ADR-0019 §7, §9). It registers nothing: discovery is an
// explicit step, never a side effect of a read (§9).
func TestUnmatchedCwdFallsBackToItselfAndSaysSo(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	consented := mkdirAll(t, filepath.Join(base, "consented"))
	r := openRepos(t, p)
	mustRegister(t, r, consented, "consented")
	elsewhere := filepath.Join(base, "elsewhere")

	got := mustIdentify(t, r, elsewhere)
	if got.Matched {
		t.Errorf("Identify(%q) = %+v, want Matched false", elsewhere, got)
	}
	if got.ID == "" || len(got.ID) != idHexLen {
		t.Errorf("the fallback id %q is not %d characters", got.ID, idHexLen)
	}

	// The fallback hashes the directory itself, so a later init of that exact
	// directory reuses the identity it already had rather than splitting it
	// (ADR-0019 §9).
	if later := mustRegister(t, r, mkdirAll(t, elsewhere), "elsewhere"); later != got.ID {
		t.Errorf("registering %q afterwards produced %q, want the fallback id %q", elsewhere, later, got.ID)
	}

	table, _, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if len(table.Projects) != 2 {
		t.Errorf("the table holds %d entries, want 2 — a fallback must not register anything", len(table.Projects))
	}
}

// A cwd out of a harness log is a repository path (plan §4.2), so the rejection
// says what is required rather than what it was given.
func TestIdentifyRejectsARelativeCwd(t *testing.T) {
	p := testPaths(t)
	r := openRepos(t, p)

	// The rejected values carry a token no English sentence contains, so the leak
	// check cannot pass by accident and cannot fail because the message happens to
	// contain a word like "directory".
	for _, cwd := range []string{"", "unmistakable/sub", "./unmistakable", "../unmistakable", "~/unmistakable", "unmistakable"} {
		got, err := r.Identify(cwd)
		if err == nil {
			t.Errorf("Identify(%q) = (%+v, nil), want an error", cwd, got)
			continue
		}
		if got != (Identity{}) {
			t.Errorf("Identify(%q) returned %+v alongside an error, want the zero Identity", cwd, got)
		}
		if cwd != "" && strings.Contains(err.Error(), "unmistakable") {
			t.Errorf("the error %q echoes the rejected working directory", err)
		}
	}
}

// Acceptance item 8, symlink half. The harness records the cwd as the process saw
// it, which on macOS is routinely /tmp/x where the real path is /private/tmp/x.
// Both spellings are one repository.
func TestSymlinkedRootAndCanonicalRootShareAnID(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	real := mkdirAll(t, filepath.Join(base, "real"))
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}
	r := openRepos(t, p)

	// Registered through the link, which is the spelling a user would type.
	want := mustRegister(t, r, link, "repo")

	for _, cwd := range []string{link, real, filepath.Join(link, "sub"), filepath.Join(real, "sub")} {
		got := mustIdentify(t, r, cwd)
		if got.ID != want || !got.Matched {
			t.Errorf("Identify(%q) = %+v, want id %q with Matched true", cwd, got, want)
		}
	}

	table, _, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if len(table.Projects) != 1 {
		t.Fatalf("the table holds %d entries, want 1", len(table.Projects))
	}
	if table.Projects[0].Root != real {
		t.Errorf("recorded root = %q, want the canonical %q", table.Projects[0].Root, real)
	}
	if len(table.Projects[0].Aliases) != 1 || table.Projects[0].Aliases[0] != link {
		t.Errorf("recorded aliases = %v, want [%q]", table.Projects[0].Aliases, link)
	}
}

// Acceptance item 8, case half. It is filesystem-dependent by design (ADR-0019
// §5: on ext4, ~/Dev and ~/dev are genuinely different directories), so the test
// probes the filesystem it is running on and asserts the branch that applies.
// A runtime.GOOS switch would be a guess about the filesystem, not a fact.
func TestCaseFoldingFollowsTheFilesystem(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	root := mkdirAll(t, filepath.Join(base, "CaseRepo"))
	flipped := filepath.Join(base, "caserepo")

	folding := false
	if _, err := os.Stat(flipped); err == nil {
		folding = true
	}

	r := openRepos(t, p)
	want := mustRegister(t, r, root, "repo")
	got := mustIdentify(t, r, filepath.Join(flipped, "sub"))

	if folding {
		if got.ID != want || !got.Matched {
			t.Errorf("on a case-insensitive filesystem, Identify(%q) = %+v, want id %q with Matched true", flipped, got, want)
		}
		return
	}
	if got.Matched || got.ID == want {
		t.Errorf("on a case-sensitive filesystem, Identify(%q) = %+v, want an unmatched id different from %q", flipped, got, want)
	}
}

// The same property, both ways, without depending on the filesystem the tests run
// on: folding is driven by what registration recorded, never by a probe on the
// derivation path. Neither root below exists on disk.
func TestCaseFoldingIsDrivenByTheRecordedFlagNotTheDisk(t *testing.T) {
	for _, c := range []struct {
		name            string
		caseInsensitive bool
		wantMatch       bool
	}{
		{"recorded as case-insensitive", true, true},
		{"recorded as case-sensitive", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := testPaths(t)
			id := derivedID(t, p, "/Repo/Path")
			mac := derivedMAC(t, p, projectEntry{Root: "/Repo/Path", CaseInsensitive: c.caseInsensitive})
			writeProjectsJSON(t, p, `{
  "version": 1,
  "projects": [
    {"id": "`+id+`", "label": "repo", "root": "/Repo/Path", "case_insensitive": `+boolText(c.caseInsensitive)+`, "match_mac": "`+mac+`"}
  ]
}
`)
			r := openRepos(t, p)

			got := mustIdentify(t, r, "/repo/path/sub")
			if got.Matched != c.wantMatch {
				t.Errorf("Identify(%q) = %+v, want Matched %v", "/repo/path/sub", got, c.wantMatch)
			}
			if c.wantMatch && got.ID != id {
				t.Errorf("Identify() = %q, want the recorded id %q", got.ID, id)
			}
			// The exact spelling matches either way; folding is only about the
			// other spelling.
			if exact := mustIdentify(t, r, "/Repo/Path/sub"); exact.ID != id || !exact.Matched {
				t.Errorf("Identify(%q) = %+v, want id %q with Matched true", "/Repo/Path/sub", exact, id)
			}
		})
	}
}

// The probe has to answer "does this filesystem fold case", not "does some other
// name reach this directory". A sibling symlink whose name differs from the root
// only in case reaches it on any filesystem, and recording case-insensitivity from
// that would fold two genuinely distinct roots onto one id — the ext4 merge
// ADR-0019 §5 refuses.
//
// The sibling is spelled as the probe's own re-spelling of the root, because that
// is the only other name the probe ever looks at. On a case-insensitive filesystem
// that name is already taken by the root itself, which is the answer rather than a
// failure, so the test asserts whichever branch its filesystem puts it in.
func TestCaseProbeIsNotFooledByASymlinkDifferingOnlyInCase(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	root := mkdirAll(t, filepath.Join(base, "caseprobe"))
	respelled := filepath.Join(base, "caseprobE")

	linkErr := os.Symlink(root, respelled)
	folding := errors.Is(linkErr, os.ErrExist)
	if linkErr != nil && !folding {
		t.Skipf("this filesystem does not support symlinks: %v", linkErr)
	}

	r := openRepos(t, p)
	id := mustRegister(t, r, root, "repo")

	table, _, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if len(table.Projects) != 1 {
		t.Fatalf("the table holds %d entries, want 1", len(table.Projects))
	}
	if got := table.Projects[0].CaseInsensitive; got != folding {
		t.Errorf("recorded case_insensitive = %v, want %v — the probe reported on a symlink rather than on the filesystem", got, folding)
	}

	got := mustIdentify(t, r, filepath.Join(respelled, "sub"))
	if folding {
		if got.ID != id || !got.Matched {
			t.Errorf("on a case-insensitive filesystem, Identify(%q) = %+v, want id %q with Matched true", respelled, got, id)
		}
		return
	}
	if got.Matched || got.ID == id {
		t.Errorf("Identify(%q) = %+v, want an unmatched id different from %q — an unregistered symlink is not a case-folded spelling", respelled, got, id)
	}
}

// Constraint 16 and ADR-0019 §9: the table is append-only and a root once
// recorded is never reassigned, which is what T071's "an already-discovered
// repository keeps its existing identity" rests on.
func TestRegisterIsIdempotentAndKeepsTheExistingIdentity(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	r := openRepos(t, p)

	first := mustRegister(t, r, root, "one")
	second := mustRegister(t, r, root, "two")
	if first != second {
		t.Errorf("re-registering produced %q, want the existing %q", second, first)
	}

	// A second Repos value, because the in-memory table must agree with the file.
	third := mustRegister(t, openRepos(t, p), root, "three")
	if third != first {
		t.Errorf("re-registering through a second Repos produced %q, want %q", third, first)
	}

	table, _, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if len(table.Projects) != 1 {
		t.Fatalf("the table holds %d entries, want 1", len(table.Projects))
	}
	if table.Projects[0].Label != "one" {
		t.Errorf("recorded label = %q, want %q — an existing entry's label is never reassigned", table.Projects[0].Label, "one")
	}
	if table.Projects[0].Root != root {
		t.Errorf("recorded root = %q, want %q", table.Projects[0].Root, root)
	}
}

// Constraint 16 across writers, not only within one. ADR-0019 §9 makes a second
// writer part of the design — under --all the ingest sweep registers roots while
// init (T071) may be registering one — and both Repos values here are opened
// before either writes, which is the interleaving two processes produce.
//
// Writing the whole table from the snapshot taken at open erases whatever the
// other writer recorded in between. The cost is not cosmetic: a consented
// repository missing from the table resolves unmatched, so once T101's consent
// filter lands it collects nothing, with no error — the failure ADR-0019 exists
// to prevent.
func TestRegisterKeepsAnEntryWrittenByAnotherRepos(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	alpha := mkdirAll(t, filepath.Join(base, "alpha"))
	beta := mkdirAll(t, filepath.Join(base, "beta"))

	// Both opened before either writes. Opening the second afterwards is the one
	// interleaving that cannot fail.
	first, second := openRepos(t, p), openRepos(t, p)
	alphaID := mustRegister(t, first, alpha, "alpha")
	betaID := mustRegister(t, second, beta, "beta")
	if alphaID == betaID {
		t.Fatalf("two different roots share the id %q", alphaID)
	}

	table, _, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if len(table.Projects) != 2 {
		t.Fatalf("the table holds %d entries, want 2 — the second writer erased the first's entry", len(table.Projects))
	}

	// What the erasure actually costs, asserted on the path T101 will take.
	fresh := openRepos(t, p)
	for _, c := range []struct{ root, want string }{{alpha, alphaID}, {beta, betaID}} {
		if got := mustIdentify(t, fresh, filepath.Join(c.root, "sub")); got.ID != c.want || !got.Matched {
			t.Errorf("Identify(%q) = %+v, want id %q with Matched true", c.root, got, c.want)
		}
	}

	// The writer that went second merged onto what was on disk, so its own table
	// holds the other's entry too.
	if got := mustIdentify(t, second, filepath.Join(alpha, "sub")); got.ID != alphaID || !got.Matched {
		t.Errorf("the second writer resolves %q as %+v, want id %q with Matched true", alpha, got, alphaID)
	}
}

// envChildRoot carries the root a child process should register. TestMain reads
// it, so a child registers one root and exits instead of running the suite.
const envChildRoot = "WAKE_TEST_REGISTER_ROOT"

// envChildConfigKey and envChildConfigValue carry the setting a child process
// should write, for the config-side counterpart of the test below.
const (
	envChildConfigKey   = "WAKE_TEST_SET_CONFIG_KEY"
	envChildConfigValue = "WAKE_TEST_SET_CONFIG_VALUE"
)

// TestMain gives this package a second entry point for the multi-process
// registration test below. Cross-process exclusion cannot be observed from inside
// one process, and this is the cheapest honest way to have two.
func TestMain(m *testing.M) {
	if key := os.Getenv(envChildConfigKey); key != "" {
		os.Exit(setInChildProcess(key, os.Getenv(envChildConfigValue)))
	}
	if root := os.Getenv(envChildRoot); root != "" {
		os.Exit(registerInChildProcess(root))
	}
	os.Exit(m.Run())
}

// setInChildProcess is the whole child for the config test: resolve the paths the
// parent's HOME points at, write one setting, exit. The parent reports whatever
// reaches stderr.
func setInChildProcess(key, value string) int {
	p, err := ResolvePaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving paths: %v\n", err)
		return 1
	}
	if _, err := Set(p, key, value); err != nil {
		fmt.Fprintf(os.Stderr, "setting %s: %v\n", key, err)
		return 1
	}
	return 0
}

// registerInChildProcess is the whole child: resolve the paths the parent's HOME
// points at, register one root, exit. The parent reports whatever reaches stderr.
func registerInChildProcess(root string) int {
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "registering a root: %v\n", err)
		return 1
	}
	p, err := ResolvePaths()
	if err != nil {
		return fail(err)
	}
	r, err := OpenRepos(p)
	if err != nil {
		return fail(err)
	}
	if _, err := r.Register(root, filepath.Base(root), time.Time{}); err != nil {
		return fail(err)
	}
	return 0
}

// The same property as the test above, across real processes. It is the case that
// matters — a hook firing during a scan, or the --all sweep running while the user
// runs init — and the exclusion that makes it safe is per-process by nature, so
// goroutines in one process prove nothing about it.
//
// The deterministic proof of the read-modify-write hole is the test above; this one
// is the proof that the mechanism holds when the writers are not in the same
// address space. It also exercises the salt's first-run race across processes,
// which in-process goroutines could not: every child's id has to come out of the
// one salt, or the ids below would not resolve.
func TestConcurrentRegistrationsInSeparateProcessesAllSurvive(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)

	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	const children = 4
	roots := make([]string, children)
	output := make([]string, children)
	failures := make([]error, children)

	var wg sync.WaitGroup
	for i := range children {
		roots[i] = mkdirAll(t, filepath.Join(base, fmt.Sprintf("repo-%d", i)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(binary)
			// HOME comes from testPaths, so the child resolves the same roots.
			cmd.Env = append(os.Environ(), envChildRoot+"="+roots[i])
			out, runErr := cmd.CombinedOutput()
			output[i], failures[i] = string(out), runErr
		}()
	}
	wg.Wait()

	for i, err := range failures {
		if err != nil {
			t.Fatalf("child %d exited %v: %s", i, err, output[i])
		}
	}

	table, _, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if len(table.Projects) != children {
		t.Fatalf("the table holds %d entries, want %d — a registration was lost", len(table.Projects), children)
	}

	r := openRepos(t, p)
	ids := make(map[string]bool, children)
	for _, root := range roots {
		got := mustIdentify(t, r, filepath.Join(root, "sub"))
		if !got.Matched {
			t.Errorf("Identify(%q) = %+v, want Matched true", root, got)
		}
		ids[got.ID] = true
	}
	if len(ids) != children {
		t.Errorf("%d roots resolved to %d distinct ids; every child hashed under a different salt", children, len(ids))
	}
}

// Registering through a new spelling of a root that is already recorded is
// additive: the alias is appended and the id, the root and the label stay put.
func TestRegisterAppendsAnAliasWithoutReassigningTheRoot(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	real := mkdirAll(t, filepath.Join(base, "real"))
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}
	r := openRepos(t, p)

	// Canonical first, so the alias is genuinely appended to an existing entry
	// rather than recorded when it was created.
	want := mustRegister(t, r, real, "repo")
	if got := mustRegister(t, r, link, "other-label"); got != want {
		t.Errorf("registering the link produced %q, want the existing %q", got, want)
	}

	table, _, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if len(table.Projects) != 1 {
		t.Fatalf("the table holds %d entries, want 1", len(table.Projects))
	}
	entry := table.Projects[0]
	if entry.ID != want || entry.Root != real || entry.Label != "repo" {
		t.Errorf("entry = %+v, want id %q, root %q and label %q unchanged", entry, want, real, "repo")
	}
	if len(entry.Aliases) != 1 || entry.Aliases[0] != link {
		t.Errorf("aliases = %v, want [%q]", entry.Aliases, link)
	}

	// Registering the same spelling twice does not append it twice.
	mustRegister(t, r, link, "repo")
	table, _, err = readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if got := len(table.Projects[0].Aliases); got != 1 {
		t.Errorf("aliases = %v, want exactly one", table.Projects[0].Aliases)
	}

	for _, cwd := range []string{filepath.Join(real, "sub"), filepath.Join(link, "sub")} {
		if got := mustIdentify(t, r, cwd); got.ID != want || !got.Matched {
			t.Errorf("Identify(%q) = %+v, want id %q with Matched true", cwd, got, want)
		}
	}
}

// ADR-0019 §5: nesting is refused against recorded roots and aliases in both
// directions, "so consent is still checked against every spelling a root answers
// to". An alias is a recorded spelling, so appending one that sits inside another
// consented root breaks the invariant refusing nesting exists to keep: the
// recorded spellings stop being mutually non-nested, and work under that subtree
// would be attributed to the aliased repository rather than to the one the user
// consented to for it.
//
// The counter-argument is that the directory genuinely is the aliased repository.
// The ADR wins on visibility: a refused init is something the user sees and can
// act on, and a silent mis-attribution is not — which is this package's posture
// everywhere else.
func TestRegisteringAnAliasInsideAnotherConsentedRootIsRefused(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	target := mkdirAll(t, filepath.Join(base, "target"))
	other := mkdirAll(t, filepath.Join(base, "other"))
	link := filepath.Join(other, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}

	r := openRepos(t, p)
	targetID := mustRegister(t, r, target, "target")
	otherID := mustRegister(t, r, other, "other")
	before := readFileOrFail(t, p.ProjectsFile)

	// The canonical root of link is target, which is already recorded — the path
	// that returns the existing id and appends a spelling to it.
	id, err := r.Register(link, "via-link", time.Time{})
	var nested *NestedRootError
	if !errors.As(err, &nested) {
		t.Fatalf("Register(%q) = (%q, %v), want a *NestedRootError", link, id, err)
	}
	if nested.EnclosingID != otherID {
		t.Errorf("NestedRootError.EnclosingID = %q, want the enclosing repository %q", nested.EnclosingID, otherID)
	}
	if nested.Outer {
		t.Error("NestedRootError.Outer = true, want false — the offered spelling is inside the recorded root")
	}
	if got := readFileOrFail(t, p.ProjectsFile); got != before {
		t.Error("a refused registration rewrote projects.json")
	}

	// The visible half of the refusal: the subtree keeps resolving to the root that
	// lexically contains it, so nothing is silently reattributed to target while the
	// user is being told to pick which root they meant.
	if got := mustIdentify(t, r, filepath.Join(link, "sub")); got.ID != otherID || !got.Matched {
		t.Errorf("Identify(%q) = %+v, want the enclosing id %q with Matched true", link, got, otherID)
	}
	if got := mustIdentify(t, r, filepath.Join(target, "sub")); got.ID != targetID || !got.Matched {
		t.Errorf("Identify(%q) = %+v, want id %q with Matched true", target, got, targetID)
	}
}

// Fail closed on both writing paths (plan §3.4): an entry readProjects would drop
// is worse than a refusal, because the registration would look successful and the
// repository would collect nothing. Register cannot reach this — every spelling it
// passes down has been through lexicalClean — which is why the check is asserted
// at the seam where it can be.
func TestRegistrationRefusesAnEntryItCouldNotReadBack(t *testing.T) {
	p := testPaths(t)
	r := openRepos(t, p)
	recorded := projectEntry{ID: hexID('a'), Label: "repo", Root: "/repo"}

	for _, c := range []struct {
		name  string
		table projectsFile
	}{
		{"a new entry", projectsFile{Version: projectsVersion}},
		{"an alias appended to a recorded entry", projectsFile{Version: projectsVersion, Projects: []projectEntry{recorded}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, id, changed, err := r.registration(c.table, "/repo", []string{"not-absolute"}, "repo", false, time.Time{})
			if !errors.Is(err, errUnreadableEntry) {
				t.Fatalf("registration() = (%q, %v, %v), want errUnreadableEntry", id, changed, err)
			}
			if changed {
				t.Error("registration() reported a change alongside its refusal")
			}
		})
	}
}

// A label is display-only and must not start looking like the path it must never
// be (plan §3.4). The rejection does not echo it, because a label is a repository
// name.
func TestRegisterRefusesALabelWithAPathSeparator(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	r := openRepos(t, p)

	for _, label := range []string{"", "unmistakable/label", "/unmistakable", "unmistakable" + string(filepath.Separator)} {
		id, err := r.Register(root, label, time.Time{})
		var invalid *InvalidValueError
		if !errors.As(err, &invalid) {
			t.Errorf("Register(%q) = (%q, %v), want an *InvalidValueError", label, id, err)
			continue
		}
		if label != "" && strings.Contains(err.Error(), label) {
			t.Errorf("the error %q echoes the label", err)
		}
		if _, err := os.Lstat(p.ProjectsFile); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("os.Lstat(projects.json) = %v, want ErrNotExist — a refused registration writes nothing", err)
		}
	}
}

// Registration is the one place in the package that resolves symlinks and probes
// case sensitivity, and both need the directory to be there — it runs inside a
// live repository (T071). A missing directory is an error rather than a recorded
// root nothing can be resolved against.
func TestRegisterRequiresTheDirectoryToExist(t *testing.T) {
	p := testPaths(t)
	missing := filepath.Join(tempRealDir(t), "unmistakable-missing")
	r := openRepos(t, p)

	id, err := r.Register(missing, "repo", time.Time{})
	if err == nil {
		t.Fatalf("Register(%q) = (%q, nil), want an error", missing, id)
	}
	for _, secret := range []string{missing, "unmistakable-missing"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error %q leaks %q", err, secret)
		}
	}
	if _, err := os.Lstat(p.ProjectsFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Lstat(projects.json) = %v, want ErrNotExist", err)
	}
}

// A relative root is refused for the same reason a relative cwd is: the id must
// not depend on where the binary happened to be invoked from.
func TestRegisterRejectsARelativeRoot(t *testing.T) {
	p := testPaths(t)
	r := openRepos(t, p)

	for _, root := range []string{"", "relative/dir", "./dir", "~/dir"} {
		if id, err := r.Register(root, "repo", time.Time{}); err == nil {
			t.Errorf("Register(%q) = (%q, nil), want an error", root, id)
		}
	}
}

// ADR-0019 §5: a directory that is not a git repository is accepted as its own
// root. There is no ambiguity to resolve — the user consented to that exact
// directory — and agent usage outside a repository is still usage.
func TestANonGitDirectoryIsAcceptedAsItsOwnRoot(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "not-a-repo"))
	r := openRepos(t, p)

	id := mustRegister(t, r, root, "not-a-repo")
	if len(id) != idHexLen {
		t.Errorf("id %q is %d characters, want %d", id, len(id), idHexLen)
	}
	if got := mustIdentify(t, r, filepath.Join(root, "sub")); got.ID != id || !got.Matched {
		t.Errorf("Identify() = %+v, want id %q with Matched true", got, id)
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the test directory unexpectedly contains .git")
	}
}

// Acceptance item 3, on the path a user actually takes. projects_test.go asserts
// the mode writeProjects produces; this asserts that registering a repository is
// what goes through it, since Register is the only caller a user reaches.
func TestRegisterWritesTheTableAt0600(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))

	mustRegister(t, openRepos(t, p), root, "repo")

	fi, err := os.Stat(p.ProjectsFile)
	if err != nil {
		t.Fatalf("stat projects.json: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("projects.json mode = %#o, want 0600", perm)
	}
	di, err := os.Stat(p.DataDir)
	if err != nil {
		t.Fatalf("stat the data root: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("data root mode = %#o, want 0700", perm)
	}
}

// A sibling directory whose name merely starts with a consented root's name is
// not inside it. Prefix matching on raw strings is the classic way this goes
// wrong, and it would attribute one repository's work to another.
func TestASiblingWithASharedNamePrefixIsNotInsideTheRoot(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	root := mkdirAll(t, filepath.Join(base, "repo"))
	sibling := mkdirAll(t, filepath.Join(base, "repo-two"))
	r := openRepos(t, p)
	want := mustRegister(t, r, root, "repo")

	got := mustIdentify(t, r, sibling)
	if got.Matched || got.ID == want {
		t.Errorf("Identify(%q) = %+v, want an unmatched id different from %q", sibling, got, want)
	}
}

// The dropped count is how a silently shrinking table becomes visible (doctor,
// T103). OpenRepos carries it because that is where the table is read.
func TestDroppedEntriesIsReported(t *testing.T) {
	p := testPaths(t)
	writeProjectsJSON(t, p, `{
  "version": 1,
  "projects": [
    {"id": "`+derivedID(t, p, "/a")+`", "label": "good", "root": "/a", "match_mac": "`+derivedMAC(t, p, projectEntry{Root: "/a"})+`"},
    {"id": "nothex", "label": "bad", "root": "/b"},
    {"id": "`+hexID('c')+`", "label": "bad", "root": "relative"}
  ]
}
`)

	if got := openRepos(t, p).DroppedEntries(); got != 2 {
		t.Errorf("DroppedEntries() = %d, want 2", got)
	}
}

// An unreadable table is not an empty table: resolving against it would hand
// every repository a new identity on the next scan.
func TestOpenReposFailsOnAnUnreadableTable(t *testing.T) {
	p := testPaths(t)
	writeProjectsJSON(t, p, "{\"version\": 1, \"projects\": [ trailing\n")

	if r, err := OpenRepos(p); err == nil {
		t.Fatalf("OpenRepos() = (%+v, nil), want an error", r)
	}
}

func TestLexicalCleanNormalizesWithoutTouchingTheDisk(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{"/a/b", "/a/b"},
		{"/a/b/", "/a/b"},
		{"/a//b", "/a/b"},
		{"/a/./b", "/a/b"},
		{"/a/b/../c", "/a/c"},
		{"/", "/"},
	} {
		got, err := lexicalClean(c.in)
		if err != nil {
			t.Errorf("lexicalClean(%q) = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("lexicalClean(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	for _, in := range []string{"", "a", "./a", "../a", "~/a"} {
		if got, err := lexicalClean(in); err == nil {
			t.Errorf("lexicalClean(%q) = (%q, nil), want an error", in, got)
		}
	}
}

// The re-spelling has to land in the final path element, because that is the
// directory whose filesystem the probe is asking about: a mount point one element
// up can be a different filesystem with a different answer, and the answer decides
// whether two spellings of one root become one id or two (ADR-0019 §5). Walking up
// is the fallback for an element with no ASCII letter to flip (/mnt/vol/2024), and
// a path with none at all reports case-sensitive, which keeps spellings apart
// rather than merging them.
func TestFlipCaseOfLastElement(t *testing.T) {
	for _, c := range []struct {
		in    string
		want  string
		found bool
	}{
		{"/a/repo", "/a/repO", true},
		{"/a/Repo", "/a/RepO", true},
		{"/a/REPO", "/a/REPo", true},
		{"/x/b-9", "/x/B-9", true},
		{"/a", "/A", true},
		// The final element has no letter, so the nearest ancestor that has one
		// answers instead.
		{"/mnt/vol/2024", "/mnt/voL/2024", true},
		{"/mnt/2024/1/2", "/mnT/2024/1/2", true},
		// Nothing to flip anywhere: reported as case-sensitive, unchanged.
		{"/2024/2025", "/2024/2025", false},
		{"/", "/", false},
	} {
		got, found := flipCaseOfLastElement(c.in)
		if got != c.want || found != c.found {
			t.Errorf("flipCaseOfLastElement(%q) = (%q, %v), want (%q, %v)", c.in, got, found, c.want, c.found)
		}
	}
}

func TestHasPathPrefix(t *testing.T) {
	for _, c := range []struct {
		cwd  string
		root string
		fold bool
		want bool
	}{
		{"/a/b", "/a", false, true},
		{"/a", "/a", false, true},
		{"/ab", "/a", false, false},
		{"/a-b", "/a", false, false},
		{"/a/b/c", "/a/b", false, true},
		{"/a/b", "/", false, true},
		{"/", "/", false, true},
		{"/A/b", "/a", false, false},
		{"/A/b", "/a", true, true},
		{"/a/B", "/A/b", true, true},
		{"/x", "/a", true, false},
	} {
		if got := hasPathPrefix(c.cwd, c.root, c.fold); got != c.want {
			t.Errorf("hasPathPrefix(%q, %q, %v) = %v, want %v", c.cwd, c.root, c.fold, got, c.want)
		}
	}
}

// dirNames lists a directory's entries, sorted, treating a missing directory as
// empty so a test can compare "before" against "after" without caring which.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestNameKeyIsAStableSubkeyOfTheSaltAndNotTheSalt(t *testing.T) {
	paths := testPaths(t)
	repos, err := OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}

	key := repos.NameKey()
	if len(key) == 0 {
		t.Fatal("NameKey() returned no key")
	}
	salt, err := os.ReadFile(paths.SaltFile)
	if err != nil {
		t.Fatalf("reading the salt: %v", err)
	}
	// Handing out the salt itself would let the record layer compute or confirm a
	// repository id. The subkey is domain-separated so it cannot.
	if string(key) == string(salt) {
		t.Fatal("NameKey() returned the per-machine salt")
	}

	reopened, err := OpenRepos(paths)
	if err != nil {
		t.Fatalf("second OpenRepos() error = %v", err)
	}
	if string(reopened.NameKey()) != string(key) {
		t.Fatal("NameKey() is not stable across opens of the same salt")
	}

	other, err := OpenRepos(testPaths(t))
	if err != nil {
		t.Fatalf("OpenRepos() on a second machine state error = %v", err)
	}
	if string(other.NameKey()) == string(key) {
		t.Fatal("NameKey() does not depend on the salt")
	}
}

// The three keyed digests this package derives, pinned against the construction
// written out longhand. T119 moved the HMAC step into internal/keyeddigest, and
// the one thing that refactor may not do is change a byte: an id or a match
// digest that shifts silently stops resolving every entry already in
// projects.json, and a shifted NameKey re-keys every scope digest already in the
// spool (ADR-0019 §3, §8, ADR-0020, ADR-0022).
//
// crypto/hmac is spelled out here rather than calling the shared helper on
// purpose: recomputing the value through the helper would assert only that the
// helper equals itself.
func TestTheKeyedDigestSitesMatchTheConstructionWrittenLonghand(t *testing.T) {
	paths := testPaths(t)
	repos := openRepos(t, paths)
	salt, err := os.ReadFile(paths.SaltFile)
	if err != nil {
		t.Fatalf("reading the salt: %v", err)
	}

	longhand := func(key, data []byte) []byte {
		mac := hmac.New(sha256.New, key)
		if _, writeErr := mac.Write(data); writeErr != nil {
			t.Fatalf("writing to the MAC: %v", writeErr)
		}
		return mac.Sum(nil)
	}

	// NameKey is the raw, unencoded, untruncated MAC: record.NewNamer consumes it
	// as an HMAC key, so encoding it would be a different value.
	if got, want := repos.NameKey(), longhand(salt, []byte(nameKeyDomain)); !hmac.Equal(got, want) {
		t.Errorf("NameKey() = %x, want %x", got, want)
	}

	const root = "/some/consented/root"
	if got, want := repos.hashRoot(root), hex.EncodeToString(longhand(salt, []byte(root)))[:idHexLen]; got != want {
		t.Errorf("hashRoot() = %q, want %q", got, want)
	}

	// matchMAC's NUL-separated input is rebuilt rather than reused: the digest is
	// persisted in projects.json, so a change to what it covers has to be a
	// deliberate edit here as well as there.
	entry := projectEntry{
		Root:            root,
		Aliases:         []string{"/private/some/consented/root"},
		CaseInsensitive: true,
		CollectFrom:     "2026-08-19T12:00:00Z",
	}
	buf := append([]byte(matchMACDomain), 0)
	buf = append(buf, 'f', 0)
	for _, spelling := range entry.spellings() {
		buf = append(buf, spelling...)
		buf = append(buf, 0)
	}
	// The boundary sits last and is always terminated, including when there is none:
	// that is what makes a stripped boundary a different input rather than the same
	// one (ADR-0025, matchMAC).
	legacy := hex.EncodeToString(longhand(salt, buf))
	buf = append(buf, entry.CollectFrom...)
	buf = append(buf, 0)
	got := repos.matchMAC(entry)
	if want := hex.EncodeToString(longhand(salt, buf)); got != want {
		t.Errorf("matchMAC() = %q, want %q", got, want)
	}
	// And the construction from before the boundary joined the digest is exactly the
	// input without it, which is the whole of what accepting it on read admits.
	unbounded := entry
	unbounded.CollectFrom = ""
	if before := repos.legacyMatchMAC(unbounded); before != legacy {
		t.Errorf("legacyMatchMAC() = %q, want %q", before, legacy)
	}
	if repos.matchMAC(unbounded) == legacy {
		t.Error("matchMAC() equals the legacy construction for an unbounded entry; a stripped boundary would then verify")
	}
	// Deliberately not truncated, unlike the id.
	if len(got) != sha256.Size*2 {
		t.Errorf("matchMAC() is %d characters, want the full %d", len(got), sha256.Size*2)
	}
}

// A table whose digests were derived by the construction written out longhand —
// which is what every build before T119 wrote — still resolves.
//
// readProjects verifies each entry's id and match digest against freshly derived
// values and refuses the entry when either differs (trustworthy, ADR-0019 §7), so
// this is the whole compatibility question in one assertion: had the refactor
// shifted a digest by one byte, the entry would be dropped, the directory would
// hash as itself with Matched false, and every repository already in projects.json
// would silently stop being recognised on upgrade.
func TestATableDigestedByTheLonghandConstructionStillResolves(t *testing.T) {
	p := testPaths(t)
	// Opened once so the salt exists, then read directly: the expected digests must
	// come from the salt rather than from the code under test.
	openRepos(t, p)
	salt, err := os.ReadFile(p.SaltFile)
	if err != nil {
		t.Fatalf("reading the salt: %v", err)
	}
	longhand := func(data []byte) []byte {
		mac := hmac.New(sha256.New, salt)
		if _, writeErr := mac.Write(data); writeErr != nil {
			t.Fatalf("writing to the MAC: %v", writeErr)
		}
		return mac.Sum(nil)
	}

	const root = "/a/longhand"
	id := hex.EncodeToString(longhand([]byte(root)))[:idHexLen]
	buf := append([]byte(matchMACDomain), 0, 0)
	buf = append(buf, root...)
	buf = append(buf, 0)
	mac := hex.EncodeToString(longhand(buf))

	writeProjectsJSON(t, p, fmt.Sprintf(`{
  "version": 1,
  "projects": [
    {"id": %q, "label": "longhand", "root": %q, "case_insensitive": false, "match_mac": %q}
  ]
}
`, id, root, mac))

	r := openRepos(t, p)
	if dropped := r.DroppedEntries(); dropped != 0 {
		t.Fatalf("DroppedEntries() = %d, want 0 — this build refused a table the longhand construction signed", dropped)
	}
	identity, err := r.Identify(root + "/pkg")
	if err != nil {
		t.Fatalf("Identify() error = %v", err)
	}
	if !identity.Matched || identity.ID != id {
		t.Fatalf("Identify() = %+v, want the recorded id %s matched", identity, id)
	}
}

func TestConsentedRootAnswersWithTheRecordedRoot(t *testing.T) {
	paths := testPaths(t)
	repos, err := OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	if _, registerErr := repos.Register(root, "repo", time.Time{}); registerErr != nil {
		t.Fatalf("Register() error = %v", registerErr)
	}

	for _, cwd := range []string{root, filepath.Join(root, "packages", "api")} {
		got, rootErr := repos.ConsentedRoot(cwd)
		if rootErr != nil {
			t.Fatalf("ConsentedRoot(%q) error = %v", cwd, rootErr)
		}
		if got != root {
			t.Errorf("ConsentedRoot(%q) = %q, want %q", cwd, got, root)
		}
	}

	outside := mkdirAll(t, filepath.Join(tempRealDir(t), "elsewhere"))
	got, err := repos.ConsentedRoot(outside)
	if err != nil {
		t.Fatalf("ConsentedRoot() error = %v", err)
	}
	if got != "" {
		t.Errorf("ConsentedRoot() on an unconsented directory = %q, want no root", got)
	}
	if _, err := repos.ConsentedRoot("relative/dir"); err == nil {
		t.Error("ConsentedRoot() accepted a relative directory")
	}
}

// The config file's own multi-process case, and the interleaving half of the
// ticket's acceptance. Set republishes the whole file from a fresh decode, so two
// processes writing two different keys is exactly the shape that loses one — and
// the exclusion that prevents it is per-process by nature, so goroutines in one
// address space prove nothing about it.
func TestConcurrentConfigWritesInSeparateProcessesAllSurvive(t *testing.T) {
	p := testPaths(t)

	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	writes := [][2]string{
		{"store.retention_raw", "90d"},
		{"store.rollup_after", "7d"},
		{"ui.default_window", "14d"},
		{"session.idle_timeout", "45m"},
	}
	output := make([]string, len(writes))
	failures := make([]error, len(writes))

	var wg sync.WaitGroup
	for i, write := range writes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(binary)
			// HOME comes from testPaths, so the child writes the parent's config.
			cmd.Env = append(os.Environ(), envChildConfigKey+"="+write[0], envChildConfigValue+"="+write[1])
			out, runErr := cmd.CombinedOutput()
			output[i], failures[i] = string(out), runErr
		}()
	}
	wg.Wait()

	for i, err := range failures {
		if err != nil {
			t.Fatalf("child %d exited %v: %s", i, err, output[i])
		}
	}

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	for _, write := range writes {
		got, getErr := c.Get(write[0])
		if getErr != nil {
			t.Errorf("Get(%q) = %v", write[0], getErr)
			continue
		}
		if got != write[1] {
			t.Errorf("Get(%q) = %q, want %q — another process's write erased it", write[0], got, write[1])
		}
	}
}

// mustRegisterFrom registers root with a recorded collection boundary. It is
// separate from mustRegister rather than a parameter on it, because every test that
// is about identity registers with no boundary and reads better saying so once.
func mustRegisterFrom(t *testing.T, r *Repos, root, label string, from time.Time) string {
	t.Helper()
	id, err := r.Register(root, label, from)
	if err != nil {
		t.Fatalf("Register(%q, %q, %s) = %v", root, label, from, err)
	}
	return id
}

// The boundary a plain `wake init` records (ADR-0024, ADR-0025): collection begins
// at the instant consent was given, and the fact has to outlive the process that
// recorded it, because the scan that has to honour it runs from a hook in a process
// that was not there when the user consented.
func TestRegisterRecordsTheInstantCollectionBegins(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	from := time.Now().UTC()

	r := openRepos(t, p)
	id := mustRegisterFrom(t, r, root, "repo", from)

	if got := r.CollectsFrom(id); !got.Equal(from) {
		t.Errorf("CollectsFrom() = %s, want the recorded %s", got, from)
	}
	// Read back through a second open, which is what the trigger's process does.
	// A boundary that only the writing call can see is not a boundary.
	reopened := openRepos(t, p)
	if got := reopened.CollectsFrom(id); !got.Equal(from) {
		t.Errorf("CollectsFrom() after reopening = %s, want %s", got, from)
	}
	// The entry still resolves: the boundary is covered by the match digest, so an
	// entry carrying one has to verify with it.
	if dropped := reopened.DroppedEntries(); dropped != 0 {
		t.Errorf("DroppedEntries() = %d, want 0 — the entry it just wrote does not verify", dropped)
	}
	if identity := mustIdentify(t, reopened, root); !identity.Matched || identity.ID != id {
		t.Errorf("Identify() = %+v, want the recorded id %s matched", identity, id)
	}
}

// `wake init --full` records no boundary at all rather than a boundary in the past:
// the user asked for the whole history, and there is no instant that says so. Every
// table written before the boundary existed says the same thing by saying nothing,
// which is why unbounded has to be the meaning of absent.
func TestRegisterRecordsNoBoundaryForAFullImport(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))

	r := openRepos(t, p)
	id := mustRegister(t, r, root, "repo")

	if got := r.CollectsFrom(id); !got.IsZero() {
		t.Errorf("CollectsFrom() = %s, want the zero time — an unbounded repository", got)
	}
	if raw := readFileOrFail(t, p.ProjectsFile); strings.Contains(raw, "collect_from") {
		t.Errorf("projects.json = %s; a repository with no boundary records no field", raw)
	}
}

// A second plain `init` in a repository already consented leaves the boundary where
// it is. Moving it forward would skip everything collected between the two calls,
// and the instant the user consented is the one the disclosure was about.
func TestASecondRegistrationNeverMovesTheBoundaryForward(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	first := time.Now().UTC().Add(-time.Hour)

	r := openRepos(t, p)
	id := mustRegisterFrom(t, r, root, "repo", first)
	if again := mustRegisterFrom(t, r, root, "repo", time.Now().UTC()); again != id {
		t.Fatalf("Register() = %q, want the id it already had %q", again, id)
	}

	if got := openRepos(t, p).CollectsFrom(id); !got.Equal(first) {
		t.Errorf("CollectsFrom() = %s, want the boundary the first registration recorded %s", got, first)
	}
}

// `wake init --full` on a repository consented forward-only clears the boundary: the
// user has asked for the whole history, so there is nothing left for a later scan to
// hold back. It is the one edit an existing entry's boundary allows, and the entry is
// re-signed, so it still resolves afterwards.
func TestAFullImportClearsARecordedBoundary(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))

	r := openRepos(t, p)
	id := mustRegisterFrom(t, r, root, "repo", time.Now().UTC())
	if again := mustRegister(t, r, root, "repo"); again != id {
		t.Fatalf("Register() = %q, want the id it already had %q", again, id)
	}

	reopened := openRepos(t, p)
	if got := reopened.CollectsFrom(id); !got.IsZero() {
		t.Errorf("CollectsFrom() = %s, want the zero time — the full import cleared it", got)
	}
	if dropped := reopened.DroppedEntries(); dropped != 0 {
		t.Errorf("DroppedEntries() = %d, want 0 — clearing the boundary left an entry that does not verify", dropped)
	}
	if raw := readFileOrFail(t, p.ProjectsFile); strings.Contains(raw, "collect_from") {
		t.Errorf("projects.json = %s; the cleared boundary is still recorded", raw)
	}
}

// A boundary this build cannot read is refused, never resolved as unbounded.
// Reading a broken value as "collect everything" would widen what the next scan
// imports on the strength of a file that failed to parse, which is the direction
// fail-closed exists to rule out (plan §3.4).
func TestAnEntryWhoseBoundaryThisBuildCannotReadIsRefused(t *testing.T) {
	for _, boundary := range []string{
		"yesterday",
		"2026-08-19",
		// Parseable, but not the spelling this package writes: the digest was
		// computed over what was recorded, so a value that needs normalising is one
		// the digest no longer follows from.
		"2026-08-19T12:00:00+02:00",
		"0001-01-01T00:00:00Z",
	} {
		t.Run(boundary, func(t *testing.T) {
			p := testPaths(t)
			entry := projectEntry{ID: derivedID(t, p, "/a"), Label: "repo", Root: "/a", CollectFrom: boundary}
			writeProjectsJSON(t, p, `{
  "version": 1,
  "projects": [
    {"id": "`+entry.ID+`", "label": "repo", "root": "/a", "collect_from": "`+boundary+`", "match_mac": "`+derivedMAC(t, p, entry)+`"}
  ]
}
`)

			r := openRepos(t, p)
			if dropped := r.DroppedEntries(); dropped != 1 {
				t.Errorf("DroppedEntries() = %d, want 1 — a boundary this build cannot read must refuse the entry", dropped)
			}
			if identity := mustIdentify(t, r, "/a/pkg"); identity.Matched {
				t.Errorf("Identify() = %+v, want an unmatched identity", identity)
			}
		})
	}
}

// The boundary is covered by the match digest, so deleting it by hand refuses the
// entry instead of widening the next scan.
//
// This is the whole reason it is in the digest. An entry's id covers its root alone,
// so without this a `collect_from` could be stripped out of a 0600 file and the next
// trigger would import every event the repository declined — no error, no counter, and
// a disclosure that had already said it would not happen.
func TestABoundaryDeletedByHandRefusesTheEntry(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	id := mustRegisterFrom(t, openRepos(t, p), root, "repo", time.Now().UTC())

	recorded := readFileOrFail(t, p.ProjectsFile)
	var table projectsFile
	if err := json.Unmarshal([]byte(recorded), &table); err != nil {
		t.Fatalf("re-reading the recorded table: %v", err)
	}
	if len(table.Projects) != 1 || table.Projects[0].CollectFrom == "" {
		t.Fatalf("projects.json = %s, want one entry carrying a boundary", recorded)
	}
	// Only the boundary is removed. Every other byte, the recorded digest included,
	// is the one this build wrote.
	stripped := table
	stripped.Projects = []projectEntry{table.Projects[0]}
	stripped.Projects[0].CollectFrom = ""
	edited, err := json.MarshalIndent(stripped, "", "  ")
	if err != nil {
		t.Fatalf("re-encoding the edited table: %v", err)
	}
	writeProjectsJSON(t, p, string(edited)+"\n")

	r := openRepos(t, p)
	if dropped := r.DroppedEntries(); dropped != 1 {
		t.Errorf("DroppedEntries() = %d, want 1 — a boundary removed by hand must refuse the entry", dropped)
	}
	// A directory inside the refused root, so the unmatched answer is visibly not the
	// recorded id: the root's own unmatched hash is that id by construction (ADR-0019
	// §9), and asserting on it would prove nothing either way.
	if identity := mustIdentify(t, r, filepath.Join(root, "pkg")); identity.Matched || identity.ID == id {
		t.Errorf("Identify() = %+v, want an unmatched identity different from %q", identity, id)
	}
}
