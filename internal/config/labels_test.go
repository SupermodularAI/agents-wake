package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectLabelsReturnsTheLabelOfEveryTrustedRepository(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	alpha := mkdirAll(t, filepath.Join(base, "alpha"))
	beta := mkdirAll(t, filepath.Join(base, "beta"))

	r := openRepos(t, p)
	alphaID := mustRegister(t, r, alpha, "alpha")
	betaID := mustRegister(t, r, beta, "beta")

	labels := ProjectLabels(p)
	if len(labels) != 2 {
		t.Fatalf("ProjectLabels() holds %d entries, want 2", len(labels))
	}
	for id, want := range map[string]string{alphaID: "alpha", betaID: "beta"} {
		if got := labels[id]; got != want {
			t.Errorf("ProjectLabels()[the id of %s] = %q, want %q", want, got, want)
		}
	}
}

// TestProjectLabelsWritesNothingAndCreatesNoSalt is the guarantee `--dry-run`
// rests on: inspecting what would leave the machine may not itself change the
// machine, and creating the salt as a side effect would make PreviewFlush's "it
// never writes" false.
func TestProjectLabelsWritesNothingAndCreatesNoSalt(t *testing.T) {
	p := testPaths(t)

	if labels := ProjectLabels(p); len(labels) != 0 {
		t.Fatalf("ProjectLabels() on a fresh machine holds %d entries, want 0", len(labels))
	}

	for name, path := range map[string]string{"the salt": p.SaltFile, "the project table": p.ProjectsFile} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("os.Lstat(%s) = %v, want fs.ErrNotExist; ProjectLabels created it", name, err)
		}
	}
}

func TestProjectLabelsRefusesAHandEditedEntry(t *testing.T) {
	p := testPaths(t)
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "real"))

	r := openRepos(t, p)
	realID := mustRegister(t, r, root, "real")

	table, _, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	impostorID := hexID('a')
	table.Projects = append(table.Projects, projectEntry{
		ID:    impostorID,
		Label: "impostor",
		Root:  "/impostor",
	})
	if err := writeProjects(p.ProjectsFile, table); err != nil {
		t.Fatalf("writeProjects() = %v", err)
	}

	labels := ProjectLabels(p)
	if got := labels[realID]; got != "real" {
		t.Errorf("ProjectLabels()[the registered id] = %q, want %q", got, "real")
	}
	if _, ok := labels[impostorID]; ok {
		t.Errorf("ProjectLabels() carries the hand-written entry's id; an entry this build does not derive must not reach the wire")
	}
	for id, label := range labels {
		if label == "impostor" {
			t.Errorf("ProjectLabels()[%q] = %q; a hand-written label reached the projection", id, label)
		}
	}
}

// TestProjectLabelsCarriesNoRootOrAlias pins the shape of the answer: id → label,
// and nothing else. The root is what plan §3.4 keeps on the machine under every
// condition.
func TestProjectLabelsCarriesNoRootOrAlias(t *testing.T) {
	p := testPaths(t)
	real := tempRealDir(t)
	root := mkdirAll(t, filepath.Join(real, "repo"))
	link := filepath.Join(real, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("creating the alias spelling: %v", err)
	}

	r := openRepos(t, p)
	mustRegister(t, r, link, "repo")

	for id, label := range ProjectLabels(p) {
		if strings.ContainsAny(label, "/"+string(filepath.Separator)) {
			t.Errorf("ProjectLabels()[%q] = %q and holds a path separator", id, label)
		}
		for name, path := range map[string]string{"the canonical root": root, "the alias spelling": link, "the enclosing directory": real} {
			if strings.Contains(label, path) || label == path {
				t.Errorf("ProjectLabels()[%q] = %q and carries %s", id, label, name)
			}
		}
	}
}

// TestProjectLabelsIgnoresTheCollectionBoundary is BC-13: the machine-wide
// boundary is a consent boundary and never an identity, so nothing keyed to it
// can reach the wire — it has no id and no label to read in the first place.
func TestProjectLabelsIgnoresTheCollectionBoundary(t *testing.T) {
	p := testPaths(t)
	base := tempRealDir(t)
	root := mkdirAll(t, filepath.Join(base, "repo"))

	r := openRepos(t, p)
	id := mustRegister(t, r, root, "repo")
	setGlobalRoot(t, r, base)

	labels := ProjectLabels(p)
	if len(labels) != 1 {
		t.Fatalf("ProjectLabels() holds %d entries, want 1 (the boundary is not a repository)", len(labels))
	}
	if got := labels[id]; got != "repo" {
		t.Errorf("ProjectLabels()[the registered id] = %q, want %q", got, "repo")
	}
	for gotID, label := range labels {
		if strings.Contains(label, filepath.Base(base)) && label != "repo" {
			t.Errorf("ProjectLabels()[%q] = %q and names the collection boundary", gotID, label)
		}
	}
}

// The wire half of DG-104. A worktree's records keep the worktree's own hash in
// wake.repo (ADR-0019 §1, §3), so the readable name beside it has to come from
// somewhere: the label of the repository the worktree belongs to. Without this,
// wake.repo_label and langfuse.trace.name at the receiver name a scratch directory
// instead of the project.
func TestProjectLabelsResolvesAWorktreeToItsParentsLabel(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktree(t)

	r := openRepos(t, p)
	mainID := mustRegister(t, r, main, "alpha")
	worktreeID := mustRegister(t, r, worktree, "beta")

	labels := ProjectLabels(p)
	if got := labels[worktreeID]; got != "alpha" {
		t.Errorf("ProjectLabels()[the worktree's id] = %q, want the repository's label %q", got, "alpha")
	}
	if got := labels[mainID]; got != "alpha" {
		t.Errorf("ProjectLabels()[the repository's id] = %q, want %q", got, "alpha")
	}
	if len(labels) != 2 {
		t.Errorf("ProjectLabels() holds %d entries, want 2; a resolved relation removes no id", len(labels))
	}
}

// A repository that records no relation is untouched by the resolution — the
// overwhelmingly common case, and the one every existing caller already depends on.
func TestProjectLabelsIsUnchangedForAnUnrelatedRepository(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktree(t)
	other := mkdirAll(t, filepath.Join(tempRealDir(t), "gamma"))

	r := openRepos(t, p)
	mustRegister(t, r, main, "alpha")
	mustRegister(t, r, worktree, "beta")
	otherID := mustRegister(t, r, other, "gamma")

	if got := ProjectLabels(p)[otherID]; got != "gamma" {
		t.Errorf("ProjectLabels()[an unrelated repository] = %q, want its own label %q", got, "gamma")
	}
}

// The render half. RepoRollup answers only for an id whose activity is counted
// somewhere other than under itself; absent means "its own", so an unrelated
// repository never appears.
func TestRepoRollupMapsAWorktreeOntoTheRepositoryItBelongsTo(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktree(t)

	r := openRepos(t, p)
	mainID := mustRegister(t, r, main, "alpha")
	worktreeID := mustRegister(t, r, worktree, "beta")

	rollup := RepoRollup(p)
	if got := rollup[worktreeID]; got != mainID {
		t.Errorf("RepoRollup()[the worktree's id] = %q, want the repository's id %q", got, mainID)
	}
	if _, present := rollup[mainID]; present {
		t.Errorf("RepoRollup() holds the repository's own id; a repository with no relation must be absent")
	}
	if len(rollup) != 1 {
		t.Errorf("RepoRollup() holds %d entries, want 1", len(rollup))
	}
}

// Fail closed on a dangling relation: the target was removed from the table, or
// refused on read, and rolling rows onto an id nothing names would render a
// repository nobody can put a name to. The worktree renders as itself instead,
// which is what it did before the relation existed.
func TestRepoRollupAndProjectLabelsIgnoreARelationWhoseTargetIsNotInTheTable(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktree(t)

	r := openRepos(t, p)
	mainID := mustRegister(t, r, main, "alpha")
	worktreeID := mustRegister(t, r, worktree, "beta")

	table, _, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	kept := table.Projects[:0]
	for _, entry := range table.Projects {
		if entry.ID != mainID {
			kept = append(kept, entry)
		}
	}
	table.Projects = kept
	if err := writeProjects(p.ProjectsFile, table); err != nil {
		t.Fatalf("writeProjects() = %v", err)
	}

	if rollup := RepoRollup(p); len(rollup) != 0 {
		t.Errorf("RepoRollup() = %v, want empty; a relation whose target is gone rolls nothing up", rollup)
	}
	if got := ProjectLabels(p)[worktreeID]; got != "beta" {
		t.Errorf("ProjectLabels()[the worktree's id] = %q, want its own label %q", got, "beta")
	}
}

// The same shape assertion TestProjectLabelsCarriesNoRootOrAlias makes, for the
// second projection: ids on both sides, and nothing that could be a path. The root
// is what plan §3.4 keeps on the machine under every condition.
func TestRepoRollupCarriesNoRootOrAlias(t *testing.T) {
	requireGit(t)
	p := testPaths(t)
	main, worktree := initWorktree(t)

	r := openRepos(t, p)
	mustRegister(t, r, main, "alpha")
	mustRegister(t, r, worktree, "beta")

	rollup := RepoRollup(p)
	if len(rollup) == 0 {
		t.Fatal("RepoRollup() is empty; nothing was asserted")
	}
	for from, to := range rollup {
		for name, id := range map[string]string{"the key": from, "the value": to} {
			if !validID(id) {
				t.Errorf("RepoRollup(): %s %q is not a repository id", name, id)
			}
			for what, path := range map[string]string{"the repository root": main, "the worktree root": worktree} {
				if strings.Contains(id, path) {
					t.Errorf("RepoRollup(): %s %q carries %s", name, id, what)
				}
			}
		}
	}
}
