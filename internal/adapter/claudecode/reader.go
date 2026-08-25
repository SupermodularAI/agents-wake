// Package claudecode derives safe terminal records from Claude Code JSONL
// transcripts. It retains pending call metadata only until the call terminates —
// by a matching result, or by ADR-0015's staleness rule once its session has gone
// quiet — and a result's derived outcome only until its matching call arrives, and
// never persists transcript content.
package claudecode

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/jsonl"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

const harness = record.Identifier("claude-code")

// maxLineBytes is the largest transcript line this reader accepts. It stays an
// internal constant, not a config key: ADR-0014 keeps the config surface
// deliberately small, and a limit a user can raise is a limit that stops bounding
// anything.
const maxLineBytes = 1024 * 1024

// Result contains safe derived records plus collection health counters.
type Result struct {
	Records []record.Record
	// Malformed counts source lines this read had no transcript entry for: a line
	// that does not parse, one whose entry id or timestamp is outside the domain, and
	// one too long to deliver. It is doctor's drift signal.
	//
	// It is deliberately not the gate on the staleness rule. It is a broad counter —
	// a real transcript's routine per-session bookkeeping lines (ai-title,
	// last-prompt, queue-operation, which carry no uuid) are counted here on every
	// machine — and a rule gated on it would never run outside a hand-written
	// fixture. The gate is the narrower "could not be ruled out as a terminator"
	// signal computed inside Read.
	Malformed int
	// Pending counts calls still unterminated when the scan finished that the
	// staleness rule did not resolve — because their session is still inside the
	// window, or because this read hit a line it could not rule out as their
	// terminator and so could not apply the rule at all. It is a number that is not
	// final yet rather than collection that was lost (ADR-0015), which is why doctor
	// renders it as its own line and not as "collects nothing".
	Pending int
	// Interrupted counts calls this scan resolved by the staleness rule: their
	// session went quiet past the threshold, so they were emitted as terminal records
	// with outcome interrupted rather than buffered again forever. Those records are
	// in Records and are also counted by Parsed upstream.
	Interrupted int
	// OpenSessions counts the sessions this transcript's entries belong to that have
	// not closed under the staleness rule. When this read hit a line it could not rule
	// out no session is reported closed, for the same reason no call is resolved.
	OpenSessions int
	// CursorFloor is the byte offset of the earliest line belonging to any session
	// still open. A future incremental cursor (T020, T102) must not advance past it
	// until that session closes: ADR-0023 §5 generalizes ADR-0015's "does not advance
	// its cursor past [an unterminated call]" to the session grain. It is meaningful
	// only when OpenSessions is positive.
	CursorFloor int64
	// Refused counts tool calls dropped because a validated field refused its
	// source value: the primitive's own name — a Task block naming no subagent, or
	// a name the name/scope grammar refuses — or an entrypoint outside Wake's
	// vocabulary. Fail closed (ADR-0007): nothing is written and no
	// placeholder name is substituted. It is deliberately not Malformed, which
	// means "a line that is unusable" and feeds doctor's drift signal, and
	// deliberately not the store's Dropped, which counts records refused at write
	// time. The value that was refused is never carried — only the count (plan
	// §4.2).
	Refused int
	// AmbiguousSkillRuns counts attributed skill runs this scan collapsed into an
	// already-emitted fallback record for the same (session, skill name) pair.
	// ADR-0023 names that collapse an accepted limitation: no transcript signal
	// separates "one slash-command run" from "two with no tool trace", and inferring
	// one would need either a model call (ADR-0008 forbids) or an entry-ordinal
	// heuristic already shown unreliable.
	//
	// It is uncertainty about a count, never a count of invocations, and doctor renders
	// it as its own line for that reason. It carries no skill name, no session id, no
	// path and no transcript value — only how many times the question came up (plan
	// §4.2, ADR-0007). A run the tool_use/tool_result pair already described
	// contributes nothing here: that run is not uncertain, it is covered.
	AmbiguousSkillRuns int
}

// Resolver maps one observed event — a recorded working directory and the instant it
// happened — to a consented repository hash.
//
// It returns false when the event is outside consent, which has two dimensions and
// one answer: the directory was never consented, or the event predates the instant
// collection began for its repository (ADR-0024, ADR-0025). The reader passes the
// event's own timestamp and never learns the boundary — consent stays the caller's
// to answer, so no adapter scan can widen it in either dimension.
//
// The reader never accesses the filesystem while resolving a transcript entry.
type Resolver func(cwd string, at time.Time) (record.Hash, bool)

// Read streams one Claude Code transcript. Only events accepted by resolve can
// become records, so an adapter scan cannot widen project consent.
//
// names carries the key a directory-scoped reference's scope is digested under. A
// scope is a repository path fragment, so the reader cannot derive a persistable
// name for one on its own (ADR-0020); a Namer with no key drops those references
// rather than persisting an unkeyed digest of a path.
//
// stale carries ADR-0015's staleness rule (scan.stale_call_timeout and the scan's
// clock) from the caller that owns config, so the threshold is never a constant
// here. Its zero value buffers every unterminated call, which is what a caller that
// cannot read the threshold must do.
func Read(reader io.Reader, resolve Resolver, names record.Namer, stale Staleness) (Result, error) {
	if resolve == nil {
		return Result{}, errors.New("missing repository resolver")
	}

	result := Result{}
	pending := map[string]call{}
	sessions := &SessionState{}
	// earlyResults holds a tool_result whose tool_use line has not been read yet.
	// Claude Code writes the pair out of order in a small fraction of transcripts,
	// and a forward-only pairing loop drops the result and then buffers the call
	// forever — which, once the staleness path is live, writes a completed call as
	// outcome: interrupted permanently, because the store deduplicates on event_id
	// and never upserts (ADR-0004, ADR-0015).
	//
	// It is per-Read state next to pending, not package state, for the same reason
	// pending is: it is unresolved source state a cursor floor must be able to see
	// (ADR-0015, ADR-0023), so the derived count cannot depend on where a scan
	// started. It carries no consent-bearing and no free-text value (see
	// callResult), so buffering one before the call's repository consent is known
	// retains nothing.
	earlyResults := map[string]callResult{}
	// skillsInvoked and skillCandidates are the session-grain extension of pending that
	// ADR-0023 §2 asks for: which skills a session invoked through a Skill tool_use, and
	// the attributed end_turn runs this read has not yet proved are duplicates of one.
	//
	// Per-Read state next to pending, for the reason pending is: it is unresolved source
	// state a cursor floor must be able to see (ADR-0015, ADR-0023 §5). Deliberately not
	// fields on SessionState, which holds a session id, a timestamp and a byte offset and
	// no value derived from a transcript's content — the session-close determination is
	// consumed from there rather than duplicated, which is what ADR-0023 §3's "no second
	// threshold" requires.
	skillsInvoked := map[skillRun]struct{}{}
	skillCandidates := map[skillRun]skillCandidate{}
	// unreadable counts the lines this read could not use *and could not rule out* as
	// the terminator of a buffered call, which is the only blindness that bears on the
	// staleness rule: a line too long to be delivered, a line whose JSON leaves nothing
	// to inspect, and a line the reader has no entry for that carries a tool_result
	// block anyway.
	//
	// It is narrower than Malformed on purpose. A line the reader could inspect and
	// found no tool_result in cannot have terminated anything, so nothing about any
	// call's fate is hidden by it. That distinction is what keeps a real transcript's
	// routine lines — bookkeeping shapes with no uuid, and entries whose message content
	// is not the type this struct declares — out of the gate. A result payload cannot
	// put a line here at all: ToolUseResult is captured raw, so its shape never costs
	// the reader the entry that terminates a call.
	unreadable := 0
	skipped, err := jsonl.Lines(reader, maxLineBytes, func(offset int64, line []byte) {
		var entry transcriptEntry
		unmarshalErr := json.Unmarshal(line, &entry)
		// Activity comes from every line that named a session and a time, not only from
		// the ones this read has an entry for. The last thing a session wrote is what says
		// it is still alive, and a bookkeeping line or a line whose JSON did not fit this
		// struct was written by a live session just as much as an entry was. Taking
		// liveness from entries alone understates it, and the staleness rule would then
		// call a session that wrote something minutes ago dead — permanently, since
		// ADR-0004 deduplicates the correction away.
		//
		// An id outside the token domain is not observed, matching call, which skips such
		// an entry — so no call is buffered for a session this cannot judge. Observe
		// ignores a zero timestamp, which is most of what these lines carry.
		if sessionID, tokenErr := record.BoundedToken(entry.SessionID); tokenErr == nil {
			sessions.Observe(sessionID, record.NormalizedTimestamp(entry.Timestamp), offset)
		}
		if unmarshalErr != nil || !entry.valid() {
			// This read has no entry for the line, whether its JSON did not fit the struct or
			// its identity is outside the domain. Either way nothing is derived from it — and
			// it stops the staleness rule only if it could have been a terminator.
			result.Malformed++
			if !inspectable(unmarshalErr) || entry.carriesToolResult() {
				unreadable++
			}
			return
		}
		if event, ok := entry.attributedSkillCandidate(resolve, names); ok {
			deferSkillCandidate(skillCandidates, skillRun{session: event.SessionID, name: event.Name}, event)
		}
		for _, block := range entry.Message.Content {
			switch block.Type {
			case "tool_use":
				// Every branch below runs after the full entry.call gate, so an early
				// result can never bypass a consent, id-domain or naming check: the
				// refused and skipped branches discard the buffered result instead of
				// leaving it to be matched by anything. They key the discard by the raw
				// block id because pendingCall.id is empty on those paths.
				pendingCall, status := entry.call(block, resolve, names)
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
						skillsInvoked[skillRun{session: pendingCall.sessionID, name: pendingCall.name}] = struct{}{}
					}
					if early, terminated := earlyResults[pendingCall.id]; terminated {
						delete(earlyResults, pendingCall.id)
						result.Records = append(result.Records, pendingCall.complete(early))
					} else {
						pending[pendingCall.id] = pendingCall
					}
				case callRefused:
					result.Refused++
					delete(earlyResults, block.ID)
				case callSkipped:
					delete(earlyResults, block.ID)
				}
			case "tool_result":
				pendingCall, open := pending[block.ToolUseID]
				if !open {
					// The tool_use line has not been read yet, or this id was already
					// completed, skipped or refused. Retain the first result per id and
					// nothing else: a second result for one id is a no-op here exactly as
					// it was before, and a result whose tool_use never arrives yields no
					// record at all — there is no name, kind, invoker or consented repo to
					// build one from, and only a terminal event may be emitted (ADR-0015,
					// ADR-0007).
					if _, seen := earlyResults[block.ToolUseID]; !seen {
						earlyResults[block.ToolUseID] = resultOf(entry, block)
					}
					continue
				}
				delete(pending, block.ToolUseID)
				result.Records = append(result.Records, pendingCall.complete(resultOf(entry, block)))
			}
		}
	})
	if err != nil {
		return Result{}, errors.New("reading Claude Code history")
	}
	// A line too long to deliver is unusable in the same way a line that does not
	// parse is: counted as malformed so doctor can report blindness, and nothing is
	// synthesised from it — no call is opened, so no result can terminate one.
	//
	// It does not join unreadable below, unlike a line whose bytes arrived but made
	// no sense. maxLineBytes is a fixed internal constant, not a limit a user can
	// raise (see above), so an oversized line is oversized on every future scan —
	// there is no later scan for the staleness rule to defer to. Gating on it would
	// not buy a slower-but-correct read; it would pin this file's pending calls and
	// cursor floor forever.
	result.Malformed += skipped
	// A read that could not rule out one of its lines may not judge a session silent.
	// That line may be the tool_result that terminated a buffered call, and it was not
	// observed as activity either — so last activity is known-understated, not merely
	// old. Concluding "interrupted" from that would infer a terminal outcome from
	// blindness, which plan §3.3 forbids, and would write a permanent failure for a
	// call that succeeded: ADR-0015 rejects upsert and ADR-0004 deduplicates, so no
	// later scan can take it back. ADR-0015 scopes interrupted to a session that
	// actually died; this reader cannot tell that apart from one line it cannot read.
	//
	// Disabling the rule for the whole read rather than for one session is the
	// practical grain: internal/jsonl cannot attribute a line it never delivered to a
	// session, so there is no session to exempt. The cost is a call that stays Pending
	// and a cursor floor that does not move — a slower scan, never wrong data, which is
	// the direction Staleness's zero value already takes.
	//
	// The gate is unreadable and not Malformed. Every real transcript carries lines
	// Malformed counts that hide nothing — per-session bookkeeping (ai-title,
	// last-prompt, queue-operation) has no uuid, so valid() rejects it, and a message's
	// content arriving as a type this struct does not declare fails json.Unmarshal — and
	// gating on that count switched ADR-0015's rule off on every machine while every
	// hand-written fixture stayed green. Keeping this read's own two questions apart
	// ("did I have an entry for that line" and "could that line have terminated a
	// call") is what makes the rule run in production and stop only for blindness that
	// could actually mislead it.
	judged := stale
	if unreadable > 0 {
		judged = Staleness{}
	}
	interrupted := resolveStaleCalls(pending, sessions, judged)
	result.Records = append(result.Records, interrupted...)
	result.Interrupted = len(interrupted)
	result.Pending = len(pending)
	// judged, not stale: a read that could not rule out one of its lines may not
	// conclude any session went silent, and a Shape-A fallback emitted from a blind
	// read is permanent (ADR-0015 rejects upsert, ADR-0004 deduplicates the correction
	// away). The cost is deferral to a later scan, which ADR-0023's Consequences
	// already name as this record's latency profile.
	fallbacks, ambiguous := resolveSessionSkills(skillCandidates, skillsInvoked, sessions, judged)
	result.Records = append(result.Records, fallbacks...)
	result.AmbiguousSkillRuns = ambiguous
	result.OpenSessions, result.CursorFloor = sessions.CursorFloor(judged)
	return result, nil
}

// resolveSessionSkills emits the one Shape-A fallback record for every deferred
// candidate whose session has closed and whose skill name that session never invoked
// through a Skill tool_use, and reports how many extra candidates it collapsed to do
// so.
//
// A closed session is the terminal boundary for this record exactly as a tool_result
// is for a tool call (ADR-0023 §3), and it is decided by SessionState.Closed rather
// than by a comparison of its own: "no second threshold is introduced" holds only
// while there is one implementation of it. A candidate whose session is still open
// stays buffered and is visible through OpenSessions and CursorFloor, not through
// Pending, which counts tool calls.
//
// A candidate whose name is in invoked is dropped rather than emitted: the
// tool_use/tool_result pair already produced the one true record and describes the run
// better (T111's precedent, attributedSkillCandidate's comment). Its collapsed extras
// are not counted as ambiguity either — nothing about that run is uncertain.
//
// The order is sorted rather than the map's, for the reason resolveStaleCalls gives:
// iteration order is randomised and two scans of one transcript have to produce
// byte-identical store contents. The key is the chosen candidate's own (timestamp,
// event id), a total order because one transcript entry yields at most one candidate.
func resolveSessionSkills(buffer map[skillRun]skillCandidate, invoked map[skillRun]struct{}, sessions *SessionState, stale Staleness) ([]record.Record, int) {
	resolved := make([]skillRun, 0, len(buffer))
	for key := range buffer {
		if sessions.Closed(key.session, stale) {
			resolved = append(resolved, key)
		}
	}
	slices.SortFunc(resolved, func(a, b skillRun) int {
		left, right := buffer[a].chosen, buffer[b].chosen
		return cmp.Or(left.Timestamp.Compare(right.Timestamp), cmp.Compare(string(left.EventID), string(right.EventID)))
	})
	records := make([]record.Record, 0, len(resolved))
	ambiguous := 0
	for _, key := range resolved {
		candidate := buffer[key]
		delete(buffer, key)
		if _, matched := invoked[key]; matched {
			continue
		}
		records = append(records, candidate.chosen)
		ambiguous += candidate.extra
	}
	return records, ambiguous
}

// resolveStaleCalls emits the terminal record for every buffered call whose session
// has closed, and removes it from the buffer so it is not counted as pending.
//
// The record reuses the id the call already derived from its own source event: one
// unresolved call becomes at most one record, whether a tool_result terminates it or
// this does, so a rescan re-derives the same id and the store deduplicates it
// (ADR-0004, ADR-0002). ADR-0015 rejects upsert, so this is final — a tool_result
// arriving after emission is deduplicated away, which is why the threshold errs long
// (internal/config/keys.go).
//
// The order is sorted rather than the map's: iteration order is randomised, and two
// scans of one transcript have to produce byte-identical store contents. The key is
// the call's own tool_use timestamp, with the block id breaking a tie — not the
// line's position in the file, which the buffer does not retain. That is a total
// order, because the block id is unique per call: pending is keyed by it.
func resolveStaleCalls(pending map[string]call, sessions *SessionState, stale Staleness) []record.Record {
	resolved := make([]string, 0, len(pending))
	for id, buffered := range pending {
		if sessions.Closed(buffered.sessionID, stale) {
			resolved = append(resolved, id)
		}
	}
	slices.SortFunc(resolved, func(a, b string) int {
		return cmp.Or(pending[a].timestamp.Compare(pending[b].timestamp), cmp.Compare(a, b))
	})
	records := make([]record.Record, 0, len(resolved))
	for _, id := range resolved {
		records = append(records, pending[id].interrupted())
		delete(pending, id)
	}
	return records
}

type transcriptEntry struct {
	UUID                 string    `json:"uuid"`
	SessionID            string    `json:"sessionId"`
	CWD                  string    `json:"cwd"`
	Timestamp            time.Time `json:"timestamp"`
	Version              string    `json:"version"`
	AttributionMCPServer string    `json:"attributionMcpServer"`
	AttributionMCPTool   string    `json:"attributionMcpTool"`
	AttributionAgent     string    `json:"attributionAgent"`
	AttributionSkill     string    `json:"attributionSkill"`
	ToolDenialKind       string    `json:"toolDenialKind"`
	// Entrypoint is how the harness process was started ("cli", "sdk-py",
	// "sdk-cli" on Claude Code today). It is mapped onto record.Entrypoint's own
	// vocabulary and never persisted verbatim: an unmapped value refuses the event
	// rather than being passed through (ADR-0005, ADR-0007).
	Entrypoint string `json:"entrypoint"`
	// IsSidechain marks a subagent's own turn. It is read for exactly one purpose:
	// excluding such a turn from ever being considered a skill invocation (ADR-0023
	// §1). It is never a discriminator between "already covered by a Skill tool_use"
	// and "no tool_use exists" — ADR-0023's Context measured both of those at 100%
	// false, twice, so it cannot do that job. It adds no record.Record field and
	// persists nothing: reading a flag is not retaining it (ADR-0007).
	IsSidechain bool `json:"isSidechain"`
	// ToolUseResult stays raw because real Claude Code writes whatever shape the
	// tool returned: an object for a structured result, a bare string for Bash, an
	// array of content blocks for Task. A typed field type-errors the whole line —
	// and that line is the only thing that terminates its call, so the call it
	// should have completed stays buffered forever and is never emitted (ADR-0015).
	//
	// The bytes are read, never retained: interruptedResult takes one boolean from
	// them and they reach no record field, no error and no log line (plan §4.2).
	// Reading a payload is not persisting one — the record type is still the
	// allowlist (ADR-0007).
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Message       message         `json:"message"`
}

type message struct {
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Content    []contentBlock `json:"content"`
}

type contentBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	IsError   *bool  `json:"is_error"`
	Input     input  `json:"input"`
}

// input names only the allowlisted fields a primitive needs. In particular, it
// does not retain a Skill's free-text args field while decoding the transcript.
type input struct {
	Skill        string `json:"skill"`
	SubagentType string `json:"subagent_type"`
}

type toolResult struct {
	Interrupted bool `json:"interrupted"`
}

// callResult is the whole of what a tool_result line contributes to a record: the
// timestamp complete stamps and the outcome outcomeFor derives. It holds the
// derived enum rather than the source fields it came from, so the reader can
// retain a result whose tool_use has not arrived yet without holding any
// transcript string at all — not a denial kind, not a tool name (ADR-0007,
// plan §4.2).
type callResult struct {
	timestamp time.Time
	outcome   *record.Outcome
}

// resultOf derives the terminal half of a call from the tool_result entry and
// block, at the moment the line is read. Both orders of the pair go through this
// one function, so line order cannot change a derived outcome.
func resultOf(entry transcriptEntry, block contentBlock) callResult {
	return callResult{timestamp: entry.Timestamp, outcome: outcomeFor(entry, block)}
}

// interruptedResult reports the one signal this reader takes from a tool result
// payload. It reads the boolean only when the payload is an object: a string, an
// array, a number or null carries no structured result, and inferring one from it
// would be exactly the "infer structure and keep counting" plan §3.3 forbids. A
// payload that is an object but does not decode — no interrupted field, or one at
// the wrong type — is not interrupted either: a terminal verdict is promoted only
// on a positive boolean, never guessed (ADR-0005).
func (entry transcriptEntry) interruptedResult() bool {
	raw := bytes.TrimSpace(entry.ToolUseResult)
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var result toolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return false
	}
	return result.Interrupted
}

// valid bounds the entry id to the opaque-token domain rather than only requiring
// it to be non-empty, because that id is half of every tool call's derived
// identity: a value from the token domain cannot contain callSeparator, which is
// what makes the composition unambiguous (ADR-0004). A transcript entry whose id
// is outside that domain is counted malformed like any other unusable line. Real
// Claude Code ids are RFC 4122 uuids, which the domain admits.
func (entry transcriptEntry) valid() bool {
	if entry.SessionID == "" || entry.Timestamp.IsZero() {
		return false
	}
	_, err := record.BoundedToken(entry.UUID)
	return err == nil
}

// inspectable reports whether a decode that ended in err still left the entry worth
// looking at. A type mismatch does: encoding/json records it and carries on with the
// remaining keys, so a field this reader does not model arriving as some other type —
// a message's content as a plain string, ordinary in real transcripts — costs the entry
// but not the reader's view of the line. Anything else, a syntax error above all, leaves
// nothing to look at.
//
// Whether such an entry should be salvaged rather than rejected is a separate
// question, and a bigger one than the staleness rule: today the reader derives nothing
// from it. This only decides whether it may still conclude a session went silent.
func inspectable(err error) bool {
	if err == nil {
		return true
	}
	var mismatch *json.UnmarshalTypeError
	return errors.As(err, &mismatch)
}

// carriesToolResult reports whether this entry holds a tool_result block, which is
// the only thing that can terminate a buffered call. Read asks it about an entry
// valid rejected: such an entry cannot be attributed, so a result it carries is a
// call whose fate this read could not see, and the staleness rule must not run.
//
// A line the reader decoded and found no tool_result in is the opposite case — it is
// unusable and harmless, and must not stop the rule. That is most of what valid
// rejects on a real transcript: bookkeeping lines carrying a title, a queued
// operation or the last prompt's leaf, none of them a transcript entry at all.
func (entry transcriptEntry) carriesToolResult() bool {
	return slices.ContainsFunc(entry.Message.Content, func(block contentBlock) bool {
		return block.Type == "tool_result"
	})
}

// callSeparator delimits the two halves of a tool call's source identity. It is a
// unit separator, which neither half can contain — valid bounds the entry id to
// the token domain and call bounds the block id — so the split is unambiguous: no
// two distinct (entry, block) pairs share a composed identity, and no composed
// identity can equal a bare entry id, which is what attributedSkillCandidate
// derives a terminal run from.
const callSeparator = "\x1f"

// callSourceEvent identifies one tool_use block by the source event carrying it
// and the block's own id. Both halves come from the transcript: no ordinal of the
// block within its entry, no write time, no randomness — so the same transcript
// re-derives the same ids forever and re-ingestion stays a no-op (ADR-0004).
func callSourceEvent(entryUUID string, blockID record.Identifier) record.Identifier {
	return record.Identifier(entryUUID + callSeparator + string(blockID))
}

// skillRun keys one session's view of one skill primitive. It is the pair ADR-0023
// §3 resolves and the pair §4 bounds to a single fallback record.
//
// The session half is what keeps the count independent of how many transcripts a scan
// reads. The name half is record.Namer's output on both sides of the comparison — a
// Skill tool_use's name goes through primitiveName and a candidate's through
// attributedSkillCandidate, both landing on names.DerivedName — so a directory-scoped
// reference compares as its keyed digest and never as the path fragment behind it
// (ADR-0020). Two normalisations here would misclassify a matched run as having no
// tool call and reinstate the exact double count this exists to remove.
type skillRun struct {
	session record.Identifier
	name    record.Identifier
}

// skillCandidate is one deferred attributed skill run: the record it would emit, and
// how many further candidates for the same skillRun this read collapsed into it.
//
// It holds the record rather than the entry it came from, so every gate
// attributedSkillCandidate applies has already run and what is buffered carries only
// allowlisted fields — the record type is the allowlist (ADR-0007).
type skillCandidate struct {
	chosen record.Record
	extra  int
}

// deferSkillCandidate folds one candidate into the buffer for its skillRun, keeping
// the earliest by (timestamp, event id) and counting the rest.
//
// Earliest by a total order over values the source event supplies, never by arrival:
// nothing in the transcript format promises the entries are ordered — the reason
// SessionState.Observe folds a max and a min rather than last-wins — and a first-wins
// fold would let line order decide which id and timestamp the one emitted record
// carries, while two scans of one transcript have to produce byte-identical store
// contents (ADR-0004). The order is total because one entry yields at most one
// candidate, so no two candidates share an event id.
func deferSkillCandidate(buffer map[skillRun]skillCandidate, key skillRun, event record.Record) {
	current, seen := buffer[key]
	if !seen {
		buffer[key] = skillCandidate{chosen: event}
		return
	}
	current.extra++
	if cmp.Or(event.Timestamp.Compare(current.chosen.Timestamp), cmp.Compare(string(event.EventID), string(current.chosen.EventID))) < 0 {
		current.chosen = event
	}
	buffer[key] = current
}

type call struct {
	id          string
	eventID     record.Hash
	sessionID   record.Identifier
	timestamp   time.Time
	version     record.Version
	kind        record.Kind
	name        record.Identifier
	packageName record.Identifier
	viaSkill    record.Identifier
	viaAgent    record.Identifier
	model       record.Identifier
	invoker     record.Invoker
	entrypoint  record.Entrypoint
	repo        record.Hash
}

// callStatus separates a tool_use block a validated field refused from one Wake
// deliberately does not collect. A value a validated field refuses — the
// primitive's own name, or an entrypoint outside Wake's vocabulary — is a
// fail-closed drop worth counting (ADR-0007); an unusable id, an unconsented
// repository, and a call that predates the instant collection began for its
// repository are all a clean zero, which activation already reports as a skip
// rather than a failure, and must not be counted as a refusal.
type callStatus int

const (
	callSkipped callStatus = iota
	callAccepted
	callRefused
)

func (entry transcriptEntry) call(block contentBlock, resolve Resolver, names record.Namer) (call, callStatus) {
	id, err := record.BoundedToken(block.ID)
	if err != nil {
		return call{}, callSkipped
	}
	sessionID, err := record.BoundedToken(entry.SessionID)
	if err != nil {
		return call{}, callSkipped
	}
	// The call's own instant, which is the one the boundary is about: a call that
	// started before collection began is history the user declined, whenever its
	// result arrives. It is the same value the record carries, taken once.
	timestamp := record.NormalizedTimestamp(entry.Timestamp)
	repo, consented := resolve(entry.CWD, timestamp)
	if !consented {
		return call{}, callSkipped
	}
	// Named last, after every reason this call was never Wake's to collect. A
	// refusal is reported as lost collection, so it may only count a call that
	// would otherwise have been collected: a nameless call in a directory the user
	// never consented to is outside collection, not lost from it.
	name, err := primitiveName(block, names)
	if err != nil {
		return call{}, callRefused
	}
	// After consent and the name gate, for the same reason the name gate is last: a
	// refusal is reported as lost collection, so it may only count a call that would
	// otherwise have been collected.
	entrypoint, known := entrypointFor(entry.Entrypoint)
	if !known {
		return call{}, callRefused
	}

	derived := call{
		id:         string(id),
		eventID:    record.DeriveEventID(harness, callSourceEvent(entry.UUID, id)),
		sessionID:  sessionID,
		timestamp:  timestamp,
		kind:       kindFor(record.Identifier(block.Name)),
		name:       name,
		invoker:    record.InvokerModel,
		entrypoint: entrypoint,
		repo:       repo,
	}
	if version, err := record.BoundedVersion(entry.Version); err == nil {
		derived.version = version
	}
	if model, err := record.BoundedIdentifier(entry.Message.Model); err == nil {
		derived.model = model
	}
	if skill, err := names.DerivedName(entry.AttributionSkill); err == nil {
		derived.viaSkill = skill
	}
	if agent, err := names.DerivedName(entry.AttributionAgent); err == nil {
		derived.viaAgent = agent
	}
	if packageName, ok := packageFromAttribution(entry.AttributionMCPServer); ok {
		derived.packageName = packageName
	}
	return derived, callAccepted
}

// attributedSkillCandidate derives the record an attributed skill run *would* emit.
// It is a candidate, not a record: Claude Code puts a skill's identity on every entry
// of the turn it belongs to, and an end_turn entry alone does not say whether that run
// was already described by a Skill tool_use/tool_result pair. Only the session says,
// and only once it has closed (ADR-0023 §2-§3) — so Read buffers what this returns and
// resolveSessionSkills decides its fate.
//
// Building the whole record here rather than at resolution time is what keeps the
// fail-closed guarantee cheap: consent, the token domain and the name grammar are all
// checked before anything is buffered, so a candidate that exists has already passed
// every gate and nothing can be emitted past a failed one (ADR-0007, ADR-0019,
// ADR-0020).
//
// A sidechain entry is excluded outright. A subagent's own turn inherits the
// parent's attributionSkill, so it meets this condition without being a skill
// invocation at all — the commonest attributed shape on a real machine (ADR-0023 §1
// and its Context). Nothing about such a turn is uncertain, so it is dropped on
// sight rather than deferred.
//
// It deliberately derives nothing from attributionAgent, which Claude Code stamps
// on a subagent's entries the same way. A subagent is entered through the Task
// tool, so the parent's tool_use/tool_result pair already describes that same run —
// and describes it better: bounded by a start and an end rather than inferred from
// a stop reason (ADR-0015), and carrying an outcome, which an end_turn entry never
// does (ADR-0005). Deriving a record from both would make one run two invocations
// of one primitive: same name, same kind, same invoker, so they merge on one
// aggregation key. That is what the store's collapse guarantee ("two sources
// producing the same logical event collapse to one record") and ADR-0002's
// invocation grain forbid, and the event ids are legitimately distinct (ADR-0004),
// so the collapse has to happen here rather than at write time. plan §5.1 names
// the Task call as the subagent primitive's source; attributionAgent's own role is
// via_agent attribution on the calls a subagent makes (see call).
func (entry transcriptEntry) attributedSkillCandidate(resolve Resolver, names record.Namer) (record.Record, bool) {
	if entry.Message.StopReason != "end_turn" || entry.AttributionSkill == "" || entry.IsSidechain {
		return record.Record{}, false
	}

	primitive, err := names.DerivedName(entry.AttributionSkill)
	if err != nil {
		return record.Record{}, false
	}
	sessionID, err := record.BoundedToken(entry.SessionID)
	if err != nil {
		return record.Record{}, false
	}
	timestamp := record.NormalizedTimestamp(entry.Timestamp)
	repo, consented := resolve(entry.CWD, timestamp)
	if !consented {
		return record.Record{}, false
	}
	entrypoint, known := entrypointFor(entry.Entrypoint)
	if !known {
		return record.Record{}, false
	}

	event := record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID(harness, record.Identifier(entry.UUID)),
		Timestamp:     timestamp,
		Harness:       harness,
		SessionID:     sessionID,
		Repo:          repo,
		Kind:          record.KindSkill,
		Name:          primitive,
		Invoker:       record.InvokerUser,
		Entrypoint:    entrypoint,
	}
	if version, err := record.BoundedVersion(entry.Version); err == nil {
		event.HarnessVersion = version
	}
	if model, err := record.BoundedIdentifier(entry.Message.Model); err == nil {
		event.Model = model
	}
	return event, true
}

func primitiveName(block contentBlock, names record.Namer) (record.Identifier, error) {
	switch block.Name {
	case "Skill":
		if block.Input.Skill != "" {
			return names.DerivedName(block.Input.Skill)
		}
	case "Task":
		// Every subagent invocation carries the same tool name, so the tool name is
		// not the primitive: input.subagent_type is. It is derived, not bounded,
		// because a subagent can be directory-scoped ("apps/web:reviewer") and only
		// Namer may digest a scope (ADR-0020). There is no fall-through: a Task call
		// naming no subagent is refused rather than collected as "Task", which would
		// merge every distinct subagent into one primitive (ADR-0002) — and
		// DerivedName already refuses the empty value, so this needs no extra check.
		return names.DerivedName(block.Input.SubagentType)
	}
	return record.BoundedIdentifier(block.Name)
}

// complete pairs an accepted call with the result that terminated it. It takes the
// already-derived callResult rather than the source entry and block, so every
// terminal path — forward order, out of order, and the staleness rule — shares one
// derivation and one stamping rule, and a call can only ever yield one shape of
// record.
func (call call) complete(result callResult) record.Record {
	return record.Record{
		SchemaVersion:  record.SchemaVersion,
		EventID:        call.eventID,
		Timestamp:      record.NormalizedTimestamp(result.timestamp),
		Harness:        harness,
		HarnessVersion: call.version,
		SessionID:      call.sessionID,
		Repo:           call.repo,
		Kind:           call.kind,
		Name:           call.name,
		Package:        call.packageName,
		ViaSkill:       call.viaSkill,
		ViaAgent:       call.viaAgent,
		Model:          call.model,
		Invoker:        call.invoker,
		Entrypoint:     call.entrypoint,
		Outcome:        result.outcome,
	}
}

// interrupted is the terminal record for a call whose session closed without ever
// producing a result (ADR-0015's staleness rule). outcome interrupted means "did not
// finish" and is never a synthesised ok (ADR-0005); record.IsFailure already counts
// it, so this newly makes that path live and error rates will move — the intended
// effect of ADR-0015, not a regression.
//
// It goes through complete, like both result-terminated paths, so the id it carries
// is the id the completed record would have carried (ADR-0004): a result arriving
// after this record was written is deduplicated away rather than upserted, which is
// the alternative ADR-0015 rejected.
//
// The timestamp is the call's own tool_use timestamp rather than the session's last
// activity: the invocation grain's timestamp is when the call happened, and last
// activity would make the same logical event serialise differently once unrelated
// later lines are appended to the session. There is no result entry on this path, so
// a result-derived timestamp is unavailable by construction.
func (call call) interrupted() record.Record {
	outcome := record.OutcomeInterrupted
	return call.complete(callResult{timestamp: call.timestamp, outcome: &outcome})
}

// entrypointFor maps Claude Code's entrypoint spelling onto Wake's vocabulary.
// The empty source value is absence and maps to the unset field; anything the
// switch does not name is refused by the caller, never blanked and never
// guessed at (ADR-0005, ADR-0008 — this is a lookup, not an inference).
func entrypointFor(value string) (record.Entrypoint, bool) {
	switch value {
	case "":
		return "", true
	case "cli":
		return record.EntrypointCLI, true
	case "sdk-py":
		return record.EntrypointSDKPython, true
	case "sdk-cli":
		return record.EntrypointSDKCLI, true
	}
	return "", false
}

func kindFor(name record.Identifier) record.Kind {
	switch name {
	case "Skill":
		return record.KindSkill
	case "Task":
		return record.KindSubagent
	}
	if len(name) > 5 && string(name[:5]) == "mcp__" {
		return record.KindMCPTool
	}
	return record.KindBuiltinTool
}

func packageFromAttribution(value string) (record.Identifier, bool) {
	const prefix = "plugin:"
	if len(value) <= len(prefix) || value[:len(prefix)] != prefix {
		return "", false
	}
	for index := len(prefix); index < len(value); index++ {
		if value[index] == ':' {
			// The name domain, not the derived one: an MCP server package is never
			// directory-scoped, so a segment carrying a separator is a hostile value
			// rather than a scope to digest.
			packageName, err := record.BoundedIdentifier(value[len(prefix):index])
			return packageName, err == nil
		}
	}
	return "", false
}

func outcomeFor(entry transcriptEntry, block contentBlock) *record.Outcome {
	switch entry.ToolDenialKind {
	case "permission-rule":
		outcome := record.OutcomeDeniedPolicy
		return &outcome
	case "user-rejected":
		outcome := record.OutcomeDeniedUser
		return &outcome
	}
	if entry.interruptedResult() {
		outcome := record.OutcomeInterrupted
		return &outcome
	}
	if block.IsError == nil {
		return nil
	}
	if *block.IsError {
		outcome := record.OutcomeError
		return &outcome
	}
	outcome := record.OutcomeOK
	return &outcome
}
