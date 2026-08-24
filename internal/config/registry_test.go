package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// "All known" has to mean the harnesses this build actually reads. Naming a
// harness with no adapter would make the downstream "not observed" vs "0"
// distinction untruthful: the tool would claim to have looked.
func TestDefaultHarnessesAreTheHarnessesThisBuildReads(t *testing.T) {
	k, ok := lookup("scan.harnesses")
	if !ok {
		t.Fatal("scan.harnesses is not registered")
	}
	if want := "claude-code,opencode"; k.Default != want {
		t.Errorf("scan.harnesses default = %q, want %q", k.Default, want)
	}
	if k.Kind != KindStringList {
		t.Errorf("scan.harnesses kind = %v, want KindStringList", k.Kind)
	}
}

// ADR-0019 §2: an empty active-repo list scans nothing. The default is empty and
// the emptiness is the point, so it is pinned here rather than left to whatever
// the zero value happens to be.
func TestScanReposDefaultsToEmpty(t *testing.T) {
	k, ok := lookup("scan.repos")
	if !ok {
		t.Fatal("scan.repos is not registered")
	}
	if k.Default != "" {
		t.Errorf("scan.repos default = %q, want the empty list", k.Default)
	}
}

func TestKeysIsSortedAndACopy(t *testing.T) {
	got := Keys()
	if !slices.IsSortedFunc(got, func(a, b Key) int { return strings.Compare(a.Name, b.Name) }) {
		t.Errorf("Keys() is not sorted by Name: %v", KeyNames())
	}

	// A caller mutating the returned slice must not be able to redefine a
	// setting for the rest of the process.
	got[0].Default = "mutated"
	got[0].Name = "mutated"
	if again := Keys(); again[0].Default == "mutated" || again[0].Name == "mutated" {
		t.Error("Keys() hands out the package's own slice; a caller can redefine a setting")
	}
}

// Every default has to survive the validation its own key applies. A default
// that its own Kind rejects is a latent bug: it only surfaces the first time a
// user's file is missing that key.
func TestEveryDefaultIsValidUnderItsOwnKind(t *testing.T) {
	for _, k := range Keys() {
		if err := validate(k, k.Default); err != nil {
			t.Errorf("the default for %s is invalid: %v", k.Name, err)
		}
	}
}

// Acceptance item 4: the package rejects an unknown key and the error carries
// the known keys, so T007 only has to print it.
func TestUnknownKeyErrorCarriesEveryKnownKey(t *testing.T) {
	if _, ok := lookup("store.nope"); ok {
		t.Fatal("store.nope is registered; pick a name that is not")
	}

	err := error(&UnknownKeyError{Key: "store.nope", Known: KeyNames()})

	var unknown *UnknownKeyError
	if !errors.As(err, &unknown) {
		t.Fatalf("errors.As(%v, *UnknownKeyError) = false", err)
	}
	if !slices.Equal(unknown.Known, KeyNames()) {
		t.Errorf("Known = %v, want %v", unknown.Known, KeyNames())
	}

	msg := err.Error()
	if !strings.Contains(msg, `"store.nope"`) {
		t.Errorf("message %q does not name the rejected key", msg)
	}
	for _, name := range KeyNames() {
		if !strings.Contains(msg, name) {
			t.Errorf("message %q omits the known key %q", msg, name)
		}
	}
}

// Sentinel words are per key, not global: `forever` is a retention answer, not a
// window. Accepting one everywhere would let `ui.default_window = never` through
// to a renderer that has no meaning for it.
func TestSentinelsAreAcceptedOnlyByTheirOwnKey(t *testing.T) {
	for _, c := range []struct {
		key   string
		value string
		want  bool
	}{
		{"store.retention_raw", "forever", true},
		{"store.rollup_after", "never", true},
		{"store.retention_raw", "never", false},
		{"store.rollup_after", "forever", false},
		{"ui.default_window", "forever", false},
		{"session.idle_timeout", "never", false},
	} {
		k, ok := lookup(c.key)
		if !ok {
			t.Fatalf("%s is not registered", c.key)
		}
		err := validate(k, c.value)
		if (err == nil) != c.want {
			t.Errorf("validate(%s, %q) = %v, want accepted=%v", c.key, c.value, err, c.want)
		}
	}
}

// The reason a value was rejected is what T007 shows the user, and it must not
// quote the value: an InvalidValueError carries the value separately, while
// Problem.Reason travels out of a file whose content this package does not echo.
func TestInvalidValueErrorSeparatesTheValueFromTheReason(t *testing.T) {
	k, ok := lookup("ui.default_window")
	if !ok {
		t.Fatal("ui.default_window is not registered")
	}

	err := validate(k, "bananas")

	var invalid *InvalidValueError
	if !errors.As(err, &invalid) {
		t.Fatalf("validate(ui.default_window, \"bananas\") = %v, want *InvalidValueError", err)
	}
	if invalid.Value != "bananas" {
		t.Errorf("Value = %q, want %q", invalid.Value, "bananas")
	}
	if strings.Contains(invalid.Reason, "bananas") {
		t.Errorf("Reason %q quotes the value; the reason must stand on its own", invalid.Reason)
	}
	if !strings.Contains(invalid.Error(), "bananas") {
		t.Errorf("Error() = %q, want it to name the rejected value", invalid.Error())
	}
}
