package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/platform"
	"github.com/SupermodularAI/agents-wake/internal/version"
)

// Every published platform must resolve, so the check here cannot drift from the
// set internal/platform announces — a pair CI builds and .goreleaser.yaml
// publishes but update refuses would be a release nobody can install.
func TestSupportedAcceptsEveryPublishedPlatform(t *testing.T) {
	for _, goos := range platform.OS() {
		for _, goarch := range platform.Arch() {
			if err := supported(goos, goarch); err != nil {
				t.Errorf("supported(%q, %q) = %v, want nil", goos, goarch, err)
			}
		}
	}
}

func TestSupportedRefusesAPlatformWithNoPublishedAsset(t *testing.T) {
	for _, pair := range [][2]string{{"windows", "amd64"}, {"linux", "386"}} {
		err := supported(pair[0], pair[1])
		if err == nil {
			t.Fatalf("supported(%q, %q) = nil, want a refusal", pair[0], pair[1])
		}
		// The rejected pair and the supported set both appear, because the whole
		// point of refusing here rather than downloading is that the user is told
		// what wake publishes instead of shown curl's 404.
		for _, want := range []string{pair[0], pair[1], platform.Description()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("supported(%q, %q) error = %q, missing %q", pair[0], pair[1], err, want)
			}
		}
	}
}

// GoReleaser's .Version is the tag without its leading v, which is why
// install.sh writes ${version#v}. The asset name has to replicate that exactly:
// a name with the v in it names a file no release contains.
func TestAssetNameMatchesTheGoreleaserTemplate(t *testing.T) {
	for _, c := range []struct {
		tag, goos, goarch, want string
	}{
		{"v1.2.3", "darwin", "arm64", "wake_1.2.3_darwin_arm64.tar.gz"},
		{"v0.1.0", "linux", "amd64", "wake_0.1.0_linux_amd64.tar.gz"},
	} {
		got, err := assetName(c.tag, c.goos, c.goarch)
		if err != nil {
			t.Fatalf("assetName(%q, %q, %q) error = %v", c.tag, c.goos, c.goarch, err)
		}
		if got != c.want {
			t.Errorf("assetName(%q, %q, %q) = %q, want %q", c.tag, c.goos, c.goarch, got, c.want)
		}
	}
}

func TestAssetNameRefusesAnUnsupportedPlatform(t *testing.T) {
	got, err := assetName("v1.0.0", "windows", "amd64")
	if err == nil {
		t.Fatalf("assetName on windows = %q, want a refusal", got)
	}
	if got != "" {
		t.Errorf("assetName on windows returned %q alongside its error, want an empty name", got)
	}
}

func TestCompareDistinguishesUntaggedCurrentAndOutdated(t *testing.T) {
	for _, c := range []struct {
		running, latest string
		want            Status
	}{
		{version.Untagged, "v1.0.0", StatusUntagged},
		{"v1.0.0", "v1.0.0", StatusCurrent},
		{"v0.9.0", "v1.0.0", StatusOutdated},
	} {
		if got := Compare(c.running, c.latest); got != c.want {
			t.Errorf("Compare(%q, %q) = %v, want %v", c.running, c.latest, got, c.want)
		}
	}
}

// fakeFetcher serves bytes from memory. It exists so this package's tests can
// exercise the whole sequence with no network and no HTTP client anywhere
// (ADR-0026 covers tests too).
type fakeFetcher struct {
	effective string
	files     map[string][]byte
	resolves  int
	downloads []string
	fail      error
}

func (f *fakeFetcher) EffectiveURL(string) (string, error) {
	f.resolves++
	if f.fail != nil {
		return "", f.fail
	}
	return f.effective, nil
}

func (f *fakeFetcher) Download(url, dest string) error {
	f.downloads = append(f.downloads, url)
	body, ok := f.files[url]
	if !ok {
		return fmt.Errorf("no such asset: %s", url)
	}
	return os.WriteFile(dest, body, 0o600)
}

func TestLatestReadsTheTagFromTheRedirectTarget(t *testing.T) {
	fake := &fakeFetcher{effective: "https://github.com/" + Repo + "/releases/tag/v1.2.3"}
	updater := Updater{Fetch: fake, GOOS: platform.OS()[0], GOARCH: platform.Arch()[0]}
	tag, err := updater.Latest()
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if tag != "v1.2.3" {
		t.Errorf("Latest() = %q, want %q", tag, "v1.2.3")
	}
	// One resolve and no download: --check reaches the network once and pulls
	// nothing (ADR-0026).
	if fake.resolves != 1 {
		t.Errorf("Latest() resolved %d times, want 1", fake.resolves)
	}
	if len(fake.downloads) != 0 {
		t.Errorf("Latest() downloaded %v, want nothing", fake.downloads)
	}
}

// install.sh refuses anything that is not v-prefixed, because a redirect that
// lands somewhere else means there is no release, not that the release is called
// "releases".
func TestLatestRejectsARedirectThatNamesNoTag(t *testing.T) {
	effective := "https://github.com/" + Repo + "/releases"
	updater := Updater{Fetch: &fakeFetcher{effective: effective}, GOOS: platform.OS()[0], GOARCH: platform.Arch()[0]}
	_, err := updater.Latest()
	if err == nil {
		t.Fatal("Latest() on a redirect naming no tag = nil, want an error")
	}
	if !strings.Contains(err.Error(), effective) {
		t.Errorf("error = %q, want it to name the URL it landed on", err)
	}
}

func TestLatestPropagatesAResolveFailure(t *testing.T) {
	fake := &fakeFetcher{fail: errors.New("curl exited 6")}
	updater := Updater{Fetch: fake, GOOS: platform.OS()[0], GOARCH: platform.Arch()[0]}
	if _, err := updater.Latest(); err == nil {
		t.Fatal("Latest() with a failing fetcher = nil, want the failure")
	}
}

// The platform refusal comes before the network, so a machine with no published
// asset is told that plainly instead of being shown a release it cannot install.
func TestLatestRefusesAnUnsupportedPlatformBeforeTouchingTheNetwork(t *testing.T) {
	fake := &fakeFetcher{effective: "https://github.com/" + Repo + "/releases/tag/v1.2.3"}
	updater := Updater{Fetch: fake, GOOS: "windows", GOARCH: platform.Arch()[0]}
	if _, err := updater.Latest(); err == nil {
		t.Fatal("Latest() on an unsupported platform = nil, want a refusal")
	}
	if fake.resolves != 0 {
		t.Errorf("Latest() resolved %d times before refusing, want 0", fake.resolves)
	}
}

// tarGz builds a gzipped tar of files, so a test can produce a real release
// archive without one existing.
func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("WriteHeader(%q): %v", name, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing the gzip writer: %v", err)
	}
	return buf.Bytes()
}

// checksumLine renders the `<sha256>  <name>` line GoReleaser publishes.
func checksumLine(name string, archive []byte) string {
	return fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), name)
}

// writeArchiveAndChecksums lays a downloaded pair out in dir the way Apply does,
// returning the two paths.
func writeArchiveAndChecksums(t *testing.T, dir, name string, archive []byte, checksums string) (string, string) {
	t.Helper()
	archivePath := filepath.Join(dir, name)
	checksumsPath := filepath.Join(dir, checksumsName)
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatalf("writing the archive: %v", err)
	}
	if err := os.WriteFile(checksumsPath, []byte(checksums), 0o600); err != nil {
		t.Fatalf("writing the checksums: %v", err)
	}
	return archivePath, checksumsPath
}

func TestVerifyAcceptsAMatchingDigest(t *testing.T) {
	archive := tarGz(t, map[string][]byte{binaryName: []byte("the new binary")})
	name := "wake_1.2.3_test_arch.tar.gz"
	archivePath, checksumsPath := writeArchiveAndChecksums(t, t.TempDir(), name, archive, checksumLine(name, archive))
	if err := verify(archivePath, checksumsPath, name); err != nil {
		t.Errorf("verify() on a matching digest = %v, want nil", err)
	}
}

func TestVerifyRefusesATamperedChecksumLine(t *testing.T) {
	archive := tarGz(t, map[string][]byte{binaryName: []byte("the new binary")})
	name := "wake_1.2.3_test_arch.tar.gz"
	line := checksumLine(name, archive)
	// One flipped hex character: the published digest no longer describes these
	// bytes, and there is no way to tell which of the two was tampered with — so
	// neither is trusted.
	flipped := byte('0')
	if line[0] == '0' {
		flipped = '1'
	}
	archivePath, checksumsPath := writeArchiveAndChecksums(t, t.TempDir(), name, archive, string(flipped)+line[1:])
	err := verify(archivePath, checksumsPath, name)
	if err == nil {
		t.Fatal("verify() on a tampered checksum line = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("error = %q, want it to name the asset that failed", err)
	}
}

func TestVerifyRefusesACorruptedArchive(t *testing.T) {
	archive := tarGz(t, map[string][]byte{binaryName: []byte("the new binary")})
	name := "wake_1.2.3_test_arch.tar.gz"
	line := checksumLine(name, archive)
	archivePath, checksumsPath := writeArchiveAndChecksums(t, t.TempDir(), name, append(archive, 'x'), line)
	if err := verify(archivePath, checksumsPath, name); err == nil {
		t.Fatal("verify() on a corrupted download = nil, want a refusal")
	}
}

// A missing line is a failure too: an archive the release published no digest for
// is unverified, not verified.
func TestVerifyRefusesWhenTheChecksumFileHasNoEntryForTheAsset(t *testing.T) {
	archive := tarGz(t, map[string][]byte{binaryName: []byte("the new binary")})
	name := "wake_1.2.3_test_arch.tar.gz"
	other := checksumLine("wake_1.2.3_other_arch.tar.gz", archive)
	archivePath, checksumsPath := writeArchiveAndChecksums(t, t.TempDir(), name, archive, other)
	err := verify(archivePath, checksumsPath, name)
	if err == nil {
		t.Fatal("verify() with no line for the asset = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("error = %q, want it to name the asset with no published digest", err)
	}
}

// A real checksums.txt lists every artefact of the release, so matching the right
// line among several is the normal case rather than an edge one.
func TestVerifyIgnoresUnrelatedLines(t *testing.T) {
	archive := tarGz(t, map[string][]byte{binaryName: []byte("the new binary")})
	name := "wake_1.2.3_test_arch.tar.gz"
	other := tarGz(t, map[string][]byte{binaryName: []byte("some other platform's binary")})
	checksums := checksumLine("wake_1.2.3_a_b.tar.gz", other) +
		checksumLine(name, archive) +
		checksumLine("wake_1.2.3_c_d.tar.gz", other)
	archivePath, checksumsPath := writeArchiveAndChecksums(t, t.TempDir(), name, archive, checksums)
	if err := verify(archivePath, checksumsPath, name); err != nil {
		t.Errorf("verify() against a full checksums.txt = %v, want nil", err)
	}
}

// The three files .goreleaser.yaml puts in a release archive, so the test
// exercises the same shape a real download has rather than a bare binary.
func TestExtractWritesTheBinaryFromTheArchive(t *testing.T) {
	payload := []byte("the new binary")
	archive := tarGz(t, map[string][]byte{
		binaryName:  payload,
		"LICENSE":   []byte("a licence"),
		"README.md": []byte("# wake"),
	})
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "wake_1.2.3_test_arch.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatalf("writing the archive: %v", err)
	}
	into := t.TempDir()

	got, err := extract(archivePath, into)
	if err != nil {
		t.Fatalf("extract() error = %v", err)
	}
	if want := filepath.Join(into, binaryName); got != want {
		t.Errorf("extract() = %q, want %q", got, want)
	}
	written, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("reading the extracted binary: %v", err)
	}
	if !bytes.Equal(written, payload) {
		t.Errorf("extracted %q, want %q", written, payload)
	}
	// Only the binary: a release's LICENSE and README are not this command's to
	// scatter around the filesystem.
	entries, err := os.ReadDir(into)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("extract() wrote %d entries, want only %s", len(entries), binaryName)
	}
}

func TestExtractRejectsAnArchiveWithoutTheBinary(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "wake_1.2.3_test_arch.tar.gz")
	if err := os.WriteFile(archivePath, tarGz(t, map[string][]byte{"LICENSE": []byte("a licence")}), 0o600); err != nil {
		t.Fatalf("writing the archive: %v", err)
	}
	_, err := extract(archivePath, t.TempDir())
	if err == nil {
		t.Fatal("extract() on an archive with no binary = nil, want an error")
	}
	if !strings.Contains(err.Error(), binaryName) {
		t.Errorf("error = %q, want it to name the missing %s", err, binaryName)
	}
}

func TestExtractRejectsANonGzipArchive(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "wake_1.2.3_test_arch.tar.gz")
	if err := os.WriteFile(archivePath, []byte("not a gzip stream at all"), 0o600); err != nil {
		t.Fatalf("writing the archive: %v", err)
	}
	if _, err := extract(archivePath, t.TempDir()); err == nil {
		t.Fatal("extract() on a non-gzip file = nil, want an error")
	}
}

// The destination is composed, never taken from the archive, so a crafted entry
// name cannot write outside the directory it was handed.
func TestExtractWritesOutsideNothingNamedByTheArchive(t *testing.T) {
	payload := []byte("the new binary")
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "wake_1.2.3_test_arch.tar.gz")
	if err := os.WriteFile(archivePath, tarGz(t, map[string][]byte{"../../escaped/" + binaryName: payload}), 0o600); err != nil {
		t.Fatalf("writing the archive: %v", err)
	}
	into := t.TempDir()

	got, err := extract(archivePath, into)
	if err != nil {
		t.Fatalf("extract() error = %v", err)
	}
	if want := filepath.Join(into, binaryName); got != want {
		t.Errorf("extract() = %q, want the composed path %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(into, "..", "..", "escaped")); !os.IsNotExist(err) {
		t.Errorf("the archive's own path was honoured: Stat(escaped) error = %v, want not-exist", err)
	}
}

// serveRelease builds a fake serving one release for this machine's announced
// platform: the archive and the checksums.txt that verifies it.
//
// The platform comes from internal/platform rather than runtime, so the test
// asserts against the set wake publishes instead of the runner it happens to be
// on.
func serveRelease(t *testing.T, tag string, payload []byte) (*fakeFetcher, string) {
	t.Helper()
	name, err := assetName(tag, platform.OS()[0], platform.Arch()[0])
	if err != nil {
		t.Fatalf("assetName() error = %v", err)
	}
	archive := tarGz(t, map[string][]byte{binaryName: payload, "LICENSE": []byte("a licence")})
	return &fakeFetcher{
		effective: "https://github.com/" + Repo + "/releases/tag/" + tag,
		files: map[string][]byte{
			assetURL(tag, name):          archive,
			assetURL(tag, checksumsName): []byte(checksumLine(name, archive)),
		},
	}, name
}

// installedBinary writes a stand-in for the running binary, so a test can watch
// it be replaced — or watch it not be.
func installedBinary(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), binaryName)
	if err := os.WriteFile(path, []byte(content), binaryMode); err != nil {
		t.Fatalf("writing the stand-in binary: %v", err)
	}
	return path
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s holds %q, want %q", path, got, want)
	}
}

func updaterFor(fake Fetcher, running, executable string) Updater {
	return Updater{
		Fetch:      fake,
		GOOS:       platform.OS()[0],
		GOARCH:     platform.Arch()[0],
		Running:    running,
		Executable: executable,
	}
}

func TestApplyReplacesTheBinaryWithTheVerifiedRelease(t *testing.T) {
	fake, _ := serveRelease(t, "v1.2.3", []byte("the new binary"))
	executable := installedBinary(t, "the old binary")

	result, err := updaterFor(fake, "v1.0.0", executable).Apply()
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if want := (Result{Tag: "v1.2.3", Replaced: true}); result != want {
		t.Errorf("Apply() = %+v, want %+v", result, want)
	}
	assertContent(t, executable, "the new binary")
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	// Executable, the way install.sh's `install -m 0755` leaves it: a replacement
	// that lost the bit would report success and never run again.
	if info.Mode().Perm() != binaryMode {
		t.Errorf("replaced binary mode = %v, want %v", info.Mode().Perm(), binaryMode)
	}
}

func TestApplyIsANoOpWhenAlreadyOnTheLatestRelease(t *testing.T) {
	fake, _ := serveRelease(t, "v1.2.3", []byte("the new binary"))
	executable := installedBinary(t, "the old binary")

	result, err := updaterFor(fake, "v1.2.3", executable).Apply()
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if want := (Result{Tag: "v1.2.3", Replaced: false}); result != want {
		t.Errorf("Apply() = %+v, want %+v", result, want)
	}
	// Nothing downloaded and nothing written: "already on the latest release"
	// means the command did no work, not that it redid it quietly.
	if len(fake.downloads) != 0 {
		t.Errorf("Apply() downloaded %v, want nothing", fake.downloads)
	}
	assertContent(t, executable, "the old binary")
}

func TestApplyRefusesATamperedChecksumAndLeavesTheBinaryUntouched(t *testing.T) {
	fake, name := serveRelease(t, "v1.2.3", []byte("the new binary"))
	line := fake.files[assetURL("v1.2.3", checksumsName)]
	flipped := byte('0')
	if line[0] == '0' {
		flipped = '1'
	}
	fake.files[assetURL("v1.2.3", checksumsName)] = append([]byte{flipped}, line[1:]...)
	executable := installedBinary(t, "the old binary")

	_, err := updaterFor(fake, "v1.0.0", executable).Apply()
	if err == nil {
		t.Fatal("Apply() with a tampered checksum = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("error = %q, want it to name the asset that failed", err)
	}
	assertContent(t, executable, "the old binary")
}

func TestApplyRefusesACorruptedDownloadAndLeavesTheBinaryUntouched(t *testing.T) {
	fake, _ := serveRelease(t, "v1.2.3", []byte("the new binary"))
	name, err := assetName("v1.2.3", platform.OS()[0], platform.Arch()[0])
	if err != nil {
		t.Fatalf("assetName() error = %v", err)
	}
	fake.files[assetURL("v1.2.3", name)] = append(fake.files[assetURL("v1.2.3", name)], 'x')
	executable := installedBinary(t, "the old binary")

	if _, err := updaterFor(fake, "v1.0.0", executable).Apply(); err == nil {
		t.Fatal("Apply() with a corrupted download = nil, want a refusal")
	}
	assertContent(t, executable, "the old binary")
}

// An archive that verifies but carries no binary must not leave the old one
// replaced by nothing, or reported as replaced.
func TestApplyRefusesAnArchiveWithoutTheBinaryAndLeavesTheBinaryUntouched(t *testing.T) {
	name, err := assetName("v1.2.3", platform.OS()[0], platform.Arch()[0])
	if err != nil {
		t.Fatalf("assetName() error = %v", err)
	}
	archive := tarGz(t, map[string][]byte{"LICENSE": []byte("a licence")})
	fake := &fakeFetcher{
		effective: "https://github.com/" + Repo + "/releases/tag/v1.2.3",
		files: map[string][]byte{
			assetURL("v1.2.3", name):          archive,
			assetURL("v1.2.3", checksumsName): []byte(checksumLine(name, archive)),
		},
	}
	executable := installedBinary(t, "the old binary")

	if _, err := updaterFor(fake, "v1.0.0", executable).Apply(); err == nil {
		t.Fatal("Apply() on an archive with no binary = nil, want a refusal")
	}
	assertContent(t, executable, "the old binary")
}

func TestApplyRefusesAnUnsupportedPlatformBeforeTouchingTheNetwork(t *testing.T) {
	fake, _ := serveRelease(t, "v1.2.3", []byte("the new binary"))
	executable := installedBinary(t, "the old binary")
	updater := updaterFor(fake, "v1.0.0", executable)
	updater.GOARCH = "386"

	if _, err := updater.Apply(); err == nil {
		t.Fatal("Apply() on an unsupported platform = nil, want a refusal")
	}
	if fake.resolves != 0 || len(fake.downloads) != 0 {
		t.Errorf("Apply() resolved %d times and downloaded %v before refusing, want 0 and nothing", fake.resolves, fake.downloads)
	}
	assertContent(t, executable, "the old binary")
}

// A wake reached through a symlink must have the real file replaced: overwriting
// the link with a regular file would break every other name pointing at it.
func TestApplyReplacesTheFileASymlinkPointsAt(t *testing.T) {
	fake, _ := serveRelease(t, "v1.2.3", []byte("the new binary"))
	target := installedBinary(t, "the old binary")
	link := filepath.Join(t.TempDir(), binaryName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	if _, err := updaterFor(fake, "v1.0.0", link).Apply(); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat() error = %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file, want the link left alone")
	}
	assertContent(t, target, "the new binary")
}

// The download directory is this command's, and a failure is exactly when a
// leftover would go unnoticed.
func TestApplyLeavesNoTemporaryDirectoryBehind(t *testing.T) {
	pattern := filepath.Join(os.TempDir(), "wake-update-*")
	before, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}

	fake, _ := serveRelease(t, "v1.2.3", []byte("the new binary"))
	if _, err = updaterFor(fake, "v1.0.0", installedBinary(t, "the old binary")).Apply(); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	broken, _ := serveRelease(t, "v1.2.3", []byte("the new binary"))
	delete(broken.files, assetURL("v1.2.3", checksumsName))
	if _, err = updaterFor(broken, "v1.0.0", installedBinary(t, "the old binary")).Apply(); err == nil {
		t.Fatal("Apply() with no checksums published = nil, want a refusal")
	}

	after, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("Apply() left %d temporary directories behind", len(after)-len(before))
	}
}

func TestIsUntaggedTracksTheVersionSentinel(t *testing.T) {
	if !IsUntagged(version.Untagged) {
		t.Errorf("IsUntagged(%q) = false, want true", version.Untagged)
	}
	if IsUntagged("v1.0.0") {
		t.Error(`IsUntagged("v1.0.0") = true, want false`)
	}
}
