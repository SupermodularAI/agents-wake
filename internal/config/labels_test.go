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
