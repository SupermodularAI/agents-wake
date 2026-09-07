package config

// ProjectLabels returns, for every repository this build is willing to resolve
// against, the readable label of the repository that id's activity is attributed
// to, keyed by repository id.
//
// For all but a linked git worktree that is the entry's own recorded label. A
// worktree resolves to the label of the repository it belongs to, because its
// records carry the worktree's own hash and always will — derivation never reads
// the relation (ADR-0019 §1, §3) — so the readable name beside that hash is the
// only place the real project can be named. That is what puts the project's name
// in wake.repo_label and in langfuse.trace.name while wake.repo keeps the
// worktree's own hash (ADR-0033 §2); no wire key is added and no id is changed. A
// relation whose target is not an entry of this same table resolves to the entry's
// own label, for the reason rollupOf gives.
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
	own := make(map[string]string, len(table.Projects))
	for _, entry := range table.Projects {
		own[entry.ID] = entry.Label
	}
	// Through the same rollupOf the counted grain uses, so the name a repository's
	// activity is rendered under and the name it travels under cannot disagree.
	rollup := rollupOf(table.Projects)
	labels := make(map[string]string, len(table.Projects))
	for _, entry := range table.Projects {
		label := entry.Label
		if parent, related := rollup[entry.ID]; related {
			label = own[parent]
		}
		labels[entry.ID] = label
	}
	return labels
}

// RepoRollup returns, for every repository this build is willing to resolve
// against that records a relation, the id of the repository its activity is
// counted under: a linked worktree's id mapped onto the id of the repository it
// belongs to. A repository with no relation is absent, and absent means "its own".
//
// It is the render-time half of the worktree relation. The relation is recorded at
// registration and nothing about derivation reads it, so a worktree's records still
// carry the worktree's own hash (ADR-0019 §1, §3) — grouping by wake.repo still
// separates worktrees, which is the debuggable view. This map is what makes the
// *counted* grain the repository, established once in the aggregation layer and
// inherited by all three renderers (ADR-0011).
//
// It carries ids and nothing else: no root, no alias, no label, no path (plan §3.4).
//
// Every failure answers with no rollup rather than an error, for the reason
// ProjectLabels does: an unreadable salt, an unreadable table and an entry this
// build refuses to trust all mean the same thing — nothing rolls up — and none of
// them is a reason to fail a report.
func RepoRollup(p Paths) map[string]string {
	salt, err := readSalt(p)
	if err != nil {
		return nil
	}
	r := &Repos{paths: p, salt: salt}
	table, _, _, err := r.readTable()
	if err != nil {
		return nil
	}
	return rollupOf(table.Projects)
}

// rollupOf is the relation as the two projections read it, in one place so they
// cannot disagree about which relations count.
//
// A relation whose target is not an entry of this same table is dropped: the target
// was removed, or refused on read, and rolling rows onto an id nothing names would
// render a repository nobody can put a name to. Dropping it renders the worktree as
// itself, which is what it did before the relation existed.
//
// One hop, never transitive. The relation is only ever recorded for a linked
// worktree, whose parent is a main checkout and records none of its own, so a chain
// is a table nothing writes — and chasing one would turn a hand-edited cycle into a
// hang.
func rollupOf(entries []projectEntry) map[string]string {
	recorded := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		recorded[entry.ID] = struct{}{}
	}
	rollup := make(map[string]string)
	for _, entry := range entries {
		if entry.BelongsTo == "" {
			continue
		}
		if _, present := recorded[entry.BelongsTo]; !present {
			continue
		}
		rollup[entry.ID] = entry.BelongsTo
	}
	return rollup
}
