// Package selfupdate replaces the running binary with the newest published
// release, doing every step that touches the network through a curl subprocess.
//
// ADR-0026 puts the socket in curl rather than in this binary: the Go code here
// builds arguments, compares a checksum, unpacks an archive and performs the
// atomic replacement, and nothing in it opens a connection. The one file that
// runs a subprocess is curl.go, so the property is checkable by reading it.
//
// Every literal below — the redirect form, the archive name, the checksum file
// and the download base — is the one install.sh already uses (ADR-0026: reuse
// its steps rather than reinvent them). curl_test.go asserts they still match.
package selfupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
	"github.com/SupermodularAI/agents-wake/internal/platform"
	"github.com/SupermodularAI/agents-wake/internal/version"
)

// Repo is the GitHub repository releases are published to.
const Repo = "SupermodularAI/agents-wake"

// LatestReleaseURL redirects to the newest release's page; the tag is the last
// segment of the URL it lands on. install.sh resolves the version the same way.
const LatestReleaseURL = "https://github.com/" + Repo + "/releases/latest"

// binaryName is the file inside the release archive, and the name this tool is
// installed under (.goreleaser.yaml builds.binary).
const binaryName = "wake"

// supported reports whether a release asset exists for goos/goarch.
//
// The set comes from internal/platform rather than a list of its own: CI's build
// matrix, .goreleaser.yaml and that package are already held to one set by
// platform's own test, and a fourth copy here would sit outside that guard
// (ADR-0021).
func supported(goos, goarch string) error {
	if slices.Contains(platform.OS(), goos) && slices.Contains(platform.Arch(), goarch) {
		return nil
	}
	return fmt.Errorf("wake publishes no release for %s/%s: supported platforms are %s", goos, goarch, platform.Description())
}

// assetName is the release archive for one tag and platform.
//
// This is .goreleaser.yaml's `wake_{{ .Version }}_{{ .Os }}_{{ .Arch }}` plus
// .tar.gz, where GoReleaser's .Version is the tag without its leading v — the
// same stripping install.sh does with ${version#v}. The platform refusal comes
// first, so an unsupported machine is never handed a URL that would 404.
func assetName(tag, goos, goarch string) (string, error) {
	if err := supported(goos, goarch); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", binaryName, strings.TrimPrefix(tag, "v"), goos, goarch), nil
}

// checksumsName is what .goreleaser.yaml's checksum.name_template publishes.
const checksumsName = "checksums.txt"

// maxChecksums caps the checksum file a release is allowed to publish. It is a
// list of one line per artefact; anything larger is not that file.
const maxChecksums = 1 << 20

// verify fails unless archive's SHA-256 equals the digest checksums.txt
// publishes for name.
//
// A gate, not a warning: nothing is unpacked and nothing is moved until this
// returns nil (ADR-0001, ticket Scope). A missing line is a failure too — an
// archive the release did not publish a digest for is unverified, not verified.
func verify(archivePath, checksumsPath, name string) error {
	// The size is checked before the read rather than after: whatever curl left
	// at this path is not necessarily the file the release publishes, and reading
	// it first would already have spent the memory the cap exists to protect.
	info, err := os.Stat(checksumsPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", checksumsName, err)
	}
	if info.Size() > maxChecksums {
		return fmt.Errorf("%s is %d bytes, which is not a list of checksums", checksumsName, info.Size())
	}
	raw, err := os.ReadFile(checksumsPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", checksumsName, err)
	}

	var published string
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		// GoReleaser writes `<sha256>  <artefact>`, which is the same shape
		// install.sh greps for.
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[1] == name {
			published = fields[0]
			break
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return fmt.Errorf("reading %s: %w", checksumsName, scanErr)
	}
	if published == "" {
		return fmt.Errorf("%s publishes no checksum for %s", checksumsName, name)
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		return fmt.Errorf("hashing %s: %w", name, err)
	}
	if !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), published) {
		return fmt.Errorf("checksum verification failed for %s: the download does not match the published checksum", name)
	}
	return nil
}

// binaryMode is the mode a replaced binary is published at, matching
// install.sh's `install -m 0755`.
const binaryMode fs.FileMode = 0o755

// maxBinary caps what extract will write. A gzip stream can claim any size, and
// this one is a ~15 MB binary; the cap is what keeps a malformed or hostile
// archive from filling the disk before the write fails.
const maxBinary = 256 << 20

// extract writes the wake binary out of archivePath into dir, returning its path.
//
// The destination is always dir/binaryName, never a path taken from the archive,
// so a crafted entry name cannot escape dir. Only a regular file whose base name
// is binaryName is accepted; a release archive that carries no such entry is a
// failure rather than a silent no-op.
func extract(archivePath, dir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", filepath.Base(archivePath), err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", filepath.Base(archivePath), err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return "", fmt.Errorf("reading %s: %w", filepath.Base(archivePath), nextErr)
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != binaryName {
			continue
		}
		dest := filepath.Join(dir, binaryName)
		out, openErr := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, binaryMode)
		if openErr != nil {
			return "", fmt.Errorf("creating %s: %w", dest, openErr)
		}
		// One byte past the cap, so a stream that claims more is detected rather
		// than silently truncated into a binary that would not run.
		written, copyErr := io.Copy(out, io.LimitReader(tr, maxBinary+1))
		closeErr := out.Close()
		switch {
		case copyErr != nil:
			return "", fmt.Errorf("writing %s: %w", dest, copyErr)
		case closeErr != nil:
			return "", fmt.Errorf("writing %s: %w", dest, closeErr)
		case written > maxBinary:
			return "", fmt.Errorf("%s claims a %s larger than %d bytes", filepath.Base(archivePath), binaryName, maxBinary)
		}
		return dest, nil
	}
	return "", fmt.Errorf("%s contains no %s binary", filepath.Base(archivePath), binaryName)
}

// errNoFetcher reports an Updater built without the one field it cannot default.
var errNoFetcher = errors.New("no fetcher configured")

// Updater performs one update, or one check, for a single running binary.
//
// Every field is explicit rather than read from the process: the resolve →
// verify → replace sequence is the part that must be exercisable from a test
// without a network, a release, or a binary the test is willing to overwrite.
type Updater struct {
	// Fetch performs the network steps. Required.
	Fetch Fetcher
	// GOOS and GOARCH select the release asset.
	GOOS, GOARCH string
	// Running is the version the current binary reports (version.Version).
	Running string
	// Executable is the path of the binary Apply replaces. Required by Apply,
	// unused by Latest.
	Executable string
}

// Latest resolves the newest published tag from LatestReleaseURL's redirect,
// downloading nothing.
//
// The platform refusal comes first, before the network: a machine with no
// published asset must be told that plainly rather than be shown an available
// release it cannot install, or a raw 404 from curl (ADR-0021).
func (u Updater) Latest() (string, error) {
	if err := supported(u.GOOS, u.GOARCH); err != nil {
		return "", err
	}
	if u.Fetch == nil {
		return "", errNoFetcher
	}
	effective, err := u.Fetch.EffectiveURL(LatestReleaseURL)
	if err != nil {
		return "", err
	}
	// path, not filepath: this is a URL, and on no supported platform would the
	// separator differ anyway — but the type of the thing being split is what
	// decides which package parses it.
	tag := path.Base(strings.TrimSuffix(effective, "/"))
	// install.sh's `case "$version" in v*) ;; *) fail`: a redirect that lands
	// anywhere but a tag page means there is no release to install, and guessing
	// a name from it would produce a download URL that 404s.
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("could not determine the latest release from %s", effective)
	}
	return tag, nil
}

// downloadBase is the prefix a release's assets hang off, per tag.
const downloadBase = "https://github.com/" + Repo + "/releases/download"

// assetURL is where a named asset of one release hangs.
func assetURL(tag, name string) string { return downloadBase + "/" + tag + "/" + name }

// errNoExecutable reports an Apply with no binary to replace.
var errNoExecutable = errors.New("no executable path to replace")

// Result reports what Apply did.
type Result struct {
	// Tag is the latest published tag, whether or not it was installed.
	Tag string
	// Replaced is false when the running binary was already that tag, in which
	// case nothing was downloaded and nothing was written.
	Replaced bool
}

// Apply installs the latest release over the running binary, or reports that it
// is already installed.
//
// The order is the guarantee: refuse an unsupported platform, resolve the tag,
// stop when it is the one already running, download the archive and the
// checksums, verify, unpack, and only then replace. Anything that fails before
// the last step leaves the existing binary exactly as it was.
//
// Callers handle StatusUntagged themselves; Apply treats any Running that is not
// the latest tag as out of date.
func (u Updater) Apply() (Result, error) {
	tag, err := u.Latest()
	if err != nil {
		return Result{}, err
	}
	// No download and no write when the tag is the one already running: "already
	// on the latest release" has to mean no work happened, not that the same
	// bytes were fetched and re-verified.
	if Compare(u.Running, tag) == StatusCurrent {
		return Result{Tag: tag}, nil
	}
	if u.Executable == "" {
		return Result{}, errNoExecutable
	}
	// A wake reached through a symlink has the file the link points at replaced,
	// not the link overwritten with a regular file — the same link-awareness
	// uninstall's plan already has.
	target, err := filepath.EvalSymlinks(u.Executable)
	if err != nil {
		return Result{}, fmt.Errorf("resolving %s: %w", u.Executable, err)
	}
	name, err := assetName(tag, u.GOOS, u.GOARCH)
	if err != nil {
		return Result{}, err
	}

	dir, err := os.MkdirTemp("", "wake-update-")
	if err != nil {
		return Result{}, fmt.Errorf("creating a temporary directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	archivePath := filepath.Join(dir, name)
	checksumsPath := filepath.Join(dir, checksumsName)
	if err = u.Fetch.Download(assetURL(tag, name), archivePath); err != nil {
		return Result{}, err
	}
	if err = u.Fetch.Download(assetURL(tag, checksumsName), checksumsPath); err != nil {
		return Result{}, err
	}
	if err = verify(archivePath, checksumsPath, name); err != nil {
		return Result{}, err
	}
	binary, err := extract(archivePath, dir)
	if err != nil {
		return Result{}, err
	}

	// The whole binary in memory (~15 MB) buys the tested temp-in-the-same-
	// directory → fsync → rename → dir-sync sequence instead of a second
	// hand-rolled one, and the process is about to be replaced anyway.
	data, err := os.ReadFile(binary)
	if err != nil {
		return Result{}, fmt.Errorf("reading the unpacked %s: %w", binaryName, err)
	}
	if err = atomicfile.Publish(target, data, binaryMode); err != nil {
		return Result{}, err
	}
	return Result{Tag: tag, Replaced: true}, nil
}

// Status is the outcome of comparing the running version against the latest tag.
type Status int

const (
	// StatusUntagged means the running binary carries no release tag, so there
	// is nothing to compare it against.
	StatusUntagged Status = iota
	// StatusCurrent means the running binary already is the latest release.
	StatusCurrent
	// StatusOutdated means the latest release is not the running version.
	StatusOutdated
)

// IsUntagged reports whether running is the un-injected sentinel a build outside
// the release pipelines carries (version.Untagged).
func IsUntagged(running string) bool { return running == version.Untagged }

// Compare places the running version against the latest published tag.
//
// A string comparison and nothing more: ADR-0026 makes --check a string compare,
// so no ordering is inferred from two tags. Anything that is not the latest tag
// and not the untagged sentinel is reported as outdated, which is the honest
// answer for a hand-built binary carrying some other string too.
func Compare(running, latest string) Status {
	switch {
	case IsUntagged(running):
		return StatusUntagged
	case running == latest:
		return StatusCurrent
	default:
		return StatusOutdated
	}
}
