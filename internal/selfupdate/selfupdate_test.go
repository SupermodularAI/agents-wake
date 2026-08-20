package selfupdate

import (
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

func TestIsUntaggedTracksTheVersionSentinel(t *testing.T) {
	if !IsUntagged(version.Untagged) {
		t.Errorf("IsUntagged(%q) = false, want true", version.Untagged)
	}
	if IsUntagged("v1.0.0") {
		t.Error(`IsUntagged("v1.0.0") = true, want false`)
	}
}
