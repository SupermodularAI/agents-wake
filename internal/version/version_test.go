package version

import (
	"strings"
	"testing"
)

// The three values are injected via -ldflags, so this asserts the composition
// rather than any particular value: a build that forgets the flags reports the
// defaults, and String() must still name all three.
func TestStringNamesAllThreeValues(t *testing.T) {
	got := String()
	for _, want := range []string{Version, Commit, Date} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}

// The sentinel and the default are the same string by construction, so a package
// that special-cases an un-injected build (internal/selfupdate) can compare
// against Untagged rather than carry a second "dev" literal of its own.
func TestUntaggedIsTheDefaultVersion(t *testing.T) {
	if Version != Untagged {
		t.Errorf("Version default = %q, want the Untagged sentinel %q", Version, Untagged)
	}
}

func TestDefaultsAreNotPlausibleVersions(t *testing.T) {
	// An un-injected binary must be obvious. If these ever become real-looking
	// values, a build with no ldflags becomes indistinguishable from a release.
	if Version != "dev" && !strings.Contains(String(), Version) {
		t.Errorf("Version default changed to %q without String() reflecting it", Version)
	}
	if strings.HasPrefix(Version, "v") {
		t.Errorf("Version default %q looks like a real release tag", Version)
	}
}
