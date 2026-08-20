package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/selfupdate"
	"github.com/SupermodularAI/agents-wake/internal/version"
)

// The release this package's tests serve. The URLs are spelled out rather than
// taken from internal/selfupdate's unexported helpers, so a change there that
// altered where update looks would fail here rather than pass by construction.
const (
	testTag      = "v1.2.3"
	testRunning  = "v1.0.0"
	testDownload = "https://github.com/" + selfupdate.Repo + "/releases/download/" + testTag
)

// fakeFetcher serves a release from memory. Duplicated here rather than exported
// from internal/selfupdate: test scaffolding does not belong in the production
// surface, and nothing in this file may reach the network (ADR-0026).
type fakeFetcher struct {
	effective string
	files     map[string][]byte
	downloads []string
}

func (f *fakeFetcher) EffectiveURL(string) (string, error) { return f.effective, nil }

func (f *fakeFetcher) Download(url, dest string) error {
	f.downloads = append(f.downloads, url)
	body, ok := f.files[url]
	if !ok {
		return fmt.Errorf("no such asset: %s", url)
	}
	return os.WriteFile(dest, body, 0o600)
}

// releaseArchive builds the archive and checksums.txt a release publishes for the
// platform this test is running on.
func releaseArchive(t *testing.T, payload string) (archive []byte, checksums []byte, url string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(payload)
	if err := tw.WriteHeader(&tar.Header{Name: "wake", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("WriteHeader(): %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("Write(): %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing the gzip writer: %v", err)
	}
	name := fmt.Sprintf("wake_%s_%s_%s.tar.gz", strings.TrimPrefix(testTag, "v"), runtime.GOOS, runtime.GOARCH)
	archive = buf.Bytes()
	return archive, []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), name)), testDownload + "/" + name
}

// serveUpdate points the command's three seams at a release in memory, a version
// string, and a throwaway binary, restoring all three afterwards.
//
// No test in this package calls t.Parallel(), so swapping package-level variables
// is safe here the way uninstall_test.go's selfPath swap already is.
func serveUpdate(t *testing.T, running, installed string) (*fakeFetcher, string) {
	t.Helper()
	archive, checksums, url := releaseArchive(t, "the new binary")
	fake := &fakeFetcher{
		effective: "https://github.com/" + selfupdate.Repo + "/releases/tag/" + testTag,
		files: map[string][]byte{
			url:                             archive,
			testDownload + "/checksums.txt": checksums,
		},
	}
	executable := filepath.Join(t.TempDir(), "wake")
	if err := os.WriteFile(executable, []byte(installed), 0o755); err != nil {
		t.Fatalf("writing the stand-in binary: %v", err)
	}
	stubSeams(t, running, func() (selfupdate.Fetcher, error) { return fake, nil }, executable)
	return fake, executable
}

func stubSeams(t *testing.T, running string, fetcher func() (selfupdate.Fetcher, error), executable string) {
	t.Helper()
	savedVersion, savedFetcher, savedSelfPath := runningVersion, newFetcher, selfPath
	t.Cleanup(func() { runningVersion, newFetcher, selfPath = savedVersion, savedFetcher, savedSelfPath })
	runningVersion = func() string { return running }
	newFetcher = fetcher
	selfPath = func() (string, error) { return executable, nil }
}

func TestUpdateCheckReportsAnAvailableRelease(t *testing.T) {
	serveUpdate(t, testRunning, "the old binary")
	out, err := run(t, "update", "--check")
	if err != nil {
		t.Fatalf("update --check returned an error: %v", err)
	}
	if want := testTag + " is available (running " + testRunning + ")\n"; out != want {
		t.Errorf("update --check = %q, want %q", out, want)
	}
}

func TestUpdateCheckReportsAlreadyOnTheLatestRelease(t *testing.T) {
	serveUpdate(t, testTag, "the old binary")
	out, err := run(t, "update", "--check")
	if err != nil {
		t.Fatalf("update --check returned an error: %v", err)
	}
	if want := "already on the latest release (" + testTag + ")\n"; out != want {
		t.Errorf("update --check = %q, want %q", out, want)
	}
}

// --check resolves the tag and stops: it is the one command a user runs to find
// out whether to spend the download at all (ADR-0026).
func TestUpdateCheckDownloadsNothing(t *testing.T) {
	fake, executable := serveUpdate(t, testRunning, "the old binary")
	if _, err := run(t, "update", "--check"); err != nil {
		t.Fatalf("update --check returned an error: %v", err)
	}
	if len(fake.downloads) != 0 {
		t.Errorf("update --check downloaded %v, want nothing", fake.downloads)
	}
	assertBinary(t, executable, "the old binary")
}

// A build with no release tag has nothing to compare against, so it says that —
// and reaches neither PATH nor the network to find out, for update and
// update --check alike.
func TestUpdateOnADevBuildSaysSoAndConstructsNoFetcher(t *testing.T) {
	for _, args := range [][]string{{"update"}, {"update", "--check"}} {
		stubSeams(t, version.Untagged, func() (selfupdate.Fetcher, error) {
			t.Errorf("%v constructed a fetcher on an untagged build", args)
			return nil, nil
		}, filepath.Join(t.TempDir(), "wake"))
		out, err := run(t, args...)
		if err != nil {
			t.Fatalf("%v returned an error: %v", args, err)
		}
		if want := "not a tagged build; nothing to compare against\n"; out != want {
			t.Errorf("%v = %q, want %q", args, out, want)
		}
	}
}

func TestUpdateReportsAMissingCurlPlainly(t *testing.T) {
	stubSeams(t, testRunning, func() (selfupdate.Fetcher, error) { return nil, selfupdate.ErrCurlMissing }, filepath.Join(t.TempDir(), "wake"))
	_, err := run(t, "update")
	if err == nil {
		t.Fatal("update with no curl installed returned nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "curl") {
		t.Errorf("error = %q, want it to name curl so the user knows what to install", err)
	}
}

func TestUpdateReplacesTheBinaryAndReportsTheNewVersion(t *testing.T) {
	_, executable := serveUpdate(t, testRunning, "the old binary")
	out, err := run(t, "update")
	if err != nil {
		t.Fatalf("update returned an error: %v", err)
	}
	if want := "updated to " + testTag + "\n"; out != want {
		t.Errorf("update = %q, want %q", out, want)
	}
	// The replaced bytes are what `wake --version` would read afterwards.
	assertBinary(t, executable, "the new binary")
}

func TestUpdateRefusesATamperedDownloadAndLeavesTheBinaryUntouched(t *testing.T) {
	fake, executable := serveUpdate(t, testRunning, "the old binary")
	fake.files[testDownload+"/checksums.txt"] = []byte("0000000000000000000000000000000000000000000000000000000000000000  " +
		fmt.Sprintf("wake_%s_%s_%s.tar.gz\n", strings.TrimPrefix(testTag, "v"), runtime.GOOS, runtime.GOARCH))
	if _, err := run(t, "update"); err == nil {
		t.Fatal("update with a tampered checksum returned nil, want a refusal")
	}
	assertBinary(t, executable, "the old binary")
}

func TestUpdateWhenAlreadyCurrentChangesNothing(t *testing.T) {
	fake, executable := serveUpdate(t, testTag, "the old binary")
	out, err := run(t, "update")
	if err != nil {
		t.Fatalf("update returned an error: %v", err)
	}
	if want := "already on the latest release (" + testTag + ")\n"; out != want {
		t.Errorf("update = %q, want %q", out, want)
	}
	if len(fake.downloads) != 0 {
		t.Errorf("update downloaded %v while already current, want nothing", fake.downloads)
	}
	assertBinary(t, executable, "the old binary")
}

func TestUpdateRejectsPositionalArguments(t *testing.T) {
	serveUpdate(t, testRunning, "the old binary")
	if _, err := run(t, "update", "now"); err == nil {
		t.Fatal("update with a positional argument returned nil, want an error")
	}
}

func TestUpdateAppearsInHelp(t *testing.T) {
	out, err := run(t, "--help")
	if err != nil {
		t.Fatalf("--help returned an error: %v", err)
	}
	if !strings.Contains(out, "update") {
		t.Errorf("--help = %q, missing the update command", out)
	}
}

// ADR-0026's guard is a human reading the diff; this is the mechanical half of
// it. The source is read because "no socket here" is a property of the file, not
// of any output it produces.
func TestUpdateCommandNamesNoInProcessNetworkClient(t *testing.T) {
	raw := readUpdateSource(t)
	for _, forbidden := range []string{"net/http", "net.Dial", "http.Get", "http.Client"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("update.go names %q; the only outbound network activity is the curl subprocess (ADR-0026)", forbidden)
		}
	}
}

// And the subprocess itself belongs to internal/selfupdate: this layer parses and
// prints (ADR-0001, plan §6.2), the same assertion init_test.go makes for init.go.
func TestUpdateCommandRunsNoSubprocessItself(t *testing.T) {
	raw := readUpdateSource(t)
	for _, forbidden := range []string{"exec.Command", "os/exec"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("update.go names %q; running curl belongs to internal/selfupdate", forbidden)
		}
	}
}

func readUpdateSource(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("update.go")
	if err != nil {
		t.Fatalf("reading update.go: %v", err)
	}
	return raw
}

func assertBinary(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s holds %q, want %q", path, got, want)
	}
}
