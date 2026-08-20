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
	"fmt"
	"slices"
	"strings"

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
