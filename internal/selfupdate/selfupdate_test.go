package selfupdate

import (
	"errors"
	"fmt"
	"os"
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

func TestIsUntaggedTracksTheVersionSentinel(t *testing.T) {
	if !IsUntagged(version.Untagged) {
		t.Errorf("IsUntagged(%q) = false, want true", version.Untagged)
	}
	if IsUntagged("v1.0.0") {
		t.Error(`IsUntagged("v1.0.0") = true, want false`)
	}
}
