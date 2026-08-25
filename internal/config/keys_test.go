// The exhaustive assertions about what this tool registers: which keys exist at
// all, and which of them are provisional.
//
// They are exhaustive on purpose — "exactly these eight names", "exactly these
// three provisional keys" — so a key added without a decision behind it fails
// here rather than shipping. There is one build, so there is one copy of each
// list; keys_remote_test.go asserts only what is particular to the remote key
// itself.
package config

import (
	"slices"
	"testing"
)

// The config surface is deliberately small (ADR-0014) and this is the test that
// keeps it that way: adding a key without a decision behind it fails here rather
// than shipping. The list is the ticket's settings table, sorted.
func TestKnownKeysAreExactlyTheEight(t *testing.T) {
	want := []string{
		"remote.min_interval",
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

// ADR-0015 needs both scan thresholds tunable and says they need real-world
// calibration, which P3 owns; ADR-0018's flush interval is provisional for the
// same reason — no document states a value. Until then the uncalibrated fact is
// API, so T007 can label it — a value silently presented as calibrated is worse
// than no value.
func TestTheThreeProvisionalKeys(t *testing.T) {
	want := []string{"remote.min_interval", "scan.stale_call_timeout", "session.idle_timeout"}

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
