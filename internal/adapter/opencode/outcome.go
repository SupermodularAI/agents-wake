package opencode

import "github.com/SupermodularAI/agents-wake/internal/record"

// statusOutcome maps opencode's state.status onto Wake's enum.
//
// It is a positive, closed membership set: every member was measured against a
// real store, and a status this switch does not name is neither terminal nor an
// error — it is a family nobody has measured, so it stays unemitted and is counted
// as blindness rather than absorbed (ADR-0005: adapters "must never guess";
// plan §3.3, §12: format drift fails visibly and never infers structure to keep
// counting).
//
// terminal says whether the invocation is finished (ADR-0015: only terminal events
// are emitted). known says whether this build recognises the status at all, which
// is a different question: pending and running are recognised and unfinished,
// while "cancelled" is simply unmeasured and may be either.
//
// Denials are out of scope, and deliberately rather than by oversight. opencode's
// `permission` table is project-scoped (project_id, action, resource), is not a
// per-invocation denial log, and held zero rows on the machine this was measured
// on — so there is no observable source for record.OutcomeDeniedHarnessRule in
// this schema. Not reading a table that cannot answer decides nothing, and
// ADR-0005 is satisfied because nothing is guessed.
func statusOutcome(status string) (outcome record.Outcome, terminal bool, known bool) {
	switch status {
	case "completed":
		return record.OutcomeOK, true, true
	case "error":
		return record.OutcomeError, true, true
	case "pending", "running":
		return "", false, true
	}
	return "", false, false
}
