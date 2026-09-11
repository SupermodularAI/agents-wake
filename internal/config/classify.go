package config

import "os"

// DirectoryClass says what a working directory one scan could not resolve turned out
// to be. It is exactly one of five, and it is a count's reason and never a directory:
// nothing here carries a path, a directory name, or any spelling of either
// (ADR-0047 §2, ADR-0019 §7, plan §4.2).
//
// Its zero value is DirectoryUnclassified, which is the fail-closed answer ADR-0047 §4
// requires: a directory that is gone, or a git call that failed or timed out, gets no
// classification rather than the nearest bucket — and a path that forgets to classify
// fails into it rather than into a real reason.
type DirectoryClass int

const (
	// DirectoryUnclassified is no answer, and never a default bucket.
	DirectoryUnclassified DirectoryClass = iota
	// DirectoryNotARepository is git's own answer that the directory is inside no
	// working tree. The ordinary case, and no loss: an agent run in a plain directory
	// was never attributable to a repository.
	DirectoryNotARepository
	// DirectoryUnconsentedRepository is a working tree whose repository this machine
	// has not consented — a repository nobody asked Wake to collect, and a linked
	// worktree of one.
	DirectoryUnconsentedRepository
	// DirectoryUnregisteredWorktree is the actionable one: a linked worktree whose
	// repository this machine did consent. The user asked for that repository's
	// sessions and did not get the ones run here (ADR-0047 §1, ADR-0044 §2).
	DirectoryUnregisteredWorktree
	// DirectoryConsented is a directory the recorded table matches. Consent is not
	// what skipped its transcript; the collection window is (ADR-0024, ADR-0025).
	DirectoryConsented
)

// ClassifyUnresolvedDirectory answers, for one directory a scan could not resolve to a
// consented repository, which of the five it is — and does nothing else.
//
// ADR-0047 §1: the probe may run against any directory a scan could not resolve, on
// any machine, with or without a recorded boundary. §2 is the whole of what it may not
// do, and every clause is met here: it registers nothing, consents nothing and records
// nothing; it returns a bounded value and never a path; it changes what is collected
// not at all — a directory it calls DirectoryUnregisteredWorktree is still only
// collected from if RegisterUnderGlobalRoot, under its own unchanged rules, admits it;
// and it reads the directory's own git metadata through probeWorktree, which is the one
// bounded, environment-scrubbed, deadline-carrying call ADR-0044 §2 already specifies.
// No second probe is written.
//
// It is not on the derivation path and must never be: ADR-0019 §1 stands exactly as
// written (ADR-0044 §4), and TestClassificationIsNamedOnlyOffTheDerivationPath is the
// mechanical guard. One classification per distinct directory is the caller's to
// arrange (ADR-0047 §3); this function probes once per call.
func (r *Repos) ClassifyUnresolvedDirectory(dir string) DirectoryClass {
	cleaned, err := lexicalClean(dir)
	if err != nil {
		return DirectoryUnclassified
	}
	// The string-only question first, and it costs no git call: a directory this table
	// matches is one the user consented, so the transcript was skipped by the
	// collection window rather than by consent (ADR-0024, ADR-0025).
	if identity, idErr := r.Identify(cleaned); idErr == nil && identity.Matched {
		return DirectoryConsented
	}
	// Before git, deliberately, and for root discovery's reason turned around: git in
	// a directory that is not there fails the same way it fails when it is not a
	// repository, and ADR-0047 §4 requires the vanished one to yield no classification
	// rather than the nearest bucket. Discovery checks existence first so it cannot
	// invent a root; this checks it first so it cannot invent a reason.
	info, statErr := os.Stat(cleaned)
	if statErr != nil || !info.IsDir() {
		return DirectoryUnclassified
	}
	probe := probeWorktree(cleaned)
	if !probe.answered {
		return DirectoryUnclassified
	}
	if !probe.repository {
		return DirectoryNotARepository
	}
	// Checked after git answers, against the recorded table — never trusted from git's
	// environment (ADR-0044 §2). A parent this machine has not consented, and a working
	// tree that is not a linked worktree at all, are both a repository nobody consented.
	if len(probe.parent) > 0 && r.consentsRepository(probe.parent, r.table.GlobalRoot) {
		return DirectoryUnregisteredWorktree
	}
	return DirectoryUnconsentedRepository
}
