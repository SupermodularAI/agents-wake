package config

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setGlobalRoot records a boundary the test needs recorded, so a case about what
// happens afterwards does not spend three lines saying so.
func setGlobalRoot(t *testing.T, r *Repos, root string) {
	t.Helper()
	if err := r.SetGlobalRoot(root); err != nil {
		t.Fatalf("SetGlobalRoot(%q) = %v", root, err)
	}
}

// rewriteProjectsJSON edits the table on disk through a generic decode, so a case can
// change one recorded field and leave every digest exactly as it was. That is the
// hand-edit the digests exist to catch, and writeProjects would never produce it.
func rewriteProjectsJSON(t *testing.T, p Paths, edit func(table map[string]any)) {
	t.Helper()
	var table map[string]any
	if err := json.Unmarshal([]byte(readFileOrFail(t, p.ProjectsFile)), &table); err != nil {
		t.Fatalf("decoding the project table: %v", err)
	}
	edit(table)
	raw, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		t.Fatalf("encoding the project table: %v", err)
	}
	writeProjectsJSON(t, p, string(raw)+"\n")
}

// rawProjectsTable decodes the file as it is on disk, for the assertions that are
// about the bytes rather than about what this build resolves.
func rawProjectsTable(t *testing.T, p Paths) map[string]any {
	t.Helper()
	var table map[string]any
	if err := json.Unmarshal([]byte(readFileOrFail(t, p.ProjectsFile)), &table); err != nil {
		t.Fatalf("decoding the project table: %v", err)
	}
	return table
}

// The boundary survives the process that recorded it, which is the whole point: the
// scan that has to honour it runs from a hook in a process that was never told.
func TestSetGlobalRootRecordsAVerifiableBoundary(t *testing.T) {
	p := testPaths(t)
	boundary := mkdirAll(t, filepath.Join(tempRealDir(t), "boundary"))

	setGlobalRoot(t, openRepos(t, p), boundary)

	reopened := openRepos(t, p)
	if got := reopened.GlobalRootState(); got != (GlobalRootState{Set: true}) {
		t.Errorf("GlobalRootState() = %+v, want a set boundary with nothing discovered", got)
	}
	if !reopened.WithinGlobalRoot(filepath.Join(boundary, "project")) {
		t.Error("WithinGlobalRoot(a directory under the boundary) = false")
	}
}

// Acceptance item 5. A boundary is consent for everything under it, so a hand-edited
// one is a consent widening, and the digest is what refuses it. Refused means absent:
// nothing under the widened path is inside a boundary this build honours.
func TestAHandEditedGlobalRootIsRefusedAndTreatedAsAbsent(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	boundary := mkdirAll(t, filepath.Join(base, "boundary"))
	sibling := mkdirAll(t, filepath.Join(base, "sibling"))

	setGlobalRoot(t, openRepos(t, p), boundary)
	rewriteProjectsJSON(t, p, func(table map[string]any) {
		entry, ok := table["global_root"].(map[string]any)
		if !ok {
			t.Fatalf("the recorded table holds no global_root: %v", table)
		}
		entry["root"] = base
	})

	reopened := openRepos(t, p)
	if got := reopened.GlobalRootState(); got != (GlobalRootState{Refused: true}) {
		t.Errorf("GlobalRootState() = %+v, want the widened boundary refused and absent", got)
	}
	if reopened.WithinGlobalRoot(sibling) {
		t.Error("WithinGlobalRoot(a directory the widened boundary would enclose) = true; the hand edit took effect")
	}
}

// The posture Register already takes for an entry, applied to the boundary: one this
// build refuses to honour is one it refuses to carry back into the file.
func TestARefusedGlobalRootIsNotCarriedBackIntoTheFile(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	boundary := mkdirAll(t, filepath.Join(base, "boundary"))

	setGlobalRoot(t, openRepos(t, p), boundary)
	rewriteProjectsJSON(t, p, func(table map[string]any) {
		table["global_root"].(map[string]any)["root"] = base
	})

	r := openRepos(t, p)
	mustRegister(t, r, mkdirAll(t, filepath.Join(base, "repo")), "repo")

	if _, present := rawProjectsTable(t, p)["global_root"]; present {
		t.Errorf("global_root survived a republication: %s", readFileOrFail(t, p.ProjectsFile))
	}
}

// The boundary encloses roots and is never one (ADR-0032 §1, §7). It is a top-level
// field rather than an entry precisely so nestedWith cannot see it and resolution
// cannot match against it.
func TestTheGlobalRootIsNeverAnEntryInTheResolutionTable(t *testing.T) {
	p := testPaths(t)
	boundary := mkdirAll(t, filepath.Join(tempRealDir(t), "boundary"))

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	if identity := mustIdentify(t, r, boundary); identity.Matched {
		t.Errorf("Identify(the boundary) matched %s; a boundary is not an identity", identity.ID)
	}
	projects, ok := rawProjectsTable(t, p)["projects"].([]any)
	if !ok || len(projects) != 0 {
		t.Errorf("the recorded projects array is %v, want it empty", projects)
	}
}

// Acceptance item 3. A directory discovered three levels under the boundary registers
// the repository it belongs to, not the boundary's child and not the boundary.
func TestRegisterUnderGlobalRootRecordsTheGitToplevelNotTheBoundaryChild(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	base := tempRealDir(t)
	repo := mkdirAll(t, filepath.Join(base, "a", "b", "repo"))
	sub := mkdirAll(t, filepath.Join(repo, "sub"))
	if output, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}

	r := openRepos(t, p)
	setGlobalRoot(t, r, base)
	if _, err := r.RegisterUnderGlobalRoot(sub, time.Time{}); err != nil {
		t.Fatalf("RegisterUnderGlobalRoot(%q) = %v", sub, err)
	}

	got, err := r.ConsentedRoot(sub)
	if err != nil {
		t.Fatalf("ConsentedRoot() = %v", err)
	}
	if got != repo {
		t.Errorf("ConsentedRoot() = %q, want the repository toplevel %q", got, repo)
	}
	if got == filepath.Join(base, "a") {
		t.Errorf("ConsentedRoot() = %q, the boundary's own child; discovery recorded the wrong root", got)
	}
}

// Requirement 7. A transcript can name a working directory that has since been
// deleted, and there is nothing left there to read: the refusal is what keeps the
// table free of a root nothing can be collected from.
func TestRegisterUnderGlobalRootRefusesADirectoryThatIsGone(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)

	r := openRepos(t, p)
	setGlobalRoot(t, r, base)
	before := readFileOrFail(t, p.ProjectsFile)

	if _, err := r.RegisterUnderGlobalRoot(filepath.Join(base, "gone"), time.Time{}); !errors.Is(err, ErrDiscoveredDirectoryGone) {
		t.Errorf("RegisterUnderGlobalRoot(a vanished directory) = %v, want ErrDiscoveredDirectoryGone", err)
	}
	if after := readFileOrFail(t, p.ProjectsFile); after != before {
		t.Errorf("the refusal rewrote the table:\n%s", after)
	}
}

func TestRegisterUnderGlobalRootRefusesADirectoryOutsideTheBoundary(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	boundary := mkdirAll(t, filepath.Join(base, "boundary"))
	outside := mkdirAll(t, filepath.Join(base, "outside"))

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	if _, err := r.RegisterUnderGlobalRoot(outside, time.Time{}); !errors.Is(err, ErrOutsideGlobalRoot) {
		t.Errorf("RegisterUnderGlobalRoot(a directory outside the boundary) = %v, want ErrOutsideGlobalRoot", err)
	}
}

// The boundary itself is observed and then skipped. The common invocation is
// `wake init -g` from the home directory, and registering that as a root would
// enclose every repository the boundary later discovers — which ADR-0019 §5's
// nested-root refusal would then refuse, one by one, for every one of them.
func TestRegisterUnderGlobalRootRefusesTheBoundaryItself(t *testing.T) {
	p := testPaths(t)
	boundary := mkdirAll(t, filepath.Join(tempRealDir(t), "boundary"))

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	if _, err := r.RegisterUnderGlobalRoot(boundary, time.Time{}); !errors.Is(err, ErrOutsideGlobalRoot) {
		t.Errorf("RegisterUnderGlobalRoot(the boundary itself) = %v, want ErrOutsideGlobalRoot", err)
	}
}

// The check that has to hold after discovery, not only before it: the root git hands
// back is the one written to the table, so it is the one consent is about.
//
// The environment is real rather than contrived. GIT_CEILING_DIRECTORIES is a
// colon-separated list, so a boundary whose own path contains a colon splits into
// entries that are ancestors of nothing, and git walks straight past the ceiling to
// the repository above it — verified against git 2.50.1, which answers with the
// boundary's parent. Registering that would attribute every repository under the
// parent to one id, which is the identity collapse ADR-0019 §5 keeps the root set
// non-nested to prevent.
func TestRegisterUnderGlobalRootRefusesARootDiscoveredOutsideTheBoundary(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	outer := tempRealDir(t)
	if output, err := exec.Command("git", "init", outer).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	boundary := mkdirAll(t, filepath.Join(outer, "dev:ops"))
	project := mkdirAll(t, filepath.Join(boundary, "project"))

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)
	before := readFileOrFail(t, p.ProjectsFile)

	if _, err := r.RegisterUnderGlobalRoot(project, time.Time{}); !errors.Is(err, ErrOutsideGlobalRoot) {
		t.Errorf("RegisterUnderGlobalRoot(a directory whose discovered root is outside the boundary) = %v, want ErrOutsideGlobalRoot", err)
	}
	if after := readFileOrFail(t, p.ProjectsFile); after != before {
		t.Errorf("a root the boundary does not enclose was recorded:\n%s", after)
	}
}

// The same post-condition against the other spelling that reaches it. Register records
// the symlink-resolved root, so a directory under the boundary whose physical location
// is outside it would be recorded outside the boundary: consent is about where the
// repository is, not about how a transcript spelled the way to it.
func TestRegisterUnderGlobalRootRefusesADirectoryThatSymlinksOutsideTheBoundary(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	boundary := mkdirAll(t, filepath.Join(base, "boundary"))
	elsewhere := mkdirAll(t, filepath.Join(base, "elsewhere"))
	link := filepath.Join(boundary, "project")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)
	before := readFileOrFail(t, p.ProjectsFile)

	if _, err := r.RegisterUnderGlobalRoot(link, time.Time{}); !errors.Is(err, ErrOutsideGlobalRoot) {
		t.Errorf("RegisterUnderGlobalRoot(a link out of the boundary) = %v, want ErrOutsideGlobalRoot", err)
	}
	if after := readFileOrFail(t, p.ProjectsFile); after != before {
		t.Errorf("a root outside the boundary was recorded through a link inside it:\n%s", after)
	}
}

// An auto-registered repository collects forward only, like every repository a plain
// `wake init` consents (ADR-0024, ADR-0025). The instant is the registration's, and it
// is recorded rather than merely intended.
func TestRegisterUnderGlobalRootRecordsTheForwardOnlyInstant(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	project := mkdirAll(t, filepath.Join(base, "project"))
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, base)
	id, err := r.RegisterUnderGlobalRoot(project, from)
	if err != nil {
		t.Fatalf("RegisterUnderGlobalRoot(%q) = %v", project, err)
	}

	if got := r.CollectsFrom(id); !got.Equal(from) {
		t.Errorf("CollectsFrom() = %s, want the registration instant %s", got, from)
	}
}

// A boundary legitimately encloses many roots, so SetGlobalRoot must not consult
// nestedWith: a boundary refused for enclosing a repository the user already
// consented would refuse the whole feature on any machine that has used `wake init`.
func TestSetGlobalRootAcceptsABoundaryEnclosingAConsentedRoot(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	root := mkdirAll(t, filepath.Join(base, "repo"))

	r := openRepos(t, p)
	mustRegister(t, r, root, "repo")

	if err := r.SetGlobalRoot(base); err != nil {
		t.Fatalf("SetGlobalRoot(a directory enclosing a consented root) = %v", err)
	}
	if got := r.GlobalRootState(); got != (GlobalRootState{Set: true, Discovered: 1}) {
		t.Errorf("GlobalRootState() = %+v, want the enclosed root counted", got)
	}
}

// One boundary at a time. Replacing it narrows or moves what is consented and leaves
// every identity already recorded exactly where it was — a root is never reassigned
// (ADR-0019 §9).
func TestSetGlobalRootReplacesAnEarlierBoundary(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	first := mkdirAll(t, filepath.Join(base, "first"))
	second := mkdirAll(t, filepath.Join(base, "second"))
	recorded := mkdirAll(t, filepath.Join(first, "repo"))

	r := openRepos(t, p)
	id := mustRegister(t, r, recorded, "repo")
	setGlobalRoot(t, r, first)
	setGlobalRoot(t, r, second)

	if r.WithinGlobalRoot(filepath.Join(first, "other")) {
		t.Error("WithinGlobalRoot(under the replaced boundary) = true")
	}
	if !r.WithinGlobalRoot(filepath.Join(second, "other")) {
		t.Error("WithinGlobalRoot(under the current boundary) = false")
	}
	reopened := openRepos(t, p)
	if got := mustIdentify(t, reopened, recorded); !got.Matched || got.ID != id {
		t.Errorf("Identify(a root recorded before the replacement) = %+v, want the id %s it already had", got, id)
	}
}

// ADR-0020's domain separation, applied to the boundary. A boundary digest and an
// entry digest are two keyed values over one key, and without distinct domains one
// could be substituted for the other — a recorded root's digest pasted into
// global_root would then verify, consenting everything under it.
func TestTheGlobalRootDigestUsesItsOwnDomain(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	r := openRepos(t, p)

	boundary := r.globalRootMAC(globalRootEntry{Root: root})
	for name, other := range map[string]string{
		"matchMAC":       r.matchMAC(projectEntry{Root: root}),
		"legacyMatchMAC": r.legacyMatchMAC(projectEntry{Root: root}),
	} {
		if boundary == other {
			t.Errorf("globalRootMAC equals %s for the same root; the two digests are interchangeable", name)
		}
	}
}

// ADR-0010: `init` is the only operation that writes. doctor is this function's
// caller, and a diagnostic that created the identity salt would make reading the
// state a write — and would hand a fresh install a salt it never consented to.
func TestGlobalRootStateForCreatesNoSalt(t *testing.T) {
	p := testPaths(t)

	got, err := GlobalRootStateFor(p)
	if err != nil {
		t.Fatalf("GlobalRootStateFor() = %v", err)
	}
	if got != (GlobalRootState{}) {
		t.Errorf("GlobalRootStateFor() on a fresh install = %+v, want the zero state", got)
	}
	if _, statErr := os.Stat(p.SaltFile); !os.IsNotExist(statErr) {
		t.Errorf("Stat(the salt) = %v, want it never created", statErr)
	}
}

// ADR-0019 §1, for the boundary. Resolution is a pure string operation over the
// recorded snapshot, and the boundary check runs on the derivation path for every
// unmatched working directory a scan sees — so it may not stat, resolve a symlink, or
// shell out, whatever has happened to the disk since.
func TestWithinGlobalRootTouchesNoFilesystem(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	boundary := mkdirAll(t, filepath.Join(base, "boundary"))
	child := filepath.Join(boundary, "project")

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)
	if err := os.RemoveAll(base); err != nil {
		t.Fatalf("removing %s: %v", base, err)
	}

	if !r.WithinGlobalRoot(child) {
		t.Error("WithinGlobalRoot() = false after the tree was deleted; the answer read the disk")
	}
}

// The count doctor prints is about the boundary, so it counts what the boundary
// strictly encloses and nothing else. A root outside it was consented by a plain
// `wake init` and has nothing to do with this number.
func TestGlobalRootStateCountsOnlyTrustedRootsUnderTheBoundary(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	boundary := mkdirAll(t, filepath.Join(base, "boundary"))
	inside := []string{
		mkdirAll(t, filepath.Join(boundary, "one")),
		mkdirAll(t, filepath.Join(boundary, "two")),
	}
	outside := mkdirAll(t, filepath.Join(base, "three"))

	r := openRepos(t, p)
	for _, root := range append(inside, outside) {
		mustRegister(t, r, root, filepath.Base(root))
	}
	setGlobalRoot(t, r, boundary)

	if got := openRepos(t, p).GlobalRootState(); got != (GlobalRootState{Set: true, Discovered: 2}) {
		t.Errorf("GlobalRootState() = %+v, want the two enclosed roots counted", got)
	}
}

// The refusals this file drives name the requirement and never the directory: a
// discovered working directory is repository content, and these errors reach the same
// terminal as everything else (plan §4.2).
func TestGlobalRootRefusalsNameNoPath(t *testing.T) {
	p := testPaths(t)
	const marker = "globalroot-unmistakable"
	base := mkdirAll(t, filepath.Join(tempRealDir(t), marker))
	boundary := mkdirAll(t, filepath.Join(base, "boundary"))

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"a relative boundary", func() error { return r.SetGlobalRoot(filepath.Join(marker, "relative")) }},
		{"a directory outside the boundary", func() error {
			_, err := r.RegisterUnderGlobalRoot(base, time.Time{})
			return err
		}},
		{"a vanished directory", func() error {
			_, err := r.RegisterUnderGlobalRoot(filepath.Join(boundary, marker+"-gone"), time.Time{})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.run()
			if err == nil {
				t.Fatal("the case produced no error; it proves nothing about error messages")
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the error %q names a directory", err)
			}
		})
	}
}

// `wake init -g` with no argument means the home directory, and the resolution lives
// here rather than in internal/cli: which directory gets consented is a decision, and
// nothing under internal/cli resolves the home directory (ADR-0001,
// TestInternalCliResolvesNoHomeDirectory).
func TestDefaultGlobalRootIsTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := DefaultGlobalRoot()
	if err != nil {
		t.Fatalf("DefaultGlobalRoot() = %v", err)
	}
	if got != home {
		t.Errorf("DefaultGlobalRoot() = %q, want the home directory %q", got, home)
	}
}
