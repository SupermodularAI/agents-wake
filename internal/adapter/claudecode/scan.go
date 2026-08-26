package claudecode

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/SupermodularAI/agents-wake/internal/jsonl"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// Scan is one walk's worth of session-scoped reader state: every buffer the reader
// used to build inside a single Read, hoisted so that a session split across
// several sources is resolved once, over the union of them (ADR-0036
// §Consequences).
//
// Claude Code writes one session id into several transcripts — the parent's file
// and one per subagent — so a buffer whose lifetime is one file judges a session
// from a partial view: it closes a session another file shows running, interrupts a
// call another file terminated, emits a Shape-A fallback for a run another file's
// Skill tool_use already described, and reports one session_end per file instead of
// one per session. All four are permanent, because ADR-0015 rejects upsert and
// ADR-0004 deduplicates the correction away.
//
// It holds a session id, a timestamp, a byte offset, a source ordinal and derived
// counts. No path, no repository label and no transcript content — the invariant
// SessionState's own comment asserts, carried up to the walk (ADR-0007, ADR-0019,
// plan §4.2). Nothing here touches the filesystem: the walk is the caller's
// (ADR-0019 §1, restated ADR-0036 §3).
//
// It is Claude-Code-concrete and is not a harness-agnostic contract. ADR-0013 keeps
// the adapter interface out until a second reader exists.
//
// It is single-goroutine state and carries no lock: one walk is one caller.
type Scan struct {
	resolve Resolver
	names   record.Namer
	stale   Staleness
	idle    Idleness

	sessions *SessionState
	// pending, earlyResults, skillsInvoked, skillCandidates and grains are the
	// buffers Read used to hold per file, with the same meanings and the same
	// comments (see Read). What changed is their lifetime: it is the walk, so a
	// session's unresolved state is complete before anything is concluded from it.
	pending         map[callKey]call
	earlyResults    map[callKey]callResult
	skillsInvoked   map[skillRun]struct{}
	skillCandidates map[skillRun]skillCandidate
	grains          map[record.Identifier]*sessionGrain
	// subagents holds one unresolved run per agent id a subagent transcript declared,
	// keyed by that id alone (ADR-0036 §2). It is walk-scoped for the same reason as
	// the buffers above and one more: a subagent's transcript is a source of its own,
	// so a per-file buffer would have to resolve it without knowing whether the
	// session its parent transcript carries is still running.
	subagents map[record.Identifier]*subagentRun
	tally     invocationTally
	sources   []sourceTally
}

// sourceTally is what one scan knows about one source it read: what that source
// produced. It is what keeps doctor's Skipped counter — "read, and held no terminal
// event" — meaning what it meant while resolution happened inside one file's own
// read.
type sourceTally struct {
	// derived counts records this source's lines yielded while it was being read.
	derived int
	// refused counts invocations a validated field refused while reading it.
	refused int
	// resolved counts records the post-walk resolution attributed back to it.
	resolved int
}

// productive reports whether this source contributed anything at all. A source that
// derived nothing, refused nothing and had nothing resolved back to it is the clean
// zero doctor calls skipped.
func (t sourceTally) productive() bool {
	return t.derived > 0 || t.refused > 0 || t.resolved > 0
}

// NewScan opens a scan over one walk.
//
// resolve, names, stale and idle carry exactly what the single-source Read takes
// them to carry, and for the same reasons: consent stays the caller's to answer, a
// scope may only be digested by a Namer, and both thresholds arrive as values
// because this package does not read config. Each zero threshold disables only its
// own rule.
//
// The caller drives the walk. Scan never opens, stats or names a file — it is
// handed one reader at a time and assigns each an ordinal in the order it arrives.
func NewScan(resolve Resolver, names record.Namer, stale Staleness, idle Idleness) *Scan {
	return &Scan{
		resolve:         resolve,
		names:           names,
		stale:           stale,
		idle:            idle,
		sessions:        &SessionState{},
		pending:         map[callKey]call{},
		earlyResults:    map[callKey]callResult{},
		skillsInvoked:   map[skillRun]struct{}{},
		skillCandidates: map[skillRun]skillCandidate{},
		grains:          map[record.Identifier]*sessionGrain{},
		subagents:       map[record.Identifier]*subagentRun{},
	}
}

// Read streams one source of the walk and returns what that source alone made
// terminal: its completed tool calls, and the two counters that are per line.
//
// Everything that depends on the union of the walk's sources is deliberately left
// at zero here — Pending, Interrupted, OpenSessions, CursorFloor,
// AmbiguousSkillRuns and SkippedSources. None of them is knowable before the walk
// ends, which is the whole point: a call unterminated in this source may be
// terminated in the next, and a session quiet in this source may be running in the
// next. Close answers all six. A caller must not read a zero here as an answer.
//
// Refused is this source's own lines' refusals, and it is complete here: a subagent
// run is judged only after the walk, and Close reports those on RefusedSubagentRuns —
// a counter of its own rather than a second half of this one, because doctor's state
// word may follow one and not the other (see Result.RefusedSubagentRuns).
//
// A read that fails part-way costs its own records and nothing else: they are
// dropped with the error, and every session the source carried is marked blind so
// the walk resolves none of them from the partial view it now holds. The caller may
// therefore keep walking after an error — which is what a per-harness soft failure
// means (plan §4.3) — without a truncated source contaminating a sibling.
//
// Only events accepted by the resolver can become records, so an adapter scan
// cannot widen project consent.
func (s *Scan) Read(reader io.Reader) (Result, error) {
	if s.resolve == nil {
		return Result{}, errors.New("missing repository resolver")
	}
	// The ordinal is claimed before the source is read, not after. A read that fails
	// part-way has already folded lines under it, and reusing it for the next source
	// would merge two files' byte offsets into one dimension.
	s.sources = append(s.sources, sourceTally{})
	source := len(s.sources) - 1
	result := Result{}
	// seen collects every session id this source observed activity for, so that a
	// line this source could not rule out taints exactly those sessions (see the
	// blindness gate below).
	seen := map[record.Identifier]struct{}{}
	// unreadable counts the lines this source could not use *and could not rule out*
	// as the terminator of a buffered call, which is the only blindness that bears on
	// the staleness rule: a line too long to be delivered, a line whose JSON leaves
	// nothing to inspect, and a line the reader has no entry for that carries a
	// tool_result block anyway.
	//
	// It is narrower than Malformed on purpose. A line the reader could inspect and
	// found no tool_result in cannot have terminated anything, so nothing about any
	// call's fate is hidden by it. That distinction is what keeps a real transcript's
	// routine lines — bookkeeping shapes with no uuid, and entries whose message
	// content is not the type this struct declares — out of the gate. A result payload
	// cannot put a line here at all: ToolUseResult is captured raw, so its shape never
	// costs the reader the entry that terminates a call.
	unreadable := 0
	skipped, err := jsonl.Lines(reader, maxLineBytes, func(offset int64, line []byte) {
		var entry transcriptEntry
		unmarshalErr := json.Unmarshal(line, &entry)
		// Activity comes from every line that named a session and a time, not only from
		// the ones this read has an entry for. The last thing a session wrote is what
		// says it is still alive, and a bookkeeping line or a line whose JSON did not fit
		// this struct was written by a live session just as much as an entry was. Taking
		// liveness from entries alone understates it, and the staleness rule would then
		// call a session that wrote something minutes ago dead — permanently, since
		// ADR-0004 deduplicates the correction away.
		//
		// An id outside the token domain is not observed, matching call, which skips such
		// an entry — so no call is buffered for a session this cannot judge. Observe
		// ignores a zero timestamp, which is most of what these lines carry.
		//
		// The bounded id is computed once here and reused for every buffer key below.
		sessionID, tokenErr := record.BoundedToken(entry.SessionID)
		if tokenErr == nil {
			s.sessions.Observe(source, sessionID, record.NormalizedTimestamp(entry.Timestamp), offset)
			seen[sessionID] = struct{}{}
		}
		if unmarshalErr != nil || !entry.valid() {
			// This read has no entry for the line, whether its JSON did not fit the struct
			// or its identity is outside the domain. Either way nothing is derived from it
			// — and it stops the staleness rule only if it could have been a terminator.
			result.Malformed++
			if !inspectable(unmarshalErr) || entry.carriesToolResult() {
				unreadable++
			}
			return
		}
		observeSessionGrain(s.grains, source, entry, s.resolve)
		// Inside the same post-valid() block, so the uuid both of the run's min-folds
		// order by is already bounded to the token domain.
		observeSubagentRun(s.subagents, source, entry, s.resolve, s.names)
		switch event, status := entry.attributedSkillCandidate(s.resolve, s.names); status {
		case callAccepted:
			deferSkillCandidate(s.skillCandidates, skillRun{session: event.SessionID, name: event.Name}, source, event)
		case callRefused:
			// Counted per refused run rather than per entry that would have collapsed
			// into one: the collapse is a deduplication of records that do exist, and a
			// candidate refused here never enters it, so there is nothing to collapse
			// against. What the number says is how many attributed runs this read could
			// have collected and did not.
			result.Refused++
		case callSkipped:
		}
		if tokenErr != nil {
			// No session to key a buffer under, so nothing from this entry can be
			// buffered or matched. Its blocks are the same clean zero call already
			// reports for them (callSkipped), not a refusal.
			return
		}
		for _, block := range entry.Message.Content {
			switch block.Type {
			case "tool_use":
				// Every branch below runs after the full entry.call gate, so an early
				// result can never bypass a consent, id-domain or naming check: the
				// refused and skipped branches discard the buffered result instead of
				// leaving it to be matched by anything. They key the discard by the raw
				// block id because pendingCall.id is empty on those paths.
				pendingCall, status := entry.call(source, block, s.resolve, s.names)
				switch status {
				case callAccepted:
					if pendingCall.kind == record.KindSkill {
						// Recorded on sight rather than on termination: ADR-0023 §2 asks which
						// skills the session invoked, and a Skill call that never terminates
						// still becomes its own record through the staleness rule. The name is
						// the one primitiveName derived, which is the same names.DerivedName
						// output a candidate carries.
						//
						// On callAccepted only: a refused or skipped call means the identical
						// name and consent gates also refuse the candidate, so no candidate
						// exists for that name either.
						//
						// Walk-scoped, so a Skill tool_use in the parent transcript covers the
						// attributed run its subagent transcript carries — the double count
						// ADR-0036 §4 is about.
						s.skillsInvoked[skillRun{session: pendingCall.sessionID, name: pendingCall.name}] = struct{}{}
					}
					key := callKey{session: pendingCall.sessionID, id: pendingCall.id}
					if early, terminated := s.earlyResults[key]; terminated {
						delete(s.earlyResults, key)
						result.Records = append(result.Records, pendingCall.complete(early))
					} else {
						s.pending[key] = pendingCall
					}
				case callRefused:
					result.Refused++
					delete(s.earlyResults, callKey{session: sessionID, id: block.ID})
				case callSkipped:
					delete(s.earlyResults, callKey{session: sessionID, id: block.ID})
				}
			case "tool_result":
				key := callKey{session: sessionID, id: block.ToolUseID}
				pendingCall, open := s.pending[key]
				if !open {
					// The tool_use line has not been read yet, or this id was already
					// completed, skipped or refused. Retain the first result per id and
					// nothing else: a second result for one id is a no-op here exactly as
					// it was before, and a result whose tool_use never arrives yields no
					// record at all — there is no name, kind, invoker or consented repo to
					// build one from, and only a terminal event may be emitted (ADR-0015,
					// ADR-0007).
					if _, held := s.earlyResults[key]; !held {
						s.earlyResults[key] = resultOf(entry, block)
					}
					continue
				}
				delete(s.pending, key)
				result.Records = append(result.Records, pendingCall.complete(resultOf(entry, block)))
			}
		}
	})
	if err != nil {
		// The read stopped part-way, so the sessions this source carried are known to
		// have been judged from an incomplete view. internal/jsonl delivers every line
		// before the failing one, so those lines are already folded into the walk-scoped
		// buffers above — while everything the harness wrote after the failure was never
		// seen. Returning the error without tainting them would leave the walk resolving
		// them as if the source had been read whole, which is the same inference from
		// blindness the unreadable gate below refuses, in its most complete form: not one
		// line unread but the rest of the file.
		//
		// The records this source did derive are dropped with the error, as they were
		// before this state was hoisted to the walk. Dropping them is safe — a record not
		// written is derived again by the next scan — and it is exactly why the taint is
		// needed: the totals folded into the session grain are now missing them, so an
		// unmarked session would report a session_end understating its own tool calls,
		// written once and never corrected (ADR-0034 §3 first-write-wins).
		//
		// A failing read is not rare enough to reason about hypothetically: these
		// transcripts are being appended to by a running harness while the walk reads
		// them, and os.Open succeeding says nothing about reading to the end. The cost of
		// the taint is a call that stays Pending and a session_end deferred to the next
		// scan — a slower scan, never a permanently wrong record.
		s.markBlind(seen)
		return Result{}, errors.New("reading Claude Code history")
	}
	// A line too long to deliver is unusable in the same way a line that does not
	// parse is: counted as malformed so doctor can report blindness, and nothing is
	// synthesised from it — no call is opened, so no result can terminate one.
	//
	// It does not join unreadable below, unlike a line whose bytes arrived but made
	// no sense. maxLineBytes is a fixed internal constant, not a limit a user can
	// raise, so an oversized line is oversized on every future scan — there is no
	// later scan for the staleness rule to defer to. Gating on it would not buy a
	// slower-but-correct read; it would pin this source's pending calls and cursor
	// floor forever.
	result.Malformed += skipped
	// A source that could not rule out one of its lines may not let any session it
	// carried be judged silent. That line may be the tool_result that terminated a
	// buffered call, and it was not observed as activity either — so last activity is
	// known-understated, not merely old. Concluding "interrupted" from that would infer
	// a terminal outcome from blindness, which plan §3.3 forbids, and would write a
	// permanent failure for a call that succeeded: ADR-0015 rejects upsert and ADR-0004
	// deduplicates, so no later scan can take it back. ADR-0015 scopes interrupted to a
	// session that actually died; this reader cannot tell that apart from one line it
	// cannot read.
	//
	// The grain is the sessions this source carried, and the taint propagates to every
	// source of those sessions — a subagent transcript's unreadable line hides its
	// parent's evidence just as much, because after ADR-0036 the session is resolved
	// over the union of them (the ticket's requirement 4). internal/jsonl cannot
	// attribute a line it never delivered to a session, so the whole of what this
	// source observed is the smallest honest grain.
	//
	// Deliberately not a walk-wide switch. Before ADR-0036 the buffers died with the
	// file, so disabling the rule for "the read" cost one transcript; now one
	// unreadable line anywhere would disable the staleness rule and session_end
	// derivation for every session on the machine. The cost of the per-session taint is
	// a call that stays Pending and a cursor floor that does not move — a slower scan,
	// never wrong data, the direction Staleness's zero value already takes.
	//
	// The gate is unreadable and not Malformed. Every real transcript carries lines
	// Malformed counts that hide nothing — per-session bookkeeping (ai-title,
	// last-prompt, queue-operation) has no uuid, so valid() rejects it, and a message's
	// content arriving as a type this struct does not declare fails json.Unmarshal — and
	// gating on that count switched ADR-0015's rule off on every machine while every
	// hand-written fixture stayed green. Keeping this source's own two questions apart
	// ("did I have an entry for that line" and "could that line have terminated a
	// call") is what makes the rule run in production and stop only for blindness that
	// could actually mislead it.
	if unreadable > 0 {
		s.markBlind(seen)
	}
	// Folded here rather than at Close so the aggregate never has to retain the
	// records: a session_end's totals are a running count over the union of its
	// sources (ADR-0034 §3 as widened by ADR-0036).
	s.tally.observe(result.Records)
	s.sources[source].derived = len(result.Records)
	s.sources[source].refused = result.Refused
	return result, nil
}

// Close resolves the walk's session-scoped state once, over the union of every
// source it read, and returns the records that resolution derived plus the counters
// that are only knowable now.
//
// RefusedSubagentRuns is among them, and only here: a subagent transcript that
// declares no usable name can be judged only once its session has closed (ADR-0036 §2,
// ADR-0015). It is separate from Refused — which stays what a source's own lines
// refused — because doctor reads the two differently, and folding them would put every
// machine that runs subagents permanently into "collects nothing"
// (see Result.RefusedSubagentRuns, health.Diagnose).
//
// This is the one place the session-close determination is resolved for a walk, and
// resolveSessionSkills is called exactly once from it: ADR-0035 §3's parent-id
// resolution consumes that single determination rather than forking a second one.
//
// The two thresholds stay separate and so do the two predicates (ADR-0023 §3,
// ADR-0034): scan.stale_call_timeout through SessionState.Closed for the buffered
// calls and the Shape-A fallbacks, session.idle_timeout through finishedSessions
// for the session grain. Neither is zeroed here for blindness — the taint is on the
// session inside SessionState, so one unreadable line stops the rules for the
// sessions its source carried and for no others.
//
// CursorFloor is left zero: a floor is per source, so a walk has no single one.
// SessionState.SourceFloor is the per-source answer.
//
// Close is called once, at the end of the walk. Calling it again resolves nothing
// further — the buffers it drained stay drained — and reports the same source
// tallies.
func (s *Scan) Close() Result {
	result := Result{}
	// Today's order exactly, so a one-source walk serialises character-for-character
	// as it did before: interrupted calls, then Shape-A fallbacks, then session_end.
	for _, derived := range resolveStaleCalls(s.pending, s.sessions, s.stale) {
		result.Records = append(result.Records, derived.event)
		s.credit(derived.source)
		result.Interrupted++
	}
	fallbacks, ambiguous := resolveSessionSkills(s.skillCandidates, s.skillsInvoked, s.sessions, s.stale)
	for _, derived := range fallbacks {
		result.Records = append(result.Records, derived.event)
		s.credit(derived.source)
	}
	// After the two existing groups, so those two serialise in exactly today's order
	// and a walk carrying no subagent run is byte-for-byte unchanged — and before the
	// tally below, so a subagent record counts toward its session's ToolCalls exactly
	// as a call terminated inside a source does.
	subagents, refusedSources := resolveSubagentRuns(s.subagents, s.sessions, s.stale)
	for _, derived := range subagents {
		result.Records = append(result.Records, derived.event)
		s.credit(derived.source)
	}
	for _, source := range refusedSources {
		s.refuse(source)
	}
	result.RefusedSubagentRuns = len(refusedSources)
	// Folded before the session grain reads the tally, so a call this resolution made
	// terminal counts toward its session's totals exactly as a call terminated inside
	// a source does.
	s.tally.observe(result.Records)
	for _, end := range resolveSessionEnds(s.grains, s.sessions, s.idle, &s.tally) {
		result.Records = append(result.Records, end)
		// Credited to every source that fed the session, not to one: the record is the
		// union's, and a subagent transcript whose only contribution is a share of these
		// totals did produce something.
		if grain, observed := s.grains[end.SessionID]; observed {
			for source := range grain.sources {
				s.credit(source)
			}
		}
	}
	result.AmbiguousSkillRuns = ambiguous
	result.Pending = len(s.pending)
	result.OpenSessions = s.sessions.OpenSessions(s.stale)
	for _, tally := range s.sources {
		if !tally.productive() {
			result.SkippedSources++
		}
	}
	return result
}

// markBlind taints every session one source observed activity for, so that nothing
// the walk resolves may conclude silence for any of them. Both callers are the two
// ways a source can be read blind — one line it could not rule out as a
// terminator, or a read that stopped part-way — and the grain is the same for
// both: the sessions that source carried, and no others.
func (s *Scan) markBlind(seen map[record.Identifier]struct{}) {
	for sessionID := range seen {
		s.sessions.MarkBlind(sessionID)
	}
}

// credit records that the post-walk resolution attributed a record back to one
// source, so that source is not reported as having produced nothing.
func (s *Scan) credit(source int) {
	if source < 0 || source >= len(s.sources) {
		return
	}
	s.sources[source].resolved++
}

// refuse records that the post-walk resolution refused an invocation this source
// carried, so a source whose only contribution was a counted refusal is not also
// reported as having produced nothing. Read assigns this field for its own lines
// before Close increments it, so the two halves add rather than overwrite.
func (s *Scan) refuse(source int) {
	if source < 0 || source >= len(s.sources) {
		return
	}
	s.sources[source].refused++
}
