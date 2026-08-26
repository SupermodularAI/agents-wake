package claudecode

import (
	"bytes"
	"cmp"
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// Idleness carries ADR-0014's session-end inference threshold —
// session.idle_timeout — into one scan, with the instant last activity is
// compared against.
//
// It is a second type beside Staleness rather than a field on it, deliberately.
// ADR-0023 §3 requires Staleness.Timeout to be scan.stale_call_timeout for every
// caller, and session.go's own doc comment states that session.idle_timeout "must
// not be wired in here"; ADR-0034's Consequences require this ticket's closure
// predicate to be separate rather than an overload of SessionState.Closed. Two
// thresholds, two types, two predicates, one each.
//
// The zero value disables session_end derivation entirely, which is what a caller
// that cannot read the threshold must do: ADR-0015 rejects upsert and ADR-0004
// deduplicates, so a session_end written on a guessed threshold is permanent.
type Idleness struct {
	// Timeout is how long a session id may be silent before it is believed finished.
	Timeout time.Duration
	// Now is the instant this scan compares last activity against.
	Now time.Time
}

// enabled reports whether this scan may believe any session finished. A zero Now
// would make every session look infinitely idle and a non-positive Timeout would
// finish every session on sight; both write records that cannot be taken back.
func (i Idleness) enabled() bool { return i.Timeout > 0 && !i.Now.IsZero() }

// sessionSeparator delimits the two halves of a session_end's source identity. It
// is a record separator, and it is a different byte from callSeparator on purpose:
// no token-domain value can contain any of them, callSourceEvent's composed identity
// contains 0x1f and never 0x1e, subagentSourceEvent's contains 0x1d and never
// either, typedSourceEvent's contains 0x1c and never any of the others, and the
// Shape-A path derives from a bare entry uuid with no separator at all.
// The five id shapes are therefore structurally disjoint — no transcript, hostile or otherwise,
// can craft a session_end id that collides with a tool call's, an attributed run's,
// a subagent run's or a typed invocation's.
const sessionSeparator = "\x1e"

// sessionName is the Name a session-grain row carries. record.Validate requires a
// non-empty name-domain identifier and a session has no primitive name, so this is
// Wake's own constant for the row: it is not derived from the transcript and
// carries nothing of it.
const sessionName = record.Identifier("session")

// sessionEndSourceEvent identifies the one session_end a session id ever produces:
// the session id, plus a kind-discriminating component. Never a segment or a
// sequence number — ADR-0034 §1 settles that there is never more than one, so this
// is ADR-0004's (harness, session_id, sequence) fallback with the sequence role
// filled by the kind.
func sessionEndSourceEvent(sessionID record.Identifier) record.Identifier {
	return record.Identifier(string(sessionID) + sessionSeparator + string(record.KindSessionEnd))
}

// finishedSessions returns, in ascending session-id order, every session this
// state observed whose last activity is further back than session.idle_timeout.
//
// Deliberately not SessionState.Closed and deliberately not keyed on
// scan.stale_call_timeout. Strictly greater than the threshold, matching closed()'s
// rule: a session silent for exactly the threshold is still open, which errs toward
// not writing a record that cannot be taken back.
//
// A session this state never observed is never finished, for closed()'s reason:
// absence of evidence that a session is alive is not evidence that it ended.
//
// Liveness is taken from every line that named a session and a time — SessionState's
// own fold — not from the consented subset. A session id still writing lines in a
// directory the user never consented to is demonstrably alive, and believing it
// finished would be permanent. What the record is *dated* by is a different
// quantity and comes from sessionGrain.lastSeen.
//
// A blind session is never finished. Some source carrying it held a line the reader
// could not rule out as a terminator, so its last activity is known-understated and
// its totals would be understated by whatever that line held. The taint is per
// session and not per walk because after ADR-0036 a source's blindness bears on
// every source of the sessions that source carried, and only on those: a global
// switch would disable session_end derivation for every session on the machine over
// one unreadable line.
//
// Sorted rather than map order: two scans of one transcript have to produce
// byte-identical store contents (ADR-0004), and a session id is unique per entry
// so the order is total.
func (s *SessionState) finishedSessions(idle Idleness) []record.Identifier {
	if !idle.enabled() {
		return nil
	}
	sessions := make([]record.Identifier, 0, len(s.sessions))
	for sessionID, activity := range s.sessions {
		if activity.blind {
			continue
		}
		if idle.Now.Sub(activity.lastActivity) > idle.Timeout {
			sessions = append(sessions, sessionID)
		}
	}
	slices.Sort(sessions)
	return sessions
}

// messageUsage is the allowlisted subset of one assistant message's usage block:
// five numbers and nothing else. Every other key the block carries is unmodelled
// and reaches nothing — the record type is the allowlist, and so is this decode
// (ADR-0007).
type messageUsage struct {
	inputTokens         *int64
	outputTokens        *int64
	cacheReadTokens     *int64
	cacheCreationTokens *int64
	thinkingTokens      *int64
}

// usage reads the five allowlisted counts out of this entry's raw usage block.
//
// A block that is not a JSON object carries no structured usage and none is
// inferred from it (plan §3.3) — the rule interruptedResult already applies to a
// tool result payload. Anything else, a syntax error above all, leaves nothing to
// trust.
//
// The decode is two-pass, into raw keys and then one count at a time, rather than
// one pass into a struct of pointers. Decoding a mismatched value into a *int64
// allocates the pointer before it fails, so the struct comes back holding a
// non-nil pointer to zero — a fabricated measurement, which is the one wrong
// answer this reader may never produce (ADR-0005). Reading each key on its own is
// what makes "a field arriving at another type costs that field and not the block"
// true rather than merely intended, and it is the same rule inspectable states one
// level up for a whole line.
func (entry transcriptEntry) usage() (messageUsage, bool) {
	raw := bytes.TrimSpace(entry.Message.Usage)
	if len(raw) == 0 || raw[0] != '{' {
		return messageUsage{}, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return messageUsage{}, false
	}
	usage := messageUsage{
		inputTokens:         countIn(fields, "input_tokens"),
		outputTokens:        countIn(fields, "output_tokens"),
		cacheReadTokens:     countIn(fields, "cache_read_input_tokens"),
		cacheCreationTokens: countIn(fields, "cache_creation_input_tokens"),
	}
	// The one count Claude Code nests. A details object that is not an object costs
	// the count inside it and nothing else, for the reason above.
	var details map[string]json.RawMessage
	if err := json.Unmarshal(fields["output_tokens_details"], &details); err == nil {
		usage.thinkingTokens = countIn(details, "thinking_tokens")
	}
	return usage, true
}

// countIn reads one allowlisted count out of a decoded block. An absent key is the
// harness reporting nothing; a key at another type is the same answer, because a
// count this reader cannot read is not a count it may guess at.
func countIn(fields map[string]json.RawMessage, key string) *int64 {
	raw, present := fields[key]
	if !present {
		return nil
	}
	var count int64
	if err := json.Unmarshal(raw, &count); err != nil {
		return nil
	}
	return &count
}

// usageCounter accumulates one nullable session-grain total across one session's
// messages.
type usageCounter struct {
	total    int64
	reported bool
	// refused marks a source value this counter will not carry: a negative count,
	// or an addition that would overflow int64. The total then reports unknown
	// rather than a wrapped or partial number — the choice spanTimes already makes
	// for an unrepresentable timestamp, and the only wrong answer that cannot be
	// mistaken for a measurement (ADR-0005).
	refused bool
}

// add folds one message's reported count in. A nil value is the harness reporting
// nothing and is not a zero.
func (c *usageCounter) add(value *int64) {
	if value == nil {
		return
	}
	if *value < 0 || *value > math.MaxInt64-c.total {
		c.refused = true
		return
	}
	c.total += *value
	c.reported = true
}

// value is the nullable total this counter carries onto the record: nil unless
// something was reported and nothing was refused.
func (c usageCounter) value() *int64 {
	if !c.reported || c.refused {
		return nil
	}
	total := c.total
	return &total
}

// sessionAnchor is the earliest consented entry of a session, by (timestamp,
// uuid), whose entrypoint this build maps. It supplies every allowlisted dimension
// the session_end record carries that is not a total.
//
// Earliest by a total order over values the source supplies, never by arrival:
// nothing in the transcript format promises the entries are ordered
// (SessionState.Observe folds a max and a min for the same reason), and a
// first-wins fold would let line order decide which repository and version the one
// record carries.
type sessionAnchor struct {
	uuid       string
	timestamp  time.Time
	repo       record.Hash
	version    record.Version
	entrypoint record.Entrypoint
}

// sessionGrain is what one scan accumulates for one session id, from consented
// entries only: the anchor, the last consented activity that dates the record, the
// deduplicated message ids, the usage totals ADR-0034 §3 calls a snapshot, and the
// sources whose lines fed any of them.
type sessionGrain struct {
	anchor   sessionAnchor
	anchored bool
	lastSeen time.Time
	messages map[record.Identifier]struct{}
	// sources is exactly the set of sources whose consented lines contributed to this
	// session's totals or its anchor. It exists so the one session_end this grain
	// yields can be credited back to every source that fed it, which is what keeps
	// doctor's Skipped counter honest for a source whose only contribution resolved
	// after the walk. It holds source ordinals, never paths (ADR-0007, plan §4.2).
	sources map[int]struct{}

	inputTokens         usageCounter
	outputTokens        usageCounter
	cacheReadTokens     usageCounter
	cacheCreationTokens usageCounter
	thinkingTokens      usageCounter
}

// observeSessionGrain folds one usable entry into its session's grain state.
//
// Gated on consent exactly as call is, and against the entry's own instant: an
// entry outside a consented root, or before the instant collection began for its
// repository, contributes nothing — not a timestamp, not a token (ADR-0016 as
// narrowed by ADR-0025). Reading a cwd to answer that question is not persisting
// one.
//
// The entrypoint gate applies to the anchor only, not to the totals. An entrypoint
// outside Wake's vocabulary means this build cannot state that dimension for the
// entry, which is a reason not to anchor on it; it is not a reason to disbelieve a
// number the harness reported. If no entry of a session anchors, no session_end is
// derived at all — fail closed — and that same condition already refuses every
// call on those entries into Result.Refused, so the blindness is reported rather
// than silent.
// source is the ordinal of the source this entry was read from, recorded for every
// consented entry the grain folds — the sources this session's numbers actually
// came from, and no more.
func observeSessionGrain(grains map[record.Identifier]*sessionGrain, source int, entry transcriptEntry, resolve Resolver) {
	sessionID, err := record.BoundedToken(entry.SessionID)
	if err != nil {
		return
	}
	timestamp := record.NormalizedTimestamp(entry.Timestamp)
	repo, consented := resolve(entry.CWD, timestamp)
	if !consented {
		return
	}
	grain, seen := grains[sessionID]
	if !seen {
		grain = &sessionGrain{sources: map[int]struct{}{}}
		grains[sessionID] = grain
	}
	grain.sources[source] = struct{}{}
	if timestamp.After(grain.lastSeen) {
		grain.lastSeen = timestamp
	}
	grain.anchorEntry(entry, timestamp, repo)
	grain.addUsage(entry)
}

// anchorEntry folds one consented entry into this grain's anchor: the entrypoint
// gate, then the min by (timestamp, uuid). BoundedVersion is best-effort, as
// call's is.
func (g *sessionGrain) anchorEntry(entry transcriptEntry, timestamp time.Time, repo record.Hash) {
	entrypoint, known := entrypointFor(entry.Entrypoint)
	if !known {
		return
	}
	if g.anchored && cmp.Or(timestamp.Compare(g.anchor.timestamp), cmp.Compare(entry.UUID, g.anchor.uuid)) >= 0 {
		return
	}
	anchor := sessionAnchor{uuid: entry.UUID, timestamp: timestamp, repo: repo, entrypoint: entrypoint}
	if version, err := record.BoundedVersion(entry.Version); err == nil {
		anchor.version = version
	}
	g.anchor = anchor
	g.anchored = true
}

// addUsage folds one entry's usage block into the five counters, at most once per
// assistant message id. The dedup is the whole point (see message.ID's comment):
// an entry whose message id is absent or outside the token domain contributes
// nothing, because a block that cannot be identified cannot be recognised as a
// repeat, and counting it again is how a token total gets multiplied. Fail closed
// (ADR-0007).
func (g *sessionGrain) addUsage(entry transcriptEntry) {
	messageID, err := record.BoundedToken(entry.Message.ID)
	if err != nil {
		return
	}
	usage, structured := entry.usage()
	if !structured {
		return
	}
	if _, counted := g.messages[messageID]; counted {
		return
	}
	if g.messages == nil {
		g.messages = map[record.Identifier]struct{}{}
	}
	g.messages[messageID] = struct{}{}
	g.inputTokens.add(usage.inputTokens)
	g.outputTokens.add(usage.outputTokens)
	g.cacheReadTokens.add(usage.cacheReadTokens)
	g.cacheCreationTokens.add(usage.cacheCreationTokens)
	g.thinkingTokens.add(usage.thinkingTokens)
}

// invocationTally accumulates, per session, the invocation-grain records one scan
// has derived and the built-in tool calls among them.
//
// It is folded as records are derived rather than computed from a retained slice:
// after ADR-0036 the aggregate spans every source of a session, so the slice would
// have to be every record of a whole machine's history held in memory until the
// walk ended. It counts and holds no record — a session id and two integers
// (ADR-0007).
//
// Every invocation-grain record counts toward the total, including an ADR-0023
// Shape-A fallback: the count exists so a receiver can recover the denominator
// encodeSpan's built-in-tool omission destroys (ADR-0006), and total minus builtin
// has to stay the number of spans the session delivers.
//
// The zero value is ready to use.
type invocationTally struct {
	total   map[record.Identifier]int64
	builtin map[record.Identifier]int64
}

// observe folds one batch of derived records into the tally. The predicate is the
// one invocationCounts applied before it: a session-grain record is not an
// invocation and counts toward neither number.
func (t *invocationTally) observe(records []record.Record) {
	for _, event := range records {
		if record.IsSessionGrain(event.Kind) {
			continue
		}
		if t.total == nil {
			t.total = map[record.Identifier]int64{}
			t.builtin = map[record.Identifier]int64{}
		}
		t.total[event.SessionID]++
		if event.Kind == record.KindBuiltinTool {
			t.builtin[event.SessionID]++
		}
	}
}

// resolveSessionEnds derives the one session_end record for every session id this
// read found finished under session.idle_timeout, in the deterministic order
// finishedSessions returns.
//
// One record per session id per scan, and one per session id ever: the id is
// derived from (harness, session id, kind), so a later scan that finds the same
// session finished again re-derives the same id and the store deduplicates it —
// first write wins, the totals already on disk are never corrected, and no second
// record is written (ADR-0034 §1-§3, ADR-0015's rejected upsert). Nothing here
// compares payloads or prefers the more complete one.
//
// The aggregate is a snapshot: it sums the records this scan has already derived,
// so a call still buffered because scan.stale_call_timeout has not elapsed is
// simply not in it, exactly as ADR-0034 §3 describes. There is no waiting, no
// backfill and no reconciliation pass.
//
// The tally spans the whole walk rather than one source, which is what makes the
// aggregate the union's: after ADR-0036 a session's invocations are spread over its
// parent transcript and one file per subagent, and a per-file total would report
// each partial view as if it were the session's (ADR-0036 §Consequences).
func resolveSessionEnds(grains map[record.Identifier]*sessionGrain, sessions *SessionState,
	idle Idleness, tally *invocationTally) []record.Record {
	finished := sessions.finishedSessions(idle)
	if len(finished) == 0 {
		return nil
	}
	records := make([]record.Record, 0, len(finished))
	for _, sessionID := range finished {
		grain, observed := grains[sessionID]
		// No anchor means no consented, mappable entry, so there is no record to build
		// and none is invented.
		if !observed || !grain.anchored || grain.lastSeen.IsZero() {
			continue
		}
		records = append(records, grain.sessionEnd(sessionID, tally.total[sessionID], tally.builtin[sessionID]))
	}
	return records
}

// sessionEnd builds the record itself.
//
// Timestamp is the last consented activity, never time.Now(): two scans of one
// transcript must compute it identically (ADR-0004). Model and DurationMS are
// deliberately absent — a session may use several models, and the timestamp is
// fixed at last activity, so a session-length duration would render the span after
// the session ended. Outcome is nil: a session reports none, and nil is UNSET on
// the wire, never OK (ADR-0005). ToolCalls and BuiltinToolCalls are always
// present, including at zero: this scan counted them, and a measured zero is the
// plan §2.7 baseline.
//
// ParentEventID is deliberately unset, and it is the only record for which that is
// deliberate rather than a failure to establish one. A session_end is the one record
// ADR-0035 §2 leaves rootless: it is the trace root every other record of the session
// reaches through case 3, whose id is this record's own — derived from
// sessionEndSourceEvent whether or not this record has been written yet (§4).
func (g *sessionGrain) sessionEnd(sessionID record.Identifier, toolCalls, builtinToolCalls int64) record.Record {
	return record.Record{
		SchemaVersion:       record.SchemaVersion,
		EventID:             record.DeriveEventID(harness, sessionEndSourceEvent(sessionID)),
		Timestamp:           g.lastSeen,
		Harness:             harness,
		HarnessVersion:      g.anchor.version,
		SessionID:           sessionID,
		Repo:                g.anchor.repo,
		Kind:                record.KindSessionEnd,
		Name:                sessionName,
		Invoker:             record.InvokerAuto,
		Entrypoint:          g.anchor.entrypoint,
		InputTokens:         g.inputTokens.value(),
		OutputTokens:        g.outputTokens.value(),
		CacheReadTokens:     g.cacheReadTokens.value(),
		CacheCreationTokens: g.cacheCreationTokens.value(),
		ThinkingTokens:      g.thinkingTokens.value(),
		ToolCalls:           &toolCalls,
		BuiltinToolCalls:    &builtinToolCalls,
	}
}
