package claudecode

import (
	"cmp"
	"slices"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// subagentSeparator delimits the two halves of a subagent invocation's source
// identity. It is a group separator, and it is a distinct byte on purpose: a
// token-domain value can contain none of them, a tool call's composed identity carries
// 0x1f, a session_end's carries 0x1e, a typed invocation's carries 0x1c, and a Shape-A
// fallback derives from a bare entry uuid with no separator at all. The five id shapes
// are therefore structurally disjoint — no transcript, hostile or otherwise, can craft a
// subagent id that collides with any other shape (ADR-0004, ADR-0035's Context on what a
// carelessly hashed shape costs).
const subagentSeparator = "\x1d"

// subagentSourceEvent identifies the one record a subagent run ever produces: the
// agent id its own transcript declares, plus a kind-discriminating component.
// ADR-0036 §1's precedence table makes the transcript the canonical source and the
// agent id its key; the kind fills ADR-0004's sequence role, as sessionEndSourceEvent
// does. Never the file path — the path carries the same value (400/400 measured) and
// derivation may not read it (ADR-0019 §1).
func subagentSourceEvent(agentID record.Identifier) record.Identifier {
	return record.Identifier(string(agentID) + subagentSeparator + string(record.KindSubagent))
}

// subagentAnchor is the earliest consented entry of one subagent transcript, by
// (timestamp, uuid), whose entrypoint this build maps. It supplies every dimension
// the record carries that is not the name.
type subagentAnchor struct {
	uuid       string
	timestamp  time.Time
	sessionID  record.Identifier
	repo       record.Hash
	version    record.Version
	entrypoint record.Entrypoint
	// source is the ordinal of the source this anchor was read from, so the record
	// the resolution derives can be credited back to it. Never a path (ADR-0007).
	source int
}

// subagentDeclaration is the earliest consented entry, by (timestamp, uuid), whose
// attributionAgent survives the name grammar. It is a second fold and not a field on
// the anchor because the two values are not co-located: agentId is on every entry of
// a subagent transcript and attributionAgent is on none of the first ones (measured
// 32787/32787 against 0/400). One fold would refuse a subagent whose name arrives
// three entries later — permanently, since ADR-0015 rejects upsert and ADR-0004
// deduplicates the correction away.
type subagentDeclaration struct {
	uuid      string
	timestamp time.Time
	name      record.Identifier
	model     record.Identifier
}

// subagentRun is one subagent transcript's unresolved state: the two folds, each
// with a flag saying whether anything has landed in it yet.
//
// It holds bounded ids, a timestamp, a hash and a source ordinal. No path, no agent
// id in a record field, and no transcript content — the record type is the allowlist
// and so is this buffer (ADR-0007, plan §4.2).
type subagentRun struct {
	anchor      subagentAnchor
	anchored    bool
	declaration subagentDeclaration
	declared    bool
}

// observeSubagentRun folds one usable entry into the run its agentId declares, if it
// declares one at all.
//
// The gates run in this order and for these reasons:
//
// An entry with no agentId is not a subagent transcript entry — a parent transcript
// never carries one (0/39039 measured) — and an id outside the token domain would
// make the composed source identity ambiguous. Both are a clean zero, exactly as
// call treats an unusable block id, and neither is a refusal.
//
// Consent is resolved from the entry's own cwd against the recorded map, as a pure
// string operation with no filesystem access (ADR-0019 §1), and it gates *both*
// folds. A file whose cwd varies — 2 of 400 measured — therefore cannot pair a
// consented anchor with a name read from an unconsented entry: an unconsented entry
// contributes nothing, not a timestamp and not a name, which is the rule
// observeSessionGrain already states (ADR-0016 as narrowed by ADR-0025).
//
// Reading an agentId and a cwd to answer those questions is not persisting either.
func observeSubagentRun(runs map[record.Identifier]*subagentRun, source int, entry transcriptEntry,
	resolve Resolver, names record.Namer) {
	agentID, err := record.BoundedToken(entry.AgentID)
	if err != nil {
		return
	}
	sessionID, err := record.BoundedToken(entry.SessionID)
	if err != nil {
		return
	}
	timestamp := record.NormalizedTimestamp(entry.Timestamp)
	repo, consented := resolve(entry.CWD, timestamp)
	if !consented {
		return
	}
	run, seen := runs[agentID]
	if !seen {
		run = &subagentRun{}
		runs[agentID] = run
	}
	run.anchorEntry(entry, timestamp, sessionID, repo, source)
	run.declare(entry, timestamp, names)
}

// anchorEntry folds one consented entry into this run's anchor: the entrypoint gate,
// then the min by (timestamp, uuid).
//
// The entrypoint gate first, mirroring sessionGrain.anchorEntry: an entrypoint
// outside Wake's vocabulary means this build cannot state that dimension for the
// entry, which is a reason not to anchor on it rather than a reason to disbelieve the
// rest of the file. If no entry of a run anchors, no record is derived and none is
// refused — the same clean zero call already reports for those entries.
//
// The min is over a total order of values the source supplies, never over arrival
// order: nothing in the transcript format promises the entries are ordered
// (SessionState.Observe folds a max and a min for the same reason), and after
// ADR-0036 the buffer spans a walk, so this is also what makes the result
// independent of which order the walk visited the sources in (ADR-0004).
//
// BoundedVersion is best-effort, as call's is.
func (r *subagentRun) anchorEntry(entry transcriptEntry, timestamp time.Time,
	sessionID record.Identifier, repo record.Hash, source int) {
	entrypoint, known := entrypointFor(entry.Entrypoint)
	if !known {
		return
	}
	if r.anchored && cmp.Or(timestamp.Compare(r.anchor.timestamp), cmp.Compare(entry.UUID, r.anchor.uuid)) >= 0 {
		return
	}
	anchor := subagentAnchor{
		uuid:       entry.UUID,
		timestamp:  timestamp,
		sessionID:  sessionID,
		repo:       repo,
		entrypoint: entrypoint,
		source:     source,
	}
	if version, err := record.BoundedVersion(entry.Version); err == nil {
		anchor.version = version
	}
	r.anchor = anchor
	r.anchored = true
}

// declare folds one consented entry into this run's name, by the same min over
// (timestamp, uuid) the anchor uses. When one file declares two distinct names — 1
// of 400 measured — that total order is what makes the choice arrival-independent
// and re-scan-identical (ADR-0004).
//
// Fail closed on the name: a value the grammar refuses never becomes one, and a
// directory-scoped reference with no scope key is refused rather than digested
// unkeyed (ADR-0007, ADR-0020). DerivedName already refuses the empty value.
//
// The model is best-effort from this entry and not from the anchor, deliberately:
// the anchor entry carries message.model 1 time in 300, while the first named entry
// carries it 292 times in 292. Same best-effort shape call already uses.
func (r *subagentRun) declare(entry transcriptEntry, timestamp time.Time, names record.Namer) {
	if entry.AttributionAgent == "" {
		return
	}
	name, err := names.DerivedName(entry.AttributionAgent)
	if err != nil {
		return
	}
	if r.declared && cmp.Or(timestamp.Compare(r.declaration.timestamp), cmp.Compare(entry.UUID, r.declaration.uuid)) >= 0 {
		return
	}
	declaration := subagentDeclaration{uuid: entry.UUID, timestamp: timestamp, name: name}
	if model, err := record.BoundedIdentifier(entry.Message.Model); err == nil {
		declaration.model = model
	}
	r.declaration = declaration
	r.declared = true
}

// resolveSubagentRuns derives the one record for every anchored run whose session has
// closed, and reports the sources whose runs were refused for want of a name.
//
// Session close is the terminal boundary for this record exactly as a tool_result is
// for a tool call, and it is decided by SessionState.Closed rather than by a
// comparison of its own: ADR-0023 §3's "no second threshold is introduced" holds only
// while there is one implementation of it. Resolving on first sight of the agent id
// instead would refuse every real subagent — the name is on none of the first entries
// — and an emitted record is permanent (ADR-0015 rejects upsert, ADR-0004
// deduplicates the correction away).
//
// A run whose session is still open stays buffered and is visible through
// OpenSessions and the source floors, not through Pending, which counts tool calls.
//
// The order is sorted rather than the map's, for the reason resolveStaleCalls gives:
// iteration order is randomised and two scans of one walk have to produce
// byte-identical store contents. The key is the anchor's timestamp with the agent id
// breaking a tie, which is total because the agent id is the map key.
//
// The second return is a slice of source ordinals rather than a bare count so a
// refusal can be credited to the source that carried it — what keeps doctor's Skipped
// counter honest for a subagent file whose only contribution was a refusal. It holds
// ordinals, never paths (ADR-0007, plan §4.2).
//
// The third return maps each resolved-and-declared agent id to the EventID of the
// record this pass emitted for it. It is returned rather than re-derived by the
// caller for two reasons: a child's case-1 link is then provably the same value
// subagent() put on the parent record, and a run this pass *refused* becomes
// distinguishable from one it has not reached — the distinction ADR-0035 §6 turns
// on, since a refused run's record will never exist while an unreached one's will.
// Nothing this function decides changes: same closed gate, same order, same refusal
// rule, same records.
func resolveSubagentRuns(runs map[record.Identifier]*subagentRun, sessions *SessionState,
	stale Staleness) ([]derivation, []int, map[record.Identifier]record.Hash) {
	resolved := make([]record.Identifier, 0, len(runs))
	for agentID, run := range runs {
		if run.anchored && sessions.Closed(run.anchor.sessionID, stale) {
			resolved = append(resolved, agentID)
		}
	}
	slices.SortFunc(resolved, func(a, b record.Identifier) int {
		return cmp.Or(
			runs[a].anchor.timestamp.Compare(runs[b].anchor.timestamp),
			cmp.Compare(a, b),
		)
	})
	records := make([]derivation, 0, len(resolved))
	refused := []int{}
	emitted := map[record.Identifier]record.Hash{}
	for _, agentID := range resolved {
		run := runs[agentID]
		delete(runs, agentID)
		if !run.declared {
			// Both "declares nothing" (2% of real transcripts) and "declares a value the
			// name grammar refuses" land here. The run happened and Wake will carry no
			// number for it, which is lost collection rather than a clean zero — and
			// naming it from the harness's documented default is inference, not evidence
			// (ADR-0036 §2 and its Alternatives).
			refused = append(refused, run.anchor.source)
			continue
		}
		event := run.subagent(agentID)
		emitted[agentID] = event.EventID
		records = append(records, derivation{event: event, source: run.anchor.source})
	}
	return records, refused, emitted
}

// subagent builds the record itself.
//
// Timestamp is the anchor entry's instant — the subagent's first entry, so when the
// invocation happened, which is the rule call.interrupted() already states for the
// invocation grain. Name and Model come from the declaration fold; every other
// dimension from the anchor.
//
// Outcome is nil. The completion boundary lives on the invoking side and ADR-0036 §5
// declines that correlation outright, so there is nothing to read a verdict from —
// and a nullable outcome is honest where a synthesized one would not be (ADR-0005:
// adapters must never guess). DurationMS is nil for the same reason and one more: no
// record this adapter emits populates it, nil means "the harness reported nothing",
// and ADR-0004's no-upsert store makes it permanent, so the conservative direction is
// the one a later schema bump can still add.
//
// Invoker is model: a subagent run is entered by the model, never typed by the user.
// ViaAgent is deliberately empty — it is the *child*'s attribution field (see call),
// and a subagent is not attributed to itself.
//
// ParentEventID is therefore ADR-0035 §2's case 3, the session span: with both
// ViaSkill and ViaAgent empty this record is attributed to nothing, and Close routes
// it with an empty agent id deliberately. Nesting a subagent under its invoking skill
// would mean resolving a parent from an attributionSkill this record does not carry,
// which is a new decision and not an implementation detail — out of scope here.
//
// The agent id, the cwd, the attributionAgent, the slug and every other value read
// from the file reach no record field, no error and no log line: the id is consumed
// into a hash and the directory into the resolver's answer (ADR-0007, plan §4.2).
func (r *subagentRun) subagent(agentID record.Identifier) record.Record {
	return record.Record{
		SchemaVersion:  record.SchemaVersion,
		EventID:        record.DeriveEventID(harness, subagentSourceEvent(agentID)),
		Timestamp:      r.anchor.timestamp,
		Harness:        harness,
		HarnessVersion: r.anchor.version,
		SessionID:      r.anchor.sessionID,
		Repo:           r.anchor.repo,
		Kind:           record.KindSubagent,
		Name:           r.declaration.name,
		Model:          r.declaration.model,
		Invoker:        record.InvokerModel,
		Entrypoint:     r.anchor.entrypoint,
	}
}
