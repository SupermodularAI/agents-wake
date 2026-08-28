package config

import (
	"strings"
	"testing"
)

// One remote.* key, not a group. The flush interval is the only sizing knob a
// document asks for (ADR-0018); batch record counts and byte ceilings have no
// decision behind them and stay constants in internal/remote, per keys.go's rule
// that a key with no decision behind it is scope creep rather than a
// convenience.
//
// The endpoint, the enabled flag and the credential are not keys at all — they
// live in the 0600 remote-auth store and never enter config.toml (ADR-0028), so
// this count is the whole remote configuration surface.
func TestExactlyOneRemoteKeyIsRegistered(t *testing.T) {
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
// be presented anywhere as calibrated. KindDuration is reused deliberately — this
// key adds no new Kind.
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
