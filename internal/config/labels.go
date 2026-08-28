package config

// ProjectLabels returns the readable label recorded for every repository this
// build is willing to resolve against, keyed by repository id.
//
// It is the projection ADR-0033 authorises and nothing wider: the label's value,
// never the root, never an alias, never the boundary. Two callers project it, and
// neither widens that: the OTLP encoder, which puts the value on the wire at flush
// time (ADR-0033 §2), and the local renderers' repository column, which is the
// readable-names purpose this local map was created for (ADR-0014 § Decision).
// projects.json itself still never travels as a file — only these values do, the
// way the hashed id already does.
//
// It reads and never writes, and it reads the salt through readSalt rather than
// loadOrCreateSalt: remote.PreviewFlush documents that `--dry-run` writes
// nothing, and creating the salt as a side effect of inspecting what would leave
// would make that false.
//
// Every failure answers with no labels rather than an error. An unreadable salt,
// an unreadable or unparsable table, and an entry this build refuses to trust all
// mean the same thing on the wire — the hash travels alone — and none of them is
// a reason to fail a flush (ADR-0018: a flush degrades, it does not break). The
// direction is one-way: this can lose a label, never invent or misattribute one.
// `doctor` is where a shrinking table is reported (ADR-0019 §7); this is not.
//
// The labels are returned as recorded, having passed readProjects' floor —
// non-empty and no path separator. The bounded-token check ADR-0033 §3 requires
// on top is applied by the encoder, at the last point before the wire, so a
// stricter rule here cannot accidentally stop a legitimately-labelled repository
// from resolving.
func ProjectLabels(p Paths) map[string]string {
	salt, err := readSalt(p)
	if err != nil {
		return nil
	}
	// The same two steps OpenRepos takes after the salt, minus the creation: a
	// *Repos built on the salt, then the table read through readTable so an entry
	// whose id or match digest this build does not derive is refused here exactly
	// as it is refused for resolution (ADR-0019 §3, §7).
	r := &Repos{paths: p, salt: salt}
	table, _, _, err := r.readTable()
	if err != nil {
		return nil
	}
	labels := make(map[string]string, len(table.Projects))
	for _, entry := range table.Projects {
		labels[entry.ID] = entry.Label
	}
	return labels
}
