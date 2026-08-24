//go:build !remote

// The key assertions that are about what the *default* build registers, kept
// with the build that has that property.
//
// They are exhaustive — "exactly these seven names", "no remote. prefix",
// "exactly these two provisional keys" — so under //go:build remote they would
// fail on the eighth key that build is meant to have. Tagging them is what lets
// the tagged build add a key without weakening the guarantee that the default
// build does not: keys_remote_test.go carries the tagged counterparts, and both
// stay exhaustive rather than becoming "at least".
package config

import (
	"slices"
	"strings"
	"testing"
)

// The config surface is deliberately small (ADR-0014) and this is the test that
// keeps it that way: adding a key without a decision behind it fails here rather
// than shipping. The list is the ticket's settings table, sorted.
func TestKnownKeysAreExactlyTheSevenInADR0014(t *testing.T) {
	want := []string{
		"scan.harnesses",
		"scan.repos",
		"scan.stale_call_timeout",
		"session.idle_timeout",
		"store.retention_raw",
		"store.rollup_after",
		"ui.default_window",
	}
	if got := KeyNames(); !slices.Equal(got, want) {
		t.Errorf("KeyNames() = %v, want %v", got, want)
	}
}

// ADR-0012: remote delivery is compiled out, not configured off. A remote.* key
// visible in the default build would be a setting that cannot do anything.
func TestDefaultBuildExposesNoRemoteKey(t *testing.T) {
	for _, name := range KeyNames() {
		if strings.HasPrefix(name, "remote.") {
			t.Errorf("the default build exposes %q; remote.* keys belong to the tagged build (ADR-0012, T090)", name)
		}
	}
}

// ADR-0015 needs both thresholds tunable and says they need real-world
// calibration, which P3 owns. Until then the uncalibrated fact is API, so T007
// can label it — a value silently presented as calibrated is worse than no
// value.
func TestTheTwoTimeoutsAreMarkedProvisional(t *testing.T) {
	want := []string{"scan.stale_call_timeout", "session.idle_timeout"}

	var got []string
	for _, k := range Keys() {
		if k.Provisional {
			got = append(got, k.Name)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("provisional keys = %v, want %v", got, want)
	}
}
