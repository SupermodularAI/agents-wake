//go:build remote

package config

// The whole remote.* configuration surface, present only under //go:build
// remote. ADR-0012 compiles remote delivery out rather than configuring it off,
// so this file is the only place a remote.* key is registered and the default
// build has none — which is why registry.go keeps an open append list instead of
// a closed literal, and why this ticket edits neither registry.go nor keys.go.
//
// One key, not a group. ADR-0018 decides that a flush is always a detached
// child, single-flight by lockfile, with a minimum interval; that interval is
// the one sizing knob a document asks for. The batch record count and the byte
// ceiling have no decision behind them and stay constants in internal/remote,
// under keys.go's rule that a key with no decision behind it is scope creep
// rather than a convenience.
//
// Provisional because no document states a value. ADR-0014's rule for exactly
// this case is that such a threshold ships provisional and labelled as such. 15m
// is a first guess — long enough that a burst of hook-triggered scans does not
// become a burst of flushes, short enough that a day's work is not one delivery
// — and must not be described anywhere as calibrated.
//
// KindDuration is reused rather than extended, so no new Kind reaches the
// default build.
func init() {
	register(Key{
		Name:        "remote.min_interval",
		Kind:        KindDuration,
		Default:     "15m",
		Provisional: true,
	})
}
