package claudecode

import (
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// PendingSubagentRun is one anchored-but-unresolved subagent run, projected onto
// a form the caller can carry across scans.
//
// It exists because the run's own buffer cannot be re-derived once the recorded
// collection boundary has moved past the run's entries: a whole-history import
// anchors the run while the session is still open, the scan ends, and every
// later forward-only scan re-reads nothing — so the run the import saw is lost
// unless the import hands it forward. The session closing afterwards is the
// normal case, not an edge: a long dispatch session routinely outlives the
// import that anchored its runs.
//
// Every field is a bounded id, a hash, a timestamp or an enum — the same
// allowlist the record type already is (ADR-0007), so persisting it widens the
// local data boundary by nothing. The source ordinal is deliberately absent: it
// is per scan and means nothing to the next one, and credit/refuse already
// ignore a negative source.
type PendingSubagentRun struct {
	AgentID    record.Identifier `json:"agent_id"`
	SessionID  record.Identifier `json:"session_id"`
	Repo       record.Hash       `json:"repo"`
	UUID       string            `json:"uuid"`
	Timestamp  time.Time         `json:"ts"`
	Entrypoint record.Entrypoint `json:"entrypoint"`
	Version    record.Version    `json:"version,omitempty"`
	// The declaration fold. Name is empty while the run has declared none yet —
	// the same state an anchored run holds inside one scan, carried rather than
	// resolved early, because the name may still arrive in an entry the next
	// scan reads (ADR-0036 §2 judges the run only once the session has closed).
	Name     record.Identifier `json:"name,omitempty"`
	Model    record.Identifier `json:"model,omitempty"`
	DeclUUID string            `json:"decl_uuid,omitempty"`
	DeclTime time.Time         `json:"decl_ts,omitempty"`
}

// PendingChild is one derived terminal record whose parent link was not
// resolvable when its scan closed, projected for the same carry as the run it
// may be waiting on. The record is complete; only the parent is open. Carrying
// it is not an upsert: it has never been emitted, so emitting it later with the
// parent set is the one write ADR-0015 allows.
type PendingChild struct {
	Event   record.Record     `json:"event"`
	AgentID record.Identifier `json:"agent_id,omitempty"`
}

// restoredSource is the source ordinal pending state carries into a scan. It is
// negative so credit and refuse ignore it: a record restored from an earlier
// scan is not credited to any source of this one, and doctor's Skipped counter
// keeps describing this scan's own sources.
const restoredSource = -1

// RestorePending re-anchors runs and re-defers children an earlier scan left
// unresolved, so this scan can resolve them when their sessions close.
//
// It must run before the first Read: the folds it re-enters are the same
// min-folds the walk applies, so a restored run and a re-read entry merge
// exactly as two entries of one scan would (ADR-0004's arrival-independence).
// Restoring after a Read would still be correct for the runs — the fold is
// order-independent — but the children must be in the deferred buffer before
// Close drains it, and before is the only contract that says so plainly.
func (s *Scan) RestorePending(runs []PendingSubagentRun, children []PendingChild) {
	for _, pending := range runs {
		run, seen := s.subagents[pending.AgentID]
		if !seen {
			run = &subagentRun{}
			s.subagents[pending.AgentID] = run
		}
		run.restore(pending)
	}
	for _, child := range children {
		s.deferred = append(s.deferred, deferredChild{
			event:   child.Event,
			source:  restoredSource,
			agentID: child.AgentID,
		})
	}
}

// Pending reports what this scan anchored or derived but could not resolve —
// the runs whose sessions are still open and the children still waiting on a
// parent — in the form RestorePending takes back.
//
// It reads post-Close state: the runs resolveSubagentRuns did not resolve and
// the children resolveDeferredChildren did not emit. Calling it before Close
// reports the same buffers mid-walk, which is a valid but meaningless snapshot.
func (s *Scan) Pending() ([]PendingSubagentRun, []PendingChild) {
	runs := make([]PendingSubagentRun, 0, len(s.subagents))
	for agentID, run := range s.subagents {
		if pending, ok := run.pending(agentID); ok {
			runs = append(runs, pending)
		}
	}
	children := make([]PendingChild, 0, len(s.pendingChildren))
	for _, child := range s.pendingChildren {
		children = append(children, PendingChild{Event: child.event, AgentID: child.agentID})
	}
	return runs, children
}

// restore folds one carried run back into this run's two min-folds. The anchor
// and the declaration re-enter through the same comparisons observeSubagentRun
// applies to a fresh entry, so a run carried across scans and an entry re-read
// by a later scan compete on the same (timestamp, uuid) order — the carried
// value wins exactly when it is the earlier one, and the result is what one
// scan over the union would have produced.
func (r *subagentRun) restore(p PendingSubagentRun) {
	if p.UUID != "" && (!r.anchored || p.Timestamp.Before(r.anchor.timestamp) ||
		(p.Timestamp.Equal(r.anchor.timestamp) && p.UUID < r.anchor.uuid)) {
		r.anchor = subagentAnchor{
			uuid:       p.UUID,
			timestamp:  p.Timestamp,
			sessionID:  p.SessionID,
			repo:       p.Repo,
			entrypoint: p.Entrypoint,
			version:    p.Version,
			source:     restoredSource,
		}
		r.anchored = true
	}
	if p.Name != "" && (!r.declared || p.DeclTime.Before(r.declaration.timestamp) ||
		(p.DeclTime.Equal(r.declaration.timestamp) && p.DeclUUID < r.declaration.uuid)) {
		r.declaration = subagentDeclaration{
			uuid:      p.DeclUUID,
			timestamp: p.DeclTime,
			name:      p.Name,
			model:     p.Model,
		}
		r.declared = true
	}
}

// pending projects this run onto its carried form, reporting false when the
// run has nothing worth carrying: an unanchored run holds no session, no repo
// and no span, so there is nothing a later scan could resolve.
func (r *subagentRun) pending(agentID record.Identifier) (PendingSubagentRun, bool) {
	if !r.anchored {
		return PendingSubagentRun{}, false
	}
	p := PendingSubagentRun{
		AgentID:    agentID,
		SessionID:  r.anchor.sessionID,
		Repo:       r.anchor.repo,
		UUID:       r.anchor.uuid,
		Timestamp:  r.anchor.timestamp,
		Entrypoint: r.anchor.entrypoint,
		Version:    r.anchor.version,
	}
	if r.declared {
		p.Name = r.declaration.name
		p.Model = r.declaration.model
		p.DeclUUID = r.declaration.uuid
		p.DeclTime = r.declaration.timestamp
	}
	return p, true
}
