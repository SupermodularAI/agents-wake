package config

// Seven of the eight settings this tool has; remote.min_interval is the eighth
// and is registered by keys_remote.go. ADR-0014 keeps the surface deliberately
// small: every key here is one a decision asked for, and a key with no decision
// behind it is scope creep rather than a convenience.
//
// Registration happens here rather than in registry.go so that each key group
// lives in its own file instead of a shared list.
func init() {
	register(
		// Retention is configuration only: T002 records these and enforces
		// nothing, because nothing expires in v1 (ADR-0014). The defaults are
		// what makes that honest — raw events are kept forever and never rolled
		// up unless the user asks.
		Key{
			Name:      "store.retention_raw",
			Kind:      KindDuration,
			Default:   "forever",
			Sentinels: []string{"forever"},
		},
		Key{
			Name:      "store.rollup_after",
			Kind:      KindDuration,
			Default:   "never",
			Sentinels: []string{"never"},
		},
		// ADR-0014's default reporting window. 30d is why parseDuration needs a
		// day suffix at all.
		Key{
			Name:    "ui.default_window",
			Kind:    KindDuration,
			Default: "30d",
		},
		// "All known" means the harnesses this build actually reads — v1 is
		// Claude Code and opencode (plan §5.6). Listing a harness with no
		// adapter would make the downstream "not observed" versus "0"
		// distinction untruthful: the tool would be claiming to have looked
		// somewhere it never looks. The spellings match testdata/<harness>/.
		Key{
			Name:    "scan.harnesses",
			Kind:    KindStringList,
			Default: "claude-code,opencode",
		},
		// The active-repo list. Empty means nothing, not everything
		// (ADR-0019 §2): only consented repos produce records, which is what
		// keeps the hash's domain to consented roots. T002 defines where the
		// list lives; T101 implements the filter.
		Key{
			Name:    "scan.repos",
			Kind:    KindStringList,
			Default: "",
		},
		// ADR-0015 requires both thresholds to be tunable — "they belong in
		// config, not in constants" — and says their values need real-world
		// calibration, which P3 owns. Neither value below is calibrated and
		// neither may be described anywhere as if it were.
		//
		// 30m for an idle session: long enough that a lunch break does not
		// split one session in two, short enough that a day's work is not one
		// session.
		Key{
			Name:        "session.idle_timeout",
			Kind:        KindDuration,
			Default:     "30m",
			Provisional: true,
		},
		// 24h errs deliberately long. Emitting `interrupted` too eagerly is
		// permanent — ADR-0004's dedup means the record cannot be corrected
		// once written — whereas a threshold that is too long only delays a
		// record that is still correct when it arrives.
		Key{
			Name:        "scan.stale_call_timeout",
			Kind:        KindDuration,
			Default:     "24h",
			Provisional: true,
		},
	)
}
