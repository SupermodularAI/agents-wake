//go:build remote

package config

import (
	"slices"
	"strings"
	"testing"
)

// The tagged mirror of TestKnownKeysAreExactlyTheSevenInADR0014. It is
// exhaustive for the same reason that one is: ADR-0014 keeps the config surface
// small, and the tagged build is not an exemption from that — it is one more key
// than the default build, not an open door.
func TestTaggedBuildRegistersRemoteMinInterval(t *testing.T) {
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

// One remote.* key, not a group. The flush interval is the only sizing knob a
// document asks for (ADR-0018); batch record counts and byte ceilings have no
// decision behind them and stay constants in internal/remote, per keys.go's rule
// that a key with no decision behind it is scope creep rather than a
// convenience.
func TestTaggedBuildAddsExactlyOneRemoteKey(t *testing.T) {
	var got []string
	for _, name := range KeyNames() {
		if strings.HasPrefix(name, "remote.") {
			got = append(got, name)
		}
	}
	if len(got) != 1 {
		t.Errorf("remote.* keys = %v, want exactly one", got)
	}
}

// No document states a value for the flush interval, so ADR-0014's rule for
// exactly that case applies: it ships provisional and labelled, and 15m must not
// be presented anywhere as calibrated. KindDuration is reused deliberately — a
// new Kind would reach the default build, which this ticket does not touch.
func TestRemoteMinIntervalIsAProvisionalDuration(t *testing.T) {
	k, ok := lookup("remote.min_interval")
	if !ok {
		t.Fatal("remote.min_interval is not registered")
	}
	if k.Kind != KindDuration {
		t.Errorf("remote.min_interval kind = %v, want KindDuration", k.Kind)
	}
	if !k.Provisional {
		t.Error("remote.min_interval is not marked provisional; no document states this value")
	}
	if want := "15m"; k.Default != want {
		t.Errorf("remote.min_interval default = %q, want %q", k.Default, want)
	}
	if err := validate(k, k.Default); err != nil {
		t.Errorf("the default for remote.min_interval is invalid: %v", err)
	}
}

// The tagged mirror of TestTheTwoTimeoutsAreMarkedProvisional. The uncalibrated
// set grows by exactly one under the tag, and stays exhaustive so a later key
// cannot join it unnoticed.
func TestTheThreeProvisionalKeysInTheTaggedBuild(t *testing.T) {
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
