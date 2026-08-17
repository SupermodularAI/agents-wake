package atomicfile

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPublishReplacesContentAndKeepsTheRequestedMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	for _, want := range []string{"first", "second"} {
		if err := Publish(path, []byte(want), 0o600); err != nil {
			t.Fatalf("Publish(%q) error = %v", want, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if string(got) != want {
			t.Errorf("content = %q, want %q", got, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat() error = %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %v, want 0600 — a republished file must not inherit the umask", perm)
		}
	}
}

func TestPublishCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "wake", "state.json")

	if err := Publish(path, []byte("content"), 0o600); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat() error = %v, want the file to exist", err)
	}
}

func TestPublishLeavesNoTemporaryFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	occupied := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(occupied, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := Publish(filepath.Join(occupied, "state.json"), []byte("content"), 0o600); err == nil {
		t.Fatal("Publish() error = nil, want a failure when the parent is a regular file")
	}

	assertNoTemporaryFile(t, dir)
}

func TestPublishLeavesThePreviousContentWhenTheReplacementFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := Publish(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	// A read-only directory is the closest a test can get to an interrupted
	// publication: the temporary file cannot be created or renamed, so the
	// publication fails partway with the old file still in place.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := Publish(path, []byte("new"), 0o600); err == nil {
		t.Fatal("Publish() error = nil, want a failure in a read-only directory")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "old" {
		t.Errorf("content = %q, want %q — a failed publication must not damage what is there", got, "old")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	assertNoTemporaryFile(t, dir)
}

func TestPublishLeavesNoTemporaryFileWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	// A destination that is a directory fails at the rename rather than earlier,
	// which is the one failure path that has a temporary file to clean up: the
	// read-only-directory case above cannot even create one.
	occupied := filepath.Join(dir, "state.json")
	if err := os.Mkdir(occupied, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	if err := Publish(occupied, []byte("content"), 0o600); err == nil {
		t.Fatal("Publish() error = nil, want a failure when the destination is a directory")
	}

	assertNoTemporaryFile(t, dir)
}

func TestConcurrentReadersSeeOnlyOneCompleteVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	first := bytes.Repeat([]byte("a"), 4096)
	second := bytes.Repeat([]byte("b"), 8192)
	if err := Publish(path, first, 0o600); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	const rounds = 200
	var writer sync.WaitGroup
	writer.Add(1)
	writeErr := make(chan error, 1)
	go func() {
		defer writer.Done()
		for round := range rounds {
			payload := first
			if round%2 == 1 {
				payload = second
			}
			if err := Publish(path, payload, 0o600); err != nil {
				writeErr <- err
				return
			}
		}
	}()

	torn := 0
	for range rounds * 2 {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if !bytes.Equal(got, first) && !bytes.Equal(got, second) {
			torn++
		}
	}
	writer.Wait()

	select {
	case err := <-writeErr:
		t.Fatalf("Publish() error = %v", err)
	default:
	}
	if torn != 0 {
		t.Errorf("torn reads = %d, want 0 — a reader must see the old file or the new one", torn)
	}
}

func TestModeOfFallsBackWhenTheFileIsAbsent(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.json")

	if got := ModeOf(absent, 0o600); got != 0o600 {
		t.Errorf("ModeOf() = %v, want the 0600 fallback", got)
	}
}

func TestModeOfReportsAnExistingFilesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "theirs.json")
	if err := os.WriteFile(path, []byte("theirs"), 0o640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	if got := ModeOf(path, 0o600); got != 0o640 {
		t.Errorf("ModeOf() = %v, want 0640 — another program's file keeps its own mode", got)
	}
}

// A publication replaces a path with a regular file, so the mode of anything that
// is not one is not a mode it can preserve. A symlink's own bits are 0755 on darwin
// and 0777 on linux, and reporting them would have a caller publish a file the whole
// machine can read — or on linux write — over a file that had been private.
func TestModeOfFallsBackWhenThePathIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "theirs.json")
	if err := os.WriteFile(target, []byte("theirs"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got := ModeOf(link, 0o600); got != 0o600 {
		t.Errorf("ModeOf(a symlink) = %v, want the 0600 fallback — a link's own bits are not a file's mode", got)
	}
	if got := ModeOf(dir, 0o600); got != 0o600 {
		t.Errorf("ModeOf(a directory) = %v, want the 0600 fallback", got)
	}
}

// assertNoTemporaryFile fails when dir holds a leftover publication temporary.
// A leftover is a full copy of whatever was being published that nothing would
// ever clean up.
func assertNoTemporaryFile(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if matched, _ := filepath.Match(tempPattern, entry.Name()); matched {
			t.Errorf("leftover temporary file %q in %s", entry.Name(), dir)
		}
	}
}
