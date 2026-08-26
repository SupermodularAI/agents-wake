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
	// OpenSessions counts the sessions this scan's entries belong to that have not
	// closed under the staleness rule. It is walk-wide: a session is open if any source
	// of the scan shows recent activity for it (ADR-0036). A session some source read
	// blind is never reported closed, for the same reason no call of it is resolved.
	OpenSessions int
	// CursorFloor is the byte offset of the earliest line belonging to any session
	// still open, inside the one source Read read. A future incremental cursor (T020,
	// T102) must not advance past it until that session closes: ADR-0023 §5 generalizes
	// ADR-0015's "does not advance its cursor past [an unterminated call]" to the
	// session grain, and ADR-0036 widens "still open" from one file to every file of the
	// session. It is meaningful only when OpenSessions is positive.
	//
	// Scan leaves it zero, deliberately: a floor is per source, so a walk has no single
	// one, and a Scan caller reads SessionState.SourceFloor for each source instead.
	CursorFloor int64
	// Refused counts invocations dropped because a validated field refused its
	// source value: a name the name/scope grammar refuses, or an entrypoint outside
	// Wake's vocabulary. Two derivations feed it — a tool call and an attributed
	// skill run — because each is an invocation that happened and that no number
	// will carry otherwise. Fail closed (ADR-0007): nothing is written and no
	// placeholder name is substituted.
	//
	// It is what a line of this source could not be used for, so it is reported by
	// Read. A subagent run has RefusedSubagentRuns instead, for the reason stated
	// there.
	//
	// It is deliberately not Malformed, which
	// means "a line that is unusable" and feeds doctor's drift signal, and
	// deliberately not the store's Dropped, which counts records refused at write
	// time. The value that was refused is never carried — only the count (plan
	// §4.2).
	Refused int
	// RefusedSubagentRuns counts subagent runs the post-walk resolution could not name:
	// the transcript declared no name at all, declared one the name/scope grammar
	// refuses, or declared a directory-scoped one with no scope key to digest it under
	// (ADR-0036 §2, ADR-0020). Same fail-closed rule as Refused, and the same silence it
	// exists to prevent — the run happened and no number carries it.
	//
	// It is its own counter rather than a second half of Refused, because the two are
	// different kinds of loss and doctor treats them differently. Refused is what a
	// source's own line could not be used for, and a harness renaming the field a
	// primitive's identity lives in is exactly that — which is why it blinds the
	// integration state. A run refused here is a standing fact about a transcript
	// instead: ADR-0036 §2 measured 2% of real ones declaring no name and refuses to
	// name them from the harness's documented default, so no release makes the count
	// fall, and with no incremental cursor every scan refuses the same runs again. A
	// state word driven by it could never change back (see health.Diagnose).
	//
	// Only Close reports it. A subagent run can be judged only once its session has
	// closed, so a Read of one source has no answer at all and leaves it zero.
	RefusedSubagentRuns int
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
	// SkippedTypedInvocations counts typed invocations this scan read the command tag
	// of and derived no record from, because the name it declared is not a primitive
	// this machine has: the injected known-name set does not hold it, or holds it under
	// a kind ADR-0036 §1's precedence row does not cover, or the name/scope grammar
	// refuses it (ADR-0020).
	//
	// It is a skip and not a refusal, and that is ADR-0036 §3's own distinction: a typed
	// CLI built-in like /clear was never Wake's to collect, so nothing was lost — and it
	// is the common case, roughly 101 of 136 observed occurrences, not the edge. Routing
	// it onto Refused would pin doctor to "collects nothing" on every scan forever while
	// thousands of records are written.
	//
	// It is nonetheless counted rather than silent, because the known-name set is
	// injected and therefore fallible: a name absent from it may be a built-in or may be
	// a primitive since uninstalled or renamed, and nothing in the transcript tells the
	// two apart. It deliberately does not move doctor's state word — same reasoning as
	// RefusedSubagentRuns and ADR-0023's ambiguous skill run (see health.Diagnose).
	//
	// It carries no name, no session id and no transcript value — only how many times
	// the question came up (plan §4.2, ADR-0007). Read reports it for the one source it
	// read.
	SkippedTypedInvocations int
	// SkippedSources counts the sources this scan read that derived nothing, refused
	// nothing, and had nothing attributed back to them by the post-walk resolution —
	// most often because their working directory belongs to no consented repository.
	// It is doctor's Skipped counter, moved to where the source ordinals live.
	//
	// A per-source caller cannot compute it any more. Before ADR-0036 resolution
	// happened inside one source's read, so "parsed nothing and refused nothing" was
	// the whole story for that file; now a source's contribution can resolve after the
	// walk — a stale call given up on, a share of a session_end's totals — and a zero
	// at the end of its own read no longer separates an honest zero from a deferral.
	//
	// It is a count of sources, never of paths: a source is the ordinal the scan
	// assigned it (ADR-0007, plan §4.2). Read reports it for the one source it read.
	SkippedSources int
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
// installed carries the set of primitive names this machine has. ADR-0036 §3 admits a
// name read from a command tag only if the machine has a primitive under it, and that
// answer lives on disk while derivation may not touch the filesystem (ADR-0019 §1) — so
// it arrives as data, exactly as resolve and names do. Its zero value admits nothing,
// which is what a caller that could not build an inventory must collect: no typed
// invocation at all rather than every name it reads.
//
// stale carries ADR-0015's staleness rule (scan.stale_call_timeout and the scan's
// clock) from the caller that owns config, so the threshold is never a constant
// here. Its zero value buffers every unterminated call, which is what a caller that
// cannot read the threshold must do.
//
// idle carries ADR-0014's session-end inference threshold (session.idle_timeout)
// from the same caller, for the same reason and with the same shape. It is a
// second threshold answering a different question — when a session id is believed
// finished, rather than when an unterminated call is given up on — and its zero
// value derives no session_end at all.
//
// It is the one-source form of Scan, kept because a caller reading a single
// transcript should not have to drive a walk. A caller reading a *set* of sources
// that may share a session id must use Scan instead: resolving each file on its own
// closes a session another file shows running and reports one session_end per file
// rather than one per session, permanently (ADR-0036 §Consequences).
func Read(reader io.Reader, resolve Resolver, names record.Namer, installed Installed, stale Staleness, idle Idleness) (Result, error) {
	scan := NewScan(resolve, names, installed, stale, idle)
	first, err := scan.Read(reader)
	if err != nil {
		return Result{}, err
	}
	final := scan.Close()
	// One source, so its floor is the scan's floor and CursorFloor means what it
	// always meant. A Scan caller asks SessionState.SourceFloor per source instead,
	// because a walk has no single floor.
	if open, offset := scan.sessions.SourceFloor(0, stale); open {
		final.CursorFloor = offset
	}
	// This source's own records first, then the post-walk ones — today's order
	// exactly, so one transcript serialises character-for-character as it did before.
	final.Records = append(first.Records, final.Records...)
	final.Malformed = first.Malformed
	// Summed rather than assigned, though Close derives no tool-call or skill-run
	// refusal of its own today: a future one added there would otherwise be silently
	// dropped, which is exactly the lost collection this counter exists to make visible.
	// A refused subagent run arrives on Close's own RefusedSubagentRuns and passes
	// through untouched.
	final.Refused += first.Refused
	// Summed for the same defensive reason, though a typed invocation is judged entirely
	// within the line it is on, so Close derives none of its own today.
	final.SkippedTypedInvocations += first.SkippedTypedInvocations
	return final, nil
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
// A candidate whose name is in typed is dropped on the same terms, and that is
// ADR-0036 §4's narrowing: ADR-0023 §4's collapse now applies only to a run for which
// neither a tool_use block nor a command tag exists. Where a tag exists it is the
// canonical source (§1), its record is already emitted per occurrence (§3), and a
// fallback here would be a second record for one invocation. Its extras are treated
// exactly as a matched run's for the same reason — nothing about a run a person typed
// is uncertain, so counting them as ambiguity would report doubt where the transcript
// has none.
//
// Where neither exists, nothing has changed: no signal distinguishes one run from two,
// and one record per (session, skill name) remains the honest answer.
//
// The order is sorted rather than the map's, for the reason resolveStaleCalls gives:
// iteration order is randomised and two scans of one transcript have to produce
// byte-identical store contents. The key is the chosen candidate's own (timestamp,
// event id), a total order because one transcript entry yields at most one candidate.
//
// It returns derivations rather than bare records so each can be credited to the
// source it came from. What the function decides is unchanged: the closed gate, the
// order, the matched-drop and the extra accounting are the same as before ADR-0036
// widened the buffer from one file to one walk (ADR-0036 §4).
func resolveSessionSkills(buffer map[skillRun]skillCandidate, invoked, typed map[skillRun]struct{}, sessions *SessionState, stale Staleness) ([]derivation, int) {
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
	records := make([]derivation, 0, len(resolved))
	ambiguous := 0
	for _, key := range resolved {
		candidate := buffer[key]
		delete(buffer, key)
		if _, matched := invoked[key]; matched {
			continue
		}
		if _, wasTyped := typed[key]; wasTyped {
			// The tag is this invocation's canonical source and its record is already
			// emitted, so a fallback here would be a second record for one invocation —
			// the duplication ADR-0036 §4 narrows the collapse to remove. Its collapsed
			// extras are not ambiguity either: nothing about this run is uncertain.
			continue
		}
		records = append(records, derivation{event: candidate.chosen, source: candidate.source})
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
// the call's own tool_use timestamp, with the session id and then the block id
// breaking a tie — not the line's position in the file, which the buffer does not
// retain. The tie-break is the buffer's whole key, which is what keeps the order
// total now that the buffer is keyed by (session, block id): a block id alone is
// unique per call inside one file but not across a walk. That is ADR-0004
// determinism under a wider key, not a change to what this function decides.
func resolveStaleCalls(pending map[callKey]call, sessions *SessionState, stale Staleness) []derivation {
	resolved := make([]callKey, 0, len(pending))
	for key, buffered := range pending {
		if sessions.Closed(buffered.sessionID, stale) {
			resolved = append(resolved, key)
		}
	}
	slices.SortFunc(resolved, func(a, b callKey) int {
		return cmp.Or(
			pending[a].timestamp.Compare(pending[b].timestamp),
			cmp.Compare(a.session, b.session),
			cmp.Compare(a.id, b.id),
		)
	})
	records := make([]derivation, 0, len(resolved))
	for _, key := range resolved {
		buffered := pending[key]
		records = append(records, derivation{
			event:   buffered.interrupted(),
			source:  buffered.source,
			agentID: buffered.agentID,
		})
		delete(pending, key)
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
	// AgentID is the id of the subagent whose transcript this entry belongs to.
	// Claude Code writes it on every entry of a subagent's own file and on no entry
	// of a parent transcript (measured: 32787/32787 and 0/39039), so its presence is
	// what identifies a subagent transcript — from content, never from the path
	// convention subagents/agent-<agentId>.jsonl, because derivation may not touch
	// the filesystem (ADR-0019 §1, ADR-0036 §2 "keyed by the agent id it declares").
	//
	// It is consumed into a derived event id and never persisted: no record field
	// carries it (ADR-0007).
	AgentID string `json:"agentId"`
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
	// ID is the assistant API message's own id. It is read for exactly one purpose:
	// deduplicating a usage block Claude Code repeats verbatim on every transcript
	// line belonging to one API message — measured at three to seven lines per
	// message on a real transcript, so summing per line would multiply a session's
	// token totals several-fold. It is never persisted and reaches no record field
	// (ADR-0007).
	ID         string `json:"id"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	// Usage stays raw for the reason ToolUseResult does: a typed field type-errors
	// the whole line, and that line may be the tool_result that terminates a
	// buffered call (ADR-0015). Only the five allowlisted numbers are read out of
	// it, by usage below; the block's unmodelled siblings — cache_creation,
	// iterations, server_tool_use, service_tier, inference_geo, speed on a real
	// transcript — reach nothing. Reading a payload is not persisting one.
	Usage   json.RawMessage `json:"usage"`
	Content messageContent  `json:"content"`
}

// messageContent decodes message.content in both shapes real Claude Code writes: an
// array of content blocks on an assistant turn, and a plain string on a user turn —
// which is where a typed invocation's delimited command tag lives (plan §5.1,
// ADR-0023's Context, ADR-0036 §3). A typed field for the array alone made the string
// case a *json.UnmarshalTypeError, which cost the whole entry, so nothing could be
// derived from the line the tag is on.
//
// The string is read and not retained: UnmarshalJSON keeps only the bounded name
// commandTag extracted from it and discards the body, which is the discipline the Skill
// tool_use path already applies to its args field (ADR-0007, reading is not
// persisting). It reaches no record field, no error and no log line (plan §4.2).
type messageContent struct {
	blocks  []contentBlock
	command string
}

// UnmarshalJSON reads whichever of the two shapes this content is.
//
// The default branch delegates to the block decode rather than returning an error of
// its own, and that is load-bearing: it reproduces today's exact behaviour for every
// shape except the string one — null succeeds with a nil slice, an object, a number and
// a bool each produce the same *json.UnmarshalTypeError they did before — so
// inspectable() and Result.Malformed are unchanged for all of them.
func (c *messageContent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		// Only the bounded name survives the decode. The body it came from is not
		// assigned anywhere and goes out of scope with this call.
		c.command = commandTag(text)
		return nil
	}
	return json.Unmarshal(trimmed, &c.blocks)
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
// remaining keys, so a field this reader does not model arriving as some other type
// costs the entry but not the reader's view of the line. Anything else, a syntax error
// above all, leaves nothing to look at.
//
// A message's content as a plain string used to be this comment's example and is no
// longer a mismatch at all: messageContent models that shape, because it is the one a
// typed invocation's command tag arrives on (ADR-0036 §3). What still reaches here is a
// content at a type the reader models neither of — an object, a number, a bool — which
// messageContent.UnmarshalJSON deliberately keeps costing the entry rather than
// guessing at.
//
// Whether such an entry should be salvaged rather than rejected is a separate
// question, and a bigger one than the staleness rule: the reader derives nothing from
// one. This only decides whether it may still conclude a session went silent.
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
	return slices.ContainsFunc(entry.Message.Content.blocks, func(block contentBlock) bool {
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
	// source is the ordinal of the source chosen was read from. It moves with chosen
	// so the credited source is as order-independent as the record itself; it is
	// never persisted.
	source int
	extra  int
}

// deferSkillCandidate folds one candidate into the buffer for its skillRun, keeping
// the earliest by (timestamp, event id) and counting the rest. source is the ordinal
// of the source the candidate was read from and travels with the chosen record.
//
// Earliest by a total order over values the source event supplies, never by arrival:
// nothing in the transcript format promises the entries are ordered — the reason
// SessionState.Observe folds a max and a min rather than last-wins — and a first-wins
// fold would let line order decide which id and timestamp the one emitted record
// carries, while two scans of one transcript have to produce byte-identical store
// contents (ADR-0004). The order is total because one entry yields at most one
// candidate, so no two candidates share an event id. After ADR-0036 the buffer spans
// a walk, so that same fold is now what makes the result independent of the order the
// walk visited the sources in.
func deferSkillCandidate(buffer map[skillRun]skillCandidate, key skillRun, source int, event record.Record) {
	current, seen := buffer[key]
	if !seen {
		buffer[key] = skillCandidate{chosen: event, source: source}
		return
	}
	current.extra++
	if cmp.Or(event.Timestamp.Compare(current.chosen.Timestamp), cmp.Compare(string(event.EventID), string(current.chosen.EventID))) < 0 {
		current.chosen = event
		current.source = source
	}
	buffer[key] = current
}

// callKey identifies one buffered call by the session it belongs to and its
// tool_use block id.
//
// The session half is not decoration. Keyed on the block id alone, a tool_result
// in one session's source could terminate a call from another's — a cross-session
// pairing ADR-0002's invocation grain and ADR-0034's per-session-id delimitation
// both forbid. That was safe only while one buffer's lifetime was one file; after
// ADR-0036 the buffer spans a walk, and block ids are unique per call and not per
// machine.
type callKey struct {
	session record.Identifier
	id      string
}

// derivation is one record the post-walk resolution produced and the ordinal of
// the source whose lines it came from.
//
// The ordinal exists only so a scan can tell a source that produced nothing from
// one whose contribution resolved after the walk ended — the distinction doctor's
// Skipped counter rests on. It is an ordinal, never a path (ADR-0007, plan §4.2).
type derivation struct {
	event  record.Record
	source int
	// agentID is the agent id of the transcript the record's entry belonged to,
	// empty when it belonged to none. Only resolveStaleCalls sets it: a Shape-A
	// fallback and a subagent record are ADR-0035 case 3 — a subagent is not
	// attributed to itself (see subagentRun.subagent).
	agentID record.Identifier
}

type call struct {
	id string
	// source is the ordinal of the source this call's tool_use line was read from.
	// It exists so a record the post-walk resolution derives can be credited back to
	// the source that fed it; it is never persisted.
	source      int
	eventID     record.Hash
	sessionID   record.Identifier
	timestamp   time.Time
	version     record.Version
	kind        record.Kind
	name        record.Identifier
	packageName record.Identifier
	viaSkill    record.Identifier
	viaAgent    record.Identifier
	// agentID is the id of the subagent transcript this call's tool_use line was
	// read from, empty when the entry declared none or declared one outside the
	// token domain. It is ADR-0035 §2's case-1 key and ADR-0036 §2's — the agent id
	// the transcript declares, never a ViaAgent name and never a lookup (ADR-0035
	// §1). It is consumed into a derived parent id and never persisted: no record
	// field carries it (ADR-0007).
	agentID record.Identifier
	model       record.Identifier
	invoker     record.Invoker
	entrypoint  record.Entrypoint
	repo        record.Hash
}

// callStatus separates an invocation a validated field refused from one Wake
// deliberately does not collect. Both derivations report it: a tool_use block, and
// an attributed skill run. A value a validated field refuses — the primitive's own
// name, or an entrypoint outside Wake's vocabulary — is a fail-closed drop worth
// counting (ADR-0007); an unusable id, an unconsented repository, and a call that
// predates the instant collection began for its repository are all a clean zero,
// which activation already reports as a skip rather than a failure, and must not be
// counted as a refusal.
type callStatus int

const (
	callSkipped callStatus = iota
	callAccepted
	callRefused
)

func (entry transcriptEntry) call(source int, block contentBlock, resolve Resolver, names record.Namer) (call, callStatus) {
	if subagentInvocation(block.Name) {
		// Skipped, not refused: this block was never Wake's to collect, so counting it
		// as lost collection would report a permanent fault for a rule working as
		// designed (ADR-0036 §2, and call's own ordering rule below).
		return call{}, callSkipped
	}
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
		source:     source,
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
	// The same record.BoundedToken gate observeSubagentRun applies, so this call
	// derives byte-identically the id subagentRun.subagent() derives — and an agentId
	// outside the token domain is a clean zero that falls through the precedence
	// rather than a refusal (ADR-0035 §2 case 1, ADR-0036 §2).
	if agentID, err := record.BoundedToken(entry.AgentID); err == nil {
		derived.agentID = agentID
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
// It deliberately derives no *skill* candidate from attributionAgent, which Claude
// Code stamps on a subagent's entries the same way. A subagent run has its own
// canonical source event — the subagent's own transcript, resolved at session close
// and keyed by the agentId that transcript declares (ADR-0036 §2, subagent.go) — so
// deriving a skill record from an attributed entry as well would make one run two
// invocations of one primitive: same name, same kind, same invoker, so they merge on
// one aggregation key. That is what the store's collapse guarantee ("two sources
// producing the same logical event collapse to one record") and ADR-0002's
// invocation grain forbid, and the event ids are legitimately distinct (ADR-0004),
// so the collapse has to happen here rather than at write time. attributionAgent's
// own role on this path is via_agent attribution on the calls a subagent makes (see
// call).
func (entry transcriptEntry) attributedSkillCandidate(resolve Resolver, names record.Namer) (record.Record, callStatus) {
	if entry.Message.StopReason != "end_turn" || entry.AttributionSkill == "" || entry.IsSidechain {
		return record.Record{}, callSkipped
	}

	primitive, err := names.DerivedName(entry.AttributionSkill)
	if err != nil {
		return record.Record{}, callSkipped
	}
	sessionID, err := record.BoundedToken(entry.SessionID)
	if err != nil {
		return record.Record{}, callSkipped
	}
	timestamp := record.NormalizedTimestamp(entry.Timestamp)
	repo, consented := resolve(entry.CWD, timestamp)
	if !consented {
		return record.Record{}, callSkipped
	}
	// The one gate on this path that counts, and it counts because it is the only
	// one that sits after consent: an entrypoint outside Wake's vocabulary refuses a
	// run this repository had agreed to collect, so the invocation is lost rather
	// than never Wake's. The gates above it are ordered the other way round from
	// call's — naming comes first here — so a refusal there could not tell an
	// unconsented turn from a lost one, and stays a clean zero (ADR-0007, plan §12).
	entrypoint, known := entrypointFor(entry.Entrypoint)
	if !known {
		return record.Record{}, callRefused
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
	return event, callAccepted
}

// primitiveName derives the primitive name one tool_use block stands for.
//
// No branch names "Agent" or "Task": call excludes a subagent invocation before
// reaching here, because the subagent's own transcript is that invocation's
// canonical source and the invoking block is the same logical event seen from the
// other side (ADR-0036 §2). A naming branch for either name would be unreachable.
func primitiveName(block contentBlock, names record.Namer) (record.Identifier, error) {
	if block.Name == "Skill" && block.Input.Skill != "" {
		return names.DerivedName(block.Input.Skill)
	}
	return record.BoundedIdentifier(block.Name)
}

// subagentInvocation reports whether a tool_use block is a subagent invocation.
// Both names are recognised, and both are recognised here rather than in
// primitiveName or kindFor, because the only thing this reader does with such a
// block is decline to record it: the subagent's own transcript is the canonical
// source event and the invoking block is the same logical event seen from the other
// side (ADR-0036 §1-§2). "Agent" is what real transcripts carry; "Task" is retained
// for one written by an older harness.
func subagentInvocation(name string) bool { return name == "Agent" || name == "Task" }

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

// kindFor classifies one tool_use block's primitive kind.
//
// No branch names "Agent" or "Task", for primitiveName's reason: call excludes a
// subagent invocation before reaching here (ADR-0036 §2). The KindSubagent records
// this adapter emits are built in subagent.go, which sets the kind directly.
func kindFor(name record.Identifier) record.Kind {
	if name == "Skill" {
		return record.KindSkill
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
