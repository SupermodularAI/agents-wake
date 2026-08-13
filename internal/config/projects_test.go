package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeProjectsJSON puts a hand-written table under the data root. Hand-written
// is the point: several of these shapes are ones writeProjects would never
// produce, and they are exactly the ones a reader has to survive.
func writeProjectsJSON(t *testing.T, p Paths, content string) {
	t.Helper()
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		t.Fatalf("creating the data root: %v", err)
	}
	if err := os.WriteFile(p.ProjectsFile, []byte(content), 0o600); err != nil {
		t.Fatalf("writing projects.json: %v", err)
	}
}

func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}

// A valid entry, spelled once so a test can vary one field at a time.
func validEntry() projectEntry {
	return projectEntry{
		ID:    strings.Repeat("ab", idHexLen/2),
		Label: "the-repo",
		Root:  "/somewhere/the-repo",
	}
}

// Acceptance item 3. projects.json holds real paths and real names — the one
// genuinely sensitive file in the system besides the salt.
func TestProjectsFileIsCreated0600(t *testing.T) {
	p := testPaths(t)

	if err := writeProjects(p.ProjectsFile, projectsFile{Projects: []projectEntry{validEntry()}}); err != nil {
		t.Fatalf("writeProjects() = %v", err)
	}

	fi, err := os.Stat(p.ProjectsFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
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

// Reading is not registering. A fresh install has no table, and asking which
// repository a directory belongs to must not create one.
func TestMissingProjectsFileIsAnEmptyTableAndCreatesNothing(t *testing.T) {
	p := testPaths(t)

	table, dropped, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if len(table.Projects) != 0 || dropped != 0 {
		t.Errorf("readProjects() = (%d entries, %d dropped), want an empty table", len(table.Projects), dropped)
	}
	for _, path := range []string{p.ProjectsFile, p.DataDir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("os.Lstat(%q) = %v, want ErrNotExist — reading must create nothing", path, err)
		}
	}
}

// Fail closed and count. An entry this build cannot trust is dropped, never
// repaired into a different identity (constraint 22), and never allowed to make
// the whole table unreadable — one bad line must not cost the other repositories.
func TestAMalformedEntryIsDroppedAndCounted(t *testing.T) {
	p := testPaths(t)
	good := validEntry()
	content := `{
  "version": 1,
  "projects": [
    {"id": "` + good.ID + `", "label": "the-repo", "root": "/somewhere/the-repo"},
    {"id": "` + strings.Repeat("z", idHexLen) + `", "label": "not-hex", "root": "/somewhere/a"},
    {"id": "` + strings.Repeat("a", idHexLen-1) + `", "label": "too-short", "root": "/somewhere/b"},
    {"id": "` + strings.Repeat("b", idHexLen) + `", "label": "relative", "root": "somewhere/c"},
    {"id": "` + strings.Repeat("c", idHexLen) + `", "label": "has/separator", "root": "/somewhere/d"}
  ]
}
`
	writeProjectsJSON(t, p, content)

	table, dropped, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if dropped != 4 {
		t.Errorf("dropped = %d, want 4", dropped)
	}
	if len(table.Projects) != 1 {
		t.Fatalf("kept %d entries, want 1", len(table.Projects))
	}
	if table.Projects[0].ID != good.ID {
		t.Errorf("kept entry id = %q, want %q", table.Projects[0].ID, good.ID)
	}
	if got := readFileOrFail(t, p.ProjectsFile); got != content {
		t.Error("readProjects rewrote the file; a read must never repair the table")
	}
}

func TestEntryValidationRejectsWhatItCannotTrust(t *testing.T) {
	separator := "a" + string(filepath.Separator) + "b"

	for _, c := range []struct {
		name   string
		mutate func(e *projectEntry)
		want   bool
	}{
		{"a well-formed entry", func(*projectEntry) {}, true},
		{"an id that is not hex", func(e *projectEntry) { e.ID = strings.Repeat("g", idHexLen) }, false},
		{"an id in upper case", func(e *projectEntry) { e.ID = strings.Repeat("AB", idHexLen/2) }, false},
		{"an id that is too short", func(e *projectEntry) { e.ID = strings.Repeat("a", idHexLen-1) }, false},
		{"an id that is too long", func(e *projectEntry) { e.ID = strings.Repeat("a", idHexLen+1) }, false},
		{"a relative root", func(e *projectEntry) { e.Root = "relative/root" }, false},
		{"an empty root", func(e *projectEntry) { e.Root = "" }, false},
		{"an unclean root", func(e *projectEntry) { e.Root = "/a/b/../c" }, false},
		{"a trailing separator on the root", func(e *projectEntry) { e.Root = "/a/b/" }, false},
		{"the filesystem root", func(e *projectEntry) { e.Root = "/" }, true},
		{"an empty label", func(e *projectEntry) { e.Label = "" }, false},
		{"a label holding a separator", func(e *projectEntry) { e.Label = separator }, false},
		{"a well-formed alias", func(e *projectEntry) { e.Aliases = []string{"/elsewhere/the-repo"} }, true},
		{"a relative alias", func(e *projectEntry) { e.Aliases = []string{"elsewhere"} }, false},
		{"an unclean alias", func(e *projectEntry) { e.Aliases = []string{"/a/./b"} }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := validEntry()
			c.mutate(&e)
			if got := e.valid(); got != c.want {
				t.Errorf("valid() = %v, want %v for %+v", got, c.want, e)
			}
		})
	}
}

// A table that does not parse is not a table with no entries. Treating it as
// empty would hand every repository a new identity on the next scan, which is
// the one failure the resolution table cannot recover from.
func TestInvalidJsonIsAnErrorAndTheFileIsNotRewritten(t *testing.T) {
	p := testPaths(t)
	const content = "{\"version\": 1, \"projects\": [ trailing\n"
	writeProjectsJSON(t, p, content)

	table, dropped, err := readProjects(p.ProjectsFile)
	if err == nil {
		t.Fatalf("readProjects() = (%d entries, %d dropped, nil), want an error", len(table.Projects), dropped)
	}
	if got := readFileOrFail(t, p.ProjectsFile); got != content {
		t.Error("projects.json was rewritten from a failed parse")
	}
}

// The parse error travels as far as the user's terminal, and this file's content
// is repository paths. The offset says where to look without quoting what is
// there.
func TestInvalidJsonErrorDoesNotEchoTheFileContent(t *testing.T) {
	p := testPaths(t)
	const secret = "wake-private-repository-name"
	writeProjectsJSON(t, p, "{\"version\": 1, \"projects\": [{\"root\": \"/x/"+secret+"\" oops}]}\n")

	_, _, err := readProjects(p.ProjectsFile)
	if err == nil {
		t.Fatal("readProjects() = nil, want an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error %q echoes the file's content", err)
	}
}

// A future format read as this one would silently produce different identities,
// with no error and no way back. Refusing to guess is the only safe reading.
func TestAnUnknownTableVersionIsAnError(t *testing.T) {
	for _, version := range []string{"2", "0"} {
		t.Run(version, func(t *testing.T) {
			p := testPaths(t)
			content := `{"version": ` + version + `, "projects": []}` + "\n"
			writeProjectsJSON(t, p, content)

			if _, _, err := readProjects(p.ProjectsFile); err == nil {
				t.Fatal("readProjects() = nil, want an error for an unknown version")
			}
			if got := readFileOrFail(t, p.ProjectsFile); got != content {
				t.Error("projects.json was rewritten")
			}
		})
	}
}

// Every write stamps the version, so a table this build wrote is always one it
// can read back.
func TestWriteStampsTheVersionAndRoundTrips(t *testing.T) {
	p := testPaths(t)
	want := validEntry()
	want.Aliases = []string{"/elsewhere/the-repo"}
	want.CaseInsensitive = true

	if err := writeProjects(p.ProjectsFile, projectsFile{Projects: []projectEntry{want}}); err != nil {
		t.Fatalf("writeProjects() = %v", err)
	}

	var onDisk projectsFile
	if err := json.Unmarshal([]byte(readFileOrFail(t, p.ProjectsFile)), &onDisk); err != nil {
		t.Fatalf("the written file does not parse: %v", err)
	}
	if onDisk.Version != projectsVersion {
		t.Errorf("version on disk = %d, want %d", onDisk.Version, projectsVersion)
	}

	table, dropped, err := readProjects(p.ProjectsFile)
	if err != nil {
		t.Fatalf("readProjects() = %v", err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 — writeProjects wrote an entry readProjects rejects", dropped)
	}
	if len(table.Projects) != 1 || !reflect.DeepEqual(table.Projects[0], want) {
		t.Errorf("round trip = %+v, want %+v", table.Projects, want)
	}
}

// The write is atomic so a crash cannot leave a half-written table, and the
// temporary file has to be gone either way: a leftover projects-*.json in the
// data root is a file holding real paths that nothing will ever clean up.
func TestWriteLeavesNoTempFile(t *testing.T) {
	p := testPaths(t)

	for range 3 {
		if err := writeProjects(p.ProjectsFile, projectsFile{Projects: []projectEntry{validEntry()}}); err != nil {
			t.Fatalf("writeProjects() = %v", err)
		}
	}

	entries, err := os.ReadDir(p.DataDir)
	if err != nil {
		t.Fatalf("reading the data root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "projects.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the data root holds %v, want only projects.json", names)
	}
}

// The table is append-only (ADR-0019 §9): a rewrite keeps every recorded entry
// in place, because T071's "an already-discovered repository keeps its existing
// identity" rests on nothing ever being reassigned.
func TestWritePreservesRecordedEntriesAndTheirOrder(t *testing.T) {
	p := testPaths(t)
	first := validEntry()
	second := validEntry()
	second.ID = strings.Repeat("cd", idHexLen/2)
	second.Label = "another"
	second.Root = "/somewhere/another"

	if err := writeProjects(p.ProjectsFile, projectsFile{Projects: []projectEntry{first}}); err != nil {
		t.Fatalf("writeProjects() = %v", err)
	}
	table, _, readErr := readProjects(p.ProjectsFile)
	if readErr != nil {
		t.Fatalf("readProjects() = %v", readErr)
	}
	table.Projects = append(table.Projects, second)
	if err := writeProjects(p.ProjectsFile, table); err != nil {
		t.Fatalf("writeProjects() = %v", err)
	}

	table, _, readErr = readProjects(p.ProjectsFile)
	if readErr != nil {
		t.Fatalf("readProjects() = %v", readErr)
	}
	if len(table.Projects) != 2 {
		t.Fatalf("table holds %d entries, want 2", len(table.Projects))
	}
	if !reflect.DeepEqual(table.Projects[0], first) || !reflect.DeepEqual(table.Projects[1], second) {
		t.Errorf("table = %+v, want [%+v %+v]", table.Projects, first, second)
	}
}
