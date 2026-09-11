// Package opencode derives safe terminal records from opencode's own store.
//
// It consumes value rows and nothing else: the caller opens the store, pages it,
// and hands this package rows whose fields are already the allowlist (ADR-0019
// §1). Nothing here touches the filesystem, a database handle, config or the
// inventory — the consent answer, the configured MCP servers and both thresholds
// all arrive as data, so no reader can widen what is collected.
package opencode

import (
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// harness is the slug every record this package derives carries, and the
// namespace ADR-0004 derives every id inside.
const harness = record.Identifier("opencode")

// No call separator is declared here, and that is worth stating. opencode's
// canonical source event is a single primary key, so an invocation's id shape is
// a bare part.id with no composed separator at all — structurally disjoint from
// every Claude Code id shape, and namespaced by harness inside DeriveEventID in
// any case. The one composed id this package builds is the session grain's, in
// session.go.

// Resolver maps one observed event — a working directory the harness recorded and
// the instant it happened — to a consented repository hash.
//
// It returns false when the event is outside consent, which has two dimensions and
// one answer: the directory was never consented, or the event predates the instant
// collection began for its repository (ADR-0024, ADR-0025). The reader passes the
// event's own timestamp and never learns the boundary, so no adapter can widen
// consent in either dimension.
type Resolver func(cwd string, at time.Time) (record.Hash, bool)

// Staleness carries ADR-0015's rule into one scan: how long an unterminated
// invocation may go unresolved before it is emitted as interrupted, and the
// instant to compare against.
//
// The zero value disables the rule, which is what a caller that cannot read its
// threshold must do: ADR-0015 rejects upsert and ADR-0004 deduplicates, so an
// interrupted record emitted too early is permanent and uncorrectable, while one
// emitted too late is still correct when it arrives.
type Staleness struct {
	Timeout time.Duration
	Now     time.Time
}

// Enabled reports whether this scan may give up on anything at all.
func (s Staleness) Enabled() bool { return s.Timeout > 0 && !s.Now.IsZero() }

// Idleness carries ADR-0034's rule: how long a session id may be silent before it
// is believed finished. A second type beside Staleness rather than a field on it,
// because they answer different questions — see the first adapter's session_end.go
// for why they are never one.
type Idleness struct {
	Timeout time.Duration
	Now     time.Time
}

// Enabled reports whether this scan may believe any session finished.
func (i Idleness) Enabled() bool { return i.Timeout > 0 && !i.Now.IsZero() }

// Result is one walk's derived records plus its collection health counters.
type Result struct {
	Records []record.Record
	// Pending is the count of tool parts this walk is holding because they have no
	// terminal status yet (ADR-0015). It is a number that is not final rather than
	// collection that was lost.
	Pending int
	// Interrupted is the count of parts the staleness rule gave up on, each now in
	// Records with outcome interrupted.
	Interrupted int
	// Refused is the count of parts a validated field refused — a tool name the
	// name grammar will not admit, a session id outside the token domain, a part
	// whose session this walk never saw. Lost collection, counted so doctor can
	// say so (plan §3.3, §12). The refused value is never carried, only the count.
	Refused int
	// UnknownOutcomes is the count of parts carrying a state.status this build does
	// not recognise. It is the format-drift detector for this harness: blindness,
	// not a clean zero, and doctor's per-harness state word follows it.
	UnknownOutcomes int
	// SkippedSources is the count of sessions this walk read that yielded no record
	// at all — most often because their directory belongs to no consented
	// repository. An honest zero, never a failure.
	SkippedSources int
	// OutOfOrderPairs is the count of parts whose end instant precedes their start.
	// The record is written with a nil duration rather than a clamped 0: a clamped
	// 0 would be a measurement, and nothing measured it (ADR-0027).
	OutOfOrderPairs int
}

// derivation is one attempt at a record: what it produced, and whether a
// validated field refused its source value. A refusal carries the fact and never
// the value (plan §4.2).
type derivation struct {
	record  record.Record
	refused bool
}

// invocation derives the record for one terminal tool part.
//
// Deliberately absent, and stated rather than implied: ViaSkill, ViaAgent, Model,
// Effort, Entrypoint, Package and ParentEventID. opencode's skill and subagent
// attribution lives inside state.input, which is free text the record allowlist
// forbids reading (ADR-0007), so KindSkill and KindSubagent are never claimed for
// this harness and the "skill" and "task" tool spellings are collected as builtin
// tools like any other. That is an absence observed and reported as such, never a
// zero (ADR-0046).
func invocation(part ToolPart, from Session, repo record.Hash, servers Servers, outcome record.Outcome, duration *int64) derivation {
	derived := record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID(harness, record.Identifier(part.ID)),
		Timestamp:     record.NormalizedTimestamp(time.UnixMilli(part.StartMS).UTC()),
		Harness:       harness,
		Repo:          repo,
		// A part hangs off an assistant message: opencode records no user-issued
		// tool call, so there is no second case to distinguish here.
		Invoker:    record.InvokerModel,
		Outcome:    &outcome,
		DurationMS: duration,
	}
	// The version is an optional field, so a value outside its domain leaves it
	// empty rather than refusing an invocation that really happened.
	if version, err := record.BoundedVersion(from.Version); err == nil {
		derived.HarnessVersion = version
	}
	sessionID, err := record.BoundedToken(part.SessionID)
	if err != nil {
		return derivation{refused: true}
	}
	derived.SessionID = sessionID

	name, err := record.BoundedIdentifier(part.Tool)
	if err != nil {
		return derivation{refused: true}
	}
	derived.Name = name

	derived.Kind = record.KindBuiltinTool
	if server, matched := servers.Match(part.Tool); matched {
		derived.Kind = record.KindMCPTool
		derived.MCPServer = server
	}
	return finish(derived)
}

// toolDuration is the exact interval between the two instants the harness itself
// recorded, or nil where there is none to state.
//
// A missing end instant and an end that precedes its start both yield nil: the
// first measured nothing, and the second measured something impossible. Neither
// becomes 0, which on this field means a call that returned inside the source's
// resolution (ADR-0027).
func toolDuration(part ToolPart) (*int64, bool) {
	if !part.HasEnd {
		return nil, false
	}
	elapsed := part.EndMS - part.StartMS
	if elapsed < 0 {
		return nil, true
	}
	return &elapsed, false
}

// finish is the fail-closed gate every derived record passes through: a record
// this package would not itself accept is dropped and counted, never written and
// never repaired (ADR-0007, plan §3.4).
//
// It is also where a timestamp outside the representable range is caught. A
// hostile or corrupt instant can put a record's time outside the year range JSON
// can encode, and a record that cannot be marshalled is one the store would drop
// later, further from the counter that can explain it.
func finish(derived record.Record) derivation {
	if year := derived.Timestamp.Year(); year < 1 || year > 9999 {
		return derivation{refused: true}
	}
	if err := record.Validate(derived); err != nil {
		return derivation{refused: true}
	}
	return derivation{record: derived}
}
