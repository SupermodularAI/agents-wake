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

// mustRegisterUnderGlobalRoot registers a directory the boundary encloses the way a
// scan does — the auto-registration path, with no explicit `wake init` anywhere — and
// fails the case if it is refused. It is the counterpart of mustRegister for the one
// other call site ADR-0032 §2 licenses.
func mustRegisterUnderGlobalRoot(t *testing.T, r *Repos, dir string, from time.Time) string {
	t.Helper()
	id, err := r.RegisterUnderGlobalRoot(dir, from)
	if err != nil {
		t.Fatalf("RegisterUnderGlobalRoot(%q) = %v", dir, err)
	}
	if id == "" {
		t.Fatal("RegisterUnderGlobalRoot() returned no id")
	}
	return id
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

	_, err := r.RegisterUnderGlobalRoot(outside, time.Time{})
	if !errors.Is(err, ErrNotAnAdmittedWorktree) {
		t.Errorf("RegisterUnderGlobalRoot(a directory outside the boundary) = %v, want ErrNotAnAdmittedWorktree", err)
	}
	// The sentinel a scan skips rather than counts. This directory was never
	// consented, so nothing was lost by turning it away.
	if errors.Is(err, ErrOutsideGlobalRoot) {
		t.Errorf("the refusal is also ErrOutsideGlobalRoot; a scan would count a directory nobody consented as collection that was lost")
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

	if _, err := r.RegisterUnderGlobalRoot(boundary, time.Time{}); !errors.Is(err, ErrNotAnAdmittedWorktree) {
		t.Errorf("RegisterUnderGlobalRoot(the boundary itself) = %v, want ErrNotAnAdmittedWorktree", err)
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

	_, err := r.RegisterUnderGlobalRoot(project, time.Time{})
	if !errors.Is(err, ErrOutsideGlobalRoot) {
		t.Errorf("RegisterUnderGlobalRoot(a directory whose discovered root is outside the boundary) = %v, want ErrOutsideGlobalRoot", err)
	}
	// The sentinel a scan counts. The user consented this directory by naming the
	// boundary that encloses it, and the refusal means its sessions are carried by no
	// number — collection that was lost, not the boundary working.
	if errors.Is(err, ErrNotAnAdmittedWorktree) {
		t.Errorf("the refusal is also ErrNotAnAdmittedWorktree; a scan would skip a root that escaped the boundary instead of counting it")
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

	_, err := r.RegisterUnderGlobalRoot(link, time.Time{})
	if !errors.Is(err, ErrOutsideGlobalRoot) {
		t.Errorf("RegisterUnderGlobalRoot(a link out of the boundary) = %v, want ErrOutsideGlobalRoot", err)
	}
	if errors.Is(err, ErrNotAnAdmittedWorktree) {
		t.Errorf("the refusal is also ErrNotAnAdmittedWorktree; a scan would skip a root that escaped the boundary instead of counting it")
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

	cases := []struct {
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
	}

	// The refusal ADR-0044 §1's second arm adds. Both the worktree and the repository
	// it belongs to sit under the marker directory, so a message that leaked either
	// one is caught here. Appended rather than declared inline because it needs a real
	// git, and a machine without one should still run the three cases above.
	if _, err := exec.LookPath("git"); err == nil {
		t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
		t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
		main := mkdirAll(t, filepath.Join(base, "main"))
		worktree := filepath.Join(base, "worktree")
		for _, step := range [][]string{
			{"init", main},
			{"-C", main, "-c", "user.email=wake@example.invalid", "-c", "user.name=wake", "commit", "--allow-empty", "-m", "init"},
			{"-C", main, "worktree", "add", worktree},
		} {
			if output, gitErr := exec.Command("git", step...).CombinedOutput(); gitErr != nil {
				t.Fatalf("git %v: %v: %s", step, gitErr, output)
			}
		}
		cases = append(cases, struct {
			name string
			run  func() error
		}{"a linked worktree of an unconsented repository", func() error {
			_, err := r.RegisterUnderGlobalRoot(worktree, time.Time{})
			return err
		}})
	}

	for _, c := range cases {
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

// DG-109. The path that becomes the everyday one once a global root is recorded: a
// linked worktree a scan discovers under the boundary gains the repository it belongs
// to, and no `wake init` is ever run inside it. Registration-during-ingest *is*
// registration and calls the identical function (ADR-0032 §2), so it records the
// relation the registration path records (ADR-0040 §2).
//
// The parent is registered first because ADR-0040 §2 is explicit that a parent this
// table does not hold is a parent this machine has not consented: the relation would
// be refused, which is the separate case below.
func TestRegisterUnderGlobalRootRecordsTheWorktreeRelationWithNoExplicitInit(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	// The sibling-worktree fixture DG-104 added (ADR-0019 §5: a worktree inside the
	// main checkout would be nested with a consented root and refused). The boundary
	// is the directory holding both siblings, and it encloses many roots legitimately
	// because a boundary is not itself a root (ADR-0032 §3).
	main, worktree := initWorktree(t)
	boundary := filepath.Dir(main)
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	mainID := mustRegisterUnderGlobalRoot(t, r, main, from)
	worktreeID := mustRegisterUnderGlobalRoot(t, r, worktree, from)

	if mainID == worktreeID {
		t.Fatalf("the worktree registered under the main checkout's identity %q; a worktree is its own repository (ADR-0019 §6)", mainID)
	}
	if got := recordedEntry(t, p, worktreeID).BelongsTo; got != mainID {
		t.Errorf("the worktree's belongs_to = %q, want the main checkout's id %q — auto-registration under a boundary must record the relation without an explicit init", got, mainID)
	}
	// Requirement 2. An entry that belongs to itself is not a worktree, and valid()
	// refuses one as the floor (ADR-0040 §1).
	if got := recordedEntry(t, p, mainID).BelongsTo; got != "" {
		t.Errorf("the main checkout's belongs_to = %q, want none", got)
	}
	// Nothing absorbed and nothing reassigned: two entries, each under its own root.
	if entries := recordedEntries(t, p); len(entries) != 2 {
		t.Fatalf("projects.json holds %d entries, want 2", len(entries))
	}
	for _, want := range []struct{ id, root string }{{mainID, main}, {worktreeID, worktree}} {
		if got := recordedEntry(t, p, want.id).Root; got != want.root {
			t.Errorf("entry %q has root %q, want %q", want.id, got, want.root)
		}
	}
}

// DG-109, requirement 3. Most directories under a boundary are not worktrees, and the
// absence has to be an asserted absence rather than an untested default: belongs_to is
// omitempty, so a wrong value and no value serialise differently but read alike.
//
// No requireGit: a plain directory is a repository wake will consent (ADR-0019 §5) and
// a worktree of nothing, and the parent lookup in internal/config/root.go answers nil
// in every git failure direction, so the absence is the same with git present and with
// git missing. This mirrors TestRegisterUnderGlobalRootRecordsTheForwardOnlyInstant,
// which registers a plain directory the same way.
func TestRegisterUnderGlobalRootRecordsNoRelationForAPlainRepository(t *testing.T) {
	p := testPaths(t)
	boundary := tempRealDir(t)
	plain := mkdirAll(t, filepath.Join(boundary, "plain"))
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	id := mustRegisterUnderGlobalRoot(t, r, plain, from)
	if got := recordedEntry(t, p, id).BelongsTo; got != "" {
		t.Errorf("belongs_to = %q, want none; a plain repository belongs to nothing", got)
	}
}

// DG-109, requirement 4. A boundary walk has no opinion about which of two sibling
// directories it reaches first, and the explicit-`init` path never exercises the order
// where the worktree comes first. ADR-0040 §2 decides it: a parent this table does not
// hold is a parent this machine has not consented, so the *relation* is refused and the
// registration is not.
//
// The refusal is permanent rather than pending. §2's "never moved and never cleared"
// and §6's "Nothing is rewritten on read" mean nothing fills it in afterwards, and the
// scan never offers the directory again — resolverFor observes a cwd only when
// Identify did not match it (internal/activation/activation.go), which is ADR-0032 §5's
// "matches no recorded entry". The remedy is the one ADR-0040 §6 names: `wake init`
// inside the worktree.
//
// This divergence between the two orders is not an ADR-0004 breach. Repos.Identify
// never reads belongs_to (ADR-0040 §1), so no stored id, no event_id and no wake.repo
// hash varies with registration order; only the render-time and flush-time roll-up
// does (ADR-0040 §4, §5), which is an accepted consequence of ADR-0040.
func TestRegisterUnderGlobalRootRefusesTheRelationWhenTheWorktreeIsDiscoveredFirst(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktree(t)
	boundary := filepath.Dir(main)
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	worktreeID := mustRegisterUnderGlobalRoot(t, r, worktree, from)
	if got := recordedEntry(t, p, worktreeID).BelongsTo; got != "" {
		t.Fatalf("belongs_to = %q with the parent not yet consented, want none (ADR-0040 §2)", got)
	}

	mainID := mustRegisterUnderGlobalRoot(t, r, main, from)
	if mainID == worktreeID {
		t.Fatalf("the two siblings share the id %q", mainID)
	}
	if got := recordedEntry(t, p, worktreeID).BelongsTo; got != "" {
		t.Errorf("belongs_to = %q once the parent registered, want none; a refused relation is never filled in later (ADR-0040 §2, §6)", got)
	}
	if got := recordedEntry(t, p, mainID).BelongsTo; got != "" {
		t.Errorf("the main checkout's belongs_to = %q, want none", got)
	}
	// Deterministic rather than merely not-yet: the worktree now matches a recorded
	// entry, so the walk that would re-offer it never sees it again.
	identity := mustIdentify(t, r, worktree)
	if !identity.Matched || identity.ID != worktreeID {
		t.Fatalf("Identify(the worktree) = %+v, want its own id %q matched", identity, worktreeID)
	}
	if entries := recordedEntries(t, p); len(entries) != 2 {
		t.Errorf("projects.json holds %d entries, want 2", len(entries))
	}
}

// ADR-0019 §1, for the candidate test DG-113 widens the walk's gate to.
//
// It runs on the derivation path for every unmatched working directory a scan sees,
// exactly where WithinGlobalRoot ran, so it may not stat, resolve a symlink, or shell
// out — whatever has happened to the disk since. The git question ADR-0044 §1 adds is
// asked after the walk, once, in RegisterUnderGlobalRoot.
func TestOfferableUnderGlobalRootAdmitsNothingAndTouchesNoFilesystem(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	boundary := mkdirAll(t, filepath.Join(base, "boundary"))
	inside := filepath.Join(boundary, "project")
	outside := mkdirAll(t, filepath.Join(base, "elsewhere"))

	r := openRepos(t, p)
	// The common case first: no boundary recorded, so nothing is ever a candidate and
	// a scan on a machine that never ran `init --global` pays for nothing.
	if r.OfferableUnderGlobalRoot(inside) {
		t.Error("OfferableUnderGlobalRoot() = true with no boundary recorded")
	}

	setGlobalRoot(t, r, boundary)
	if !r.OfferableUnderGlobalRoot(inside) {
		t.Error("OfferableUnderGlobalRoot(a directory under the boundary) = false")
	}
	// The widening itself: where a linked worktree lives is a property of whichever
	// tool manages them, so no path test can tell one from any other directory
	// (ADR-0044 §1). Admission is RegisterUnderGlobalRoot's decision, not this one's.
	if !r.OfferableUnderGlobalRoot(outside) {
		t.Error("OfferableUnderGlobalRoot(a directory outside the boundary) = false; a linked worktree cannot be told from its path")
	}

	if err := os.RemoveAll(base); err != nil {
		t.Fatalf("removing %s: %v", base, err)
	}
	if !r.OfferableUnderGlobalRoot(inside) || !r.OfferableUnderGlobalRoot(outside) {
		t.Error("OfferableUnderGlobalRoot() changed after the tree was deleted; the answer read the disk")
	}
}

// initWorktreeElsewhere is initWorktree's fixture with the worktree deliberately
// outside the directory a test records as the boundary — the shape DG-113 is about,
// and the one initWorktree (both siblings under one base) cannot express.
//
// Two separate temporary bases, so filepath.Dir(main) can be the boundary and the
// worktree still falls outside it. The commit identity is supplied per invocation for
// the reason initWorktree gives.
func initWorktreeElsewhere(t *testing.T) (main, worktree string) {
	t.Helper()
	main = mkdirAll(t, filepath.Join(tempRealDir(t), "main"))
	worktree = filepath.Join(tempRealDir(t), "worktree")
	for _, step := range [][]string{
		{"init", main},
		{"-C", main, "-c", "user.email=wake@example.invalid", "-c", "user.name=wake", "commit", "--allow-empty", "-m", "init"},
		{"-C", main, "worktree", "add", worktree},
	} {
		if output, err := exec.Command("git", step...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", step, err, output)
		}
	}
	return main, worktree
}

// The bug DG-113 fixes. A linked worktree of a consented repository lives wherever
// the tool that created it put it, which is usually not inside the consented tree —
// so it failed a path test its own repository passes, and collected nothing.
//
// ADR-0044 §1: it is admitted under its own identity, carrying the belongs_to
// relation ADR-0040 defined, exactly as though the user had run `wake init` in it.
func TestRegisterUnderGlobalRootAdmitsALinkedWorktreeOfAConsentedRepository(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktreeElsewhere(t)
	boundary := filepath.Dir(main)
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	mainID := mustRegisterUnderGlobalRoot(t, r, main, from)
	worktreeID := mustRegisterUnderGlobalRoot(t, r, worktree, from)

	if mainID == worktreeID {
		t.Fatalf("the worktree registered under the main checkout's identity %q; a worktree is its own repository (ADR-0019 §6)", mainID)
	}
	if got := recordedEntry(t, p, worktreeID).Root; got != worktree {
		t.Errorf("the worktree's recorded root = %q, want its own top level %q", got, worktree)
	}
	if got := recordedEntry(t, p, worktreeID).BelongsTo; got != mainID {
		t.Errorf("the worktree's belongs_to = %q, want the main checkout's id %q", got, mainID)
	}
	if entries := recordedEntries(t, p); len(entries) != 2 {
		t.Errorf("projects.json holds %d entries, want 2", len(entries))
	}
}

// The recorded root is the worktree's top level and never the subdirectory a session
// happened to run in — the same thing the first arm's bounded discovery guarantees
// under the boundary.
//
// It is also the ceiling's behavioural assertion: this fails if the ceiling handed to
// git is the top level itself rather than its parent, because git will not chdir up
// *into* a ceiling directory and the walk would stop one directory short.
func TestRegisterUnderGlobalRootAdmitsALinkedWorktreeFromASubdirectoryOfIt(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktreeElsewhere(t)
	sub := mkdirAll(t, filepath.Join(worktree, "sub"))
	boundary := filepath.Dir(main)
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)
	mustRegisterUnderGlobalRoot(t, r, main, from)

	id := mustRegisterUnderGlobalRoot(t, r, sub, from)
	if got := recordedEntry(t, p, id).Root; got != worktree {
		t.Errorf("the recorded root = %q, want the worktree's top level %q", got, worktree)
	}
}

// ADR-0044 §1's disjunction: consented *or* inside the boundary. A repository the
// boundary encloses is one the user consented by naming the boundary, whether or not
// a scan has reached it yet.
//
// The relation is a separate question and ADR-0040 §2 owns it: a parent this table
// does not hold is a parent this machine has not consented, so belongs_to is refused
// and the registration is not. This is the narrower true statement behind ADR-0044's
// Consequences bullet about DG-109's ordering hazard, which claims it disappears on
// this path by construction — true when the parent is a recorded entry, and not when
// it is merely inside the boundary.
func TestRegisterUnderGlobalRootAdmitsAWorktreeWhoseRepositoryIsInsideTheBoundaryButNotYetAnEntry(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktreeElsewhere(t)
	boundary := filepath.Dir(main)
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	id := mustRegisterUnderGlobalRoot(t, r, worktree, from)
	if got := recordedEntry(t, p, id).Root; got != worktree {
		t.Errorf("the recorded root = %q, want the worktree %q", got, worktree)
	}
	if got := recordedEntry(t, p, id).BelongsTo; got != "" {
		t.Errorf("belongs_to = %q with the parent not yet an entry, want none (ADR-0040 §2)", got)
	}
}

// Acceptance criterion 3, and the rule working rather than failing: a worktree whose
// repository is not consented is still refused. The exposure ADR-0044 §2 accepts is
// bounded by a consent decision the user already made, and here there is none.
func TestRegisterUnderGlobalRootRefusesAWorktreeWhoseRepositoryIsNotConsented(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	_, worktree := initWorktreeElsewhere(t)
	boundary := tempRealDir(t)
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	if _, err := r.RegisterUnderGlobalRoot(worktree, from); !errors.Is(err, ErrNotAnAdmittedWorktree) {
		t.Errorf("RegisterUnderGlobalRoot(a worktree of an unconsented repository) error = %v, want ErrNotAnAdmittedWorktree", err)
	}
	if entries := recordedEntries(t, p); len(entries) != 0 {
		t.Errorf("projects.json holds %d entries, want none", len(entries))
	}
}

// Acceptance criterion 4. The second arm admits linked worktrees and nothing else: a
// plain git repository outside the boundary is exactly as unconsented as it was
// before ADR-0044, and so is every directory that is not a repository at all.
func TestRegisterUnderGlobalRootRefusesAGitRepositoryOutsideTheBoundaryThatIsNotAWorktree(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	repo, _ := initRepo(t)
	boundary := tempRealDir(t)
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)

	if _, err := r.RegisterUnderGlobalRoot(repo, from); !errors.Is(err, ErrNotAnAdmittedWorktree) {
		t.Errorf("RegisterUnderGlobalRoot(a main checkout outside the boundary) error = %v, want ErrNotAnAdmittedWorktree", err)
	}
	if entries := recordedEntries(t, p); len(entries) != 0 {
		t.Errorf("projects.json holds %d entries, want none", len(entries))
	}
}

// Acceptance criterion 6. The registration a hook fires inherits a session's
// environment, and git documents that GIT_DIR is not excluded by a ceiling — so an
// exported GIT_DIR would otherwise make both git calls answer about a repository
// nowhere near the directory being asked about, and admit it.
func TestRegisterUnderGlobalRootDoesNotLetGitsEnvironmentWidenTheAdmittedWorktree(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktreeElsewhere(t)
	boundary := filepath.Dir(main)
	elsewhere, _ := initRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(elsewhere, ".git"))
	t.Setenv("GIT_WORK_TREE", elsewhere)
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)
	mainID := mustRegisterUnderGlobalRoot(t, r, main, from)

	worktreeID := mustRegisterUnderGlobalRoot(t, r, worktree, from)
	if got := recordedEntry(t, p, worktreeID).Root; got != worktree {
		t.Errorf("the recorded root = %q, want the worktree's own %q; the environment widened the answer", got, worktree)
	}
	if got := recordedEntry(t, p, worktreeID).BelongsTo; got != mainID {
		t.Errorf("belongs_to = %q, want the main checkout's id %q", got, mainID)
	}
	for _, entry := range recordedEntries(t, p) {
		if entry.Root == elsewhere {
			t.Errorf("an entry was recorded for %q, the repository only the environment named", elsewhere)
		}
	}
}

// Fail closed, in the one direction the probe's inherited ceiling can be pushed. The
// probe deliberately honours an inherited GIT_CEILING_DIRECTORIES because it can only
// make git find less; a ceiling at the worktree itself makes the probe unable to see
// the worktree from a subdirectory of it, and the answer is a refusal rather than a
// registration of the subdirectory as a repository of its own.
func TestRegisterUnderGlobalRootRefusesAWorktreeWhoseCeilingBoundDiscoveryDisagreesWithTheProbe(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktreeElsewhere(t)
	sub := mkdirAll(t, filepath.Join(worktree, "sub"))
	boundary := filepath.Dir(main)
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)
	mustRegisterUnderGlobalRoot(t, r, main, from)

	t.Setenv("GIT_CEILING_DIRECTORIES", worktree)
	// ErrNotAnAdmittedWorktree rather than ErrOutsideGlobalRoot: the ceiling stops the
	// probe before it can name a worktree at all, so this directory is indistinguishable
	// from one that is not a worktree and consent is never decided for it.
	if _, err := r.RegisterUnderGlobalRoot(sub, from); !errors.Is(err, ErrNotAnAdmittedWorktree) {
		t.Errorf("RegisterUnderGlobalRoot(a worktree subdirectory under a hostile ceiling) error = %v, want ErrNotAnAdmittedWorktree", err)
	}
	for _, entry := range recordedEntries(t, p) {
		if entry.Root == sub {
			t.Errorf("an entry was recorded for %q, a subdirectory of a worktree", sub)
		}
	}
}

// The second arm's consent check has to be as strict as the first arm's, not looser.
//
// git can name a repository through one spelling while it physically lives at another,
// and the two spellings can straddle the boundary. The first arm refuses a repository
// on exactly that shape: it requires both the discovered and the canonical spelling to
// be inside the boundary, because Register records the canonical root and consent is
// about where the repository is rather than about how something spelled the way to it
// (ADR-0019 §5). ADR-0044 §2 replaces the boundary ceiling on this arm with "a consent
// decision the user already made" and calls the replacement stronger, not weaker — so
// the boundary disjunct here has to agree with the first arm rather than admit what it
// refuses.
//
// The recorded-entry disjunct keeps matching under any spelling, and that is not the
// same rule: an entry is a consent decision the user made about a repository, and the
// alias is how the same repository is recognised through the other spelling (ADR-0019
// §5). The boundary disjunct is the inferred one, and it is the one that has to be
// conservative.
func TestConsentsRepositoryRequiresEverySpellingInsideTheBoundary(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	boundary := mkdirAll(t, filepath.Join(base, "boundary"))
	inside := filepath.Join(boundary, "main")
	outside := filepath.Join(base, "external", "main")

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)
	recorded := r.table.GlobalRoot

	if !r.consentsRepository([]string{inside}, recorded) {
		t.Error("consentsRepository(a repository inside the boundary) = false")
	}
	if r.consentsRepository([]string{outside}, recorded) {
		t.Error("consentsRepository(a repository outside the boundary) = true")
	}
	if r.consentsRepository([]string{inside, outside}, recorded) {
		t.Error("consentsRepository(a repository the boundary only names, and does not hold) = true; the first arm refuses that repository outright")
	}
	if r.consentsRepository([]string{outside, inside}, recorded) {
		t.Error("consentsRepository() answered differently with the spellings the other way round; the rule is about the set, not the order")
	}
}

// The identity-collapse guard, the one refusal branch in the second arm that had no
// test of its own.
//
// A worktree top level at or above the boundary would become the recorded root of
// everything inside it, which is the collapse ADR-0019 §5 keeps the root set
// non-nested to prevent and what the first arm's own post-discovery check exists for.
// ADR-0044 §2 says the same thing about this arm's ceiling — "not the boundary and not
// the filesystem root" — and consent being satisfied is not a reason to skip it: here
// the repository *is* a recorded entry, so the refusal comes from the guard and
// nowhere else.
//
// The sentinel is ErrOutsideGlobalRoot rather than ErrNotAnAdmittedWorktree, because
// consent has already been decided by this point: a scan counts this one.
func TestRegisterUnderGlobalRootRefusesAWorktreeThatEnclosesTheBoundary(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktreeElsewhere(t)
	boundary := mkdirAll(t, filepath.Join(worktree, "inner"))
	from := time.Now().UTC()

	r := openRepos(t, p)
	setGlobalRoot(t, r, boundary)
	mustRegister(t, r, main, "main")

	if _, err := r.RegisterUnderGlobalRoot(worktree, from); !errors.Is(err, ErrOutsideGlobalRoot) {
		t.Errorf("RegisterUnderGlobalRoot(a worktree enclosing the boundary) error = %v, want ErrOutsideGlobalRoot", err)
	}
	for _, entry := range recordedEntries(t, p) {
		if entry.Root == worktree {
			t.Errorf("an entry was recorded for %q, a root that encloses the boundary", worktree)
		}
	}
}
