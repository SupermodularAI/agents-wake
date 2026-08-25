package config

// The whole remote.* configuration surface: one key, registered in every build.
//
// The endpoint, the enabled flag and the credential are not here and never will
// be. They live in the 0600 remote-auth store, because config.toml is the file
// people paste into a bug report and a secret that can reach it is a secret that
// will (ADR-0028, ADR-0030). What is configuration here is a sizing knob and
// nothing else.
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
// KindDuration is reused rather than extended, so this key adds no new Kind.
func init() {
	register(Key{
		Name:        "remote.min_interval",
		Kind:        KindDuration,
		Default:     "15m",
		Provisional: true,
	})
}
