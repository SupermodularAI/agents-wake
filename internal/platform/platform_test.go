package platform

import (
	"strings"
	"testing"
)

func TestCheckAcceptsEverySupportedOS(t *testing.T) {
	for _, supported := range OS() {
		if err := Check(supported); err != nil {
			t.Errorf("Check(%q) error = %v, want nil", supported, err)
		}
	}
}

func TestCheckRefusesAnUnsupportedOS(t *testing.T) {
	err := Check("windows")
	if err == nil {
		t.Fatal("Check(\"windows\") = nil, want an error")
	}
	// The refusal has to be actionable on its own: it names the platform that was
	// refused and the whole set that is not, so the reader needs no second lookup.
	for _, want := range []string{"windows", "darwin", "linux", "amd64", "arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check(\"windows\") error = %q, missing %q", err, want)
		}
	}
}

// A refusal message is user-facing output, and plan §4.2 forbids any path in it.
func TestCheckMessageCarriesNoPath(t *testing.T) {
	if got := Check("windows").Error(); strings.Contains(got, "/") {
		t.Errorf("Check(\"windows\") error = %q, want no path separator", got)
	}
}

func TestDescriptionNamesTheWholeSet(t *testing.T) {
	description := Description()
	for _, want := range append(OS(), Arch()...) {
		if !strings.Contains(description, want) {
			t.Errorf("Description() = %q, missing %q", description, want)
		}
	}
}
