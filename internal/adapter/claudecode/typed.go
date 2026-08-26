package claudecode

import (
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// commandTagOpen and commandTagClose delimit the name of a skill or command a person
// typed. Claude Code writes them itself into the user turn's message body — plan §5.1
// records the shape (`<command-name>/x` on the user message) and ADR-0023's Context
// measured it — so finding a name between them is pattern-matching a delimiter the
// harness produced, never inference from prose (ADR-0008, plan §3.3). ADR-0036 §1
// makes the entry carrying this tag the canonical source event for that invocation.
const (
	commandTagOpen  = "<command-name>"
	commandTagClose = "</command-name>"
)

// commandTag returns the name declared by the first command tag in text, or "" when
// text carries none or leaves one unterminated.
//
// Only the first tag is read, so one entry is at most one typed invocation. That is
// what keeps typedSourceEvent unique per entry: a second name on one line would need
// a second id component the source event does not supply, and inventing one would be
// the write-time id generation ADR-0004 forbids.
//
// The leading slash a person types is stripped exactly once, because that is the
// difference between the two spellings the harness has written ("/pr-review" and
// "pr-review") and not a general unprefixing rule. The name is returned for the
// grammar to judge; nothing here decides it is safe to persist.
func commandTag(text string) string {
	open := strings.Index(text, commandTagOpen)
	if open < 0 {
		return ""
	}
	body := text[open+len(commandTagOpen):]
	end := strings.Index(body, commandTagClose)
	if end < 0 {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(body[:end]), "/")
}

// InstalledPrimitive is one primitive the machine has, as the reader is told about it:
// a bounded name and a kind, and nothing else.
//
// It is exported so a caller can build the set without this package importing
// internal/inventory. That direction is the point — see Installed.
type InstalledPrimitive struct {
	Name record.Identifier
	Kind record.Kind
}

// Installed is the set of primitive names this machine has, injected as data.
//
// ADR-0036 §3 admits a name read from a command tag only if the machine actually has a
// primitive under it. That check needs the installed inventory, which lives on disk,
// and derivation may never touch the filesystem (ADR-0019 §1) — so the answer arrives
// as a value, exactly as the consent resolver and the name digester already do. The
// caller reads the filesystem; what crosses into this package is a map of bounded
// names.
//
// The map is private so NewInstalled is the only way to build one. A caller handed a
// public map could bypass the fold below, and the fold is what makes the answer
// order-independent.
//
// The zero value knows nothing, which is fail closed: a caller that could not build an
// inventory collects no typed invocation rather than admitting every name it reads
// (plan §3.4).
type Installed struct {
	kinds map[record.Identifier]record.Kind
}

// NewInstalled folds primitives into the lookup the reader is handed.
//
// Two rules, both of them about not guessing. A name outside the name domain is
// skipped: it could not be persisted anyway, and the grammar is the same gate on both
// sides of the comparison (ADR-0020, ADR-0007). And a name present under two different
// kinds is admitted under neither — ~/.claude/skills/review/ and
// ~/.claude/commands/review.md can both exist, and picking one would be a guess at a
// record dimension decided by whichever the caller happened to discover first, which
// ADR-0005 forbids and ADR-0004's order-independence rules out.
//
// The fold is order-independent by construction: an ambiguous name is recorded as
// ambiguous rather than merely deleted, so a later repeat of either kind cannot
// reinstate it. Any input order therefore yields the same kept map and the same
// dropped set.
func NewInstalled(primitives []InstalledPrimitive) Installed {
	kinds := make(map[record.Identifier]record.Kind, len(primitives))
	ambiguous := map[record.Identifier]struct{}{}
	for _, primitive := range primitives {
		if !record.ValidName(primitive.Name) {
			continue
		}
		if _, dropped := ambiguous[primitive.Name]; dropped {
			continue
		}
		existing, seen := kinds[primitive.Name]
		if seen && existing != primitive.Kind {
			delete(kinds, primitive.Name)
			ambiguous[primitive.Name] = struct{}{}
			continue
		}
		kinds[primitive.Name] = primitive.Kind
	}
	return Installed{kinds: kinds}
}

// kindOf reports the kind this machine has name installed under. A nil map — the zero
// Installed — reads as not known, so no branch has to special-case it.
func (i Installed) kindOf(name record.Identifier) (record.Kind, bool) {
	kind, known := i.kinds[name]
	return kind, known
}

// typedSeparator delimits the two halves of a typed invocation's source identity. It
// is a file separator, and it is a fifth distinct byte on purpose: a token-domain
// value can contain none of them, a tool call's composed identity carries 0x1f, a
// session_end's carries 0x1e, a subagent run's carries 0x1d, and a Shape-A fallback
// derives from a bare entry uuid with no separator at all. The five id shapes are
// therefore structurally disjoint — no transcript, hostile or otherwise, can craft a
// typed-invocation id that collides with any other shape (ADR-0004).
//
// The disjointness is load-bearing here and not merely tidy: one hostile entry can
// carry both a command tag and the stop_reason/attributionSkill pair the Shape-A path
// reads, and those are two logically different records. Sharing an id would make the
// first one written win forever, since ADR-0015 rejects upsert.
const typedSeparator = "\x1c"

// typedSequence fills ADR-0004's sequence role for this shape.
//
// It is the fixed literal "typed" and deliberately not the resolved kind. The kind is
// a property of the machine's current inventory rather than of the source event, so
// keying on it would make the same typed invocation re-derive a different id after the
// user converts a command into a skill — a stored id no later build can re-derive,
// which is the failure ADR-0004 exists to close.
const typedSequence = "typed"

// typedSourceEvent identifies one typed invocation by the entry carrying its command
// tag, which ADR-0036 §1 names its canonical source event. Both halves come from the
// transcript or are constant: no ordinal, no write time, no randomness — so the same
// transcript re-derives the same id forever and re-ingestion stays a no-op.
func typedSourceEvent(entryUUID string) record.Identifier {
	return record.Identifier(entryUUID + typedSeparator + typedSequence)
}

// tagStatus separates the four outcomes a command tag can have, and it is its own type
// rather than a fourth callStatus member on purpose: scan.go's two existing callStatus
// switches would otherwise gain an arm they silently ignore.
//
// It mirrors callStatus's skip-versus-refuse distinction (reader.go) with one extra arm.
// tagRefused is a validated field refusing a value on an invocation that would otherwise
// have been collected — lost collection worth counting, which keeps RefusedCalls' drift
// signal. tagNotInstalled is neither that nor silence: ADR-0036 §3 settles it as a skip,
// because a typed CLI built-in like /clear was never Wake's to collect so nothing was
// lost, and it is the common case rather than the edge — roughly 101 of 136 observed
// occurrences. It is still counted rather than dropped in silence, because the installed
// set is injected and therefore fallible: a name absent from it may be a built-in or may
// be a primitive since uninstalled or renamed, and nothing in the transcript tells the
// two apart.
type tagStatus int

const (
	tagAbsent tagStatus = iota
	tagAccepted
	tagNotInstalled
	tagRefused
)

// typedInvocation derives the record for a skill or command a person typed, which
// ADR-0036 §1 keys to the entry carrying the delimited command tag and §3 counts once
// per occurrence.
//
// The gates run in one fixed order, and the order is the whole of what the three
// statuses mean:
//
// A sidechain entry is excluded outright. A subagent's own turn carries the parent's
// message body, so it can hold the parent's tag without being a typed invocation at all
// — the same exclusion attributedSkillCandidate applies for the same reason (ADR-0023
// §1). Then the tag itself, the session token and consent: each of those is something
// that was never this invocation's to collect, or never Wake's, so each is a clean zero
// and not a count of anything.
//
// The name grammar and the installed lookup come next and share one status. A name the
// grammar refuses also fails ADR-0036 §3's gate — there is one question, "is this a
// primitive this machine has", and two ways to answer no — so splitting them would
// invent a distinction the ADR does not draw.
//
// Only a skill or a command is admissible, which is exactly ADR-0036 §1's row for this
// canonical source. A tag whose name matches an installed subagent must not produce a
// record: §2 makes the subagent's own transcript its canonical source and gives it
// Invoker: model, so admitting one here would report one run twice under two invokers.
//
// The entrypoint gate is last, after the installed lookup, so a typed CLI built-in on an
// entry with an unmapped entrypoint is a skip and not a refusal — nothing was lost. A
// name this machine does have, refused there, is lost collection and counts.
//
// The whole record is built before anything is returned, so every gate has run before a
// caller can emit it — the discipline attributedSkillCandidate states and the reason a
// candidate that exists has already passed every check (ADR-0007).
func (entry transcriptEntry) typedInvocation(resolve Resolver, names record.Namer, installed Installed) (record.Record, tagStatus) {
	if entry.IsSidechain {
		return record.Record{}, tagAbsent
	}
	tag := entry.Message.Content.command
	if tag == "" {
		return record.Record{}, tagAbsent
	}
	sessionID, err := record.BoundedToken(entry.SessionID)
	if err != nil {
		return record.Record{}, tagAbsent
	}
	timestamp := record.NormalizedTimestamp(entry.Timestamp)
	repo, consented := resolve(entry.CWD, timestamp)
	if !consented {
		return record.Record{}, tagAbsent
	}
	primitive, err := names.DerivedName(tag)
	if err != nil {
		return record.Record{}, tagNotInstalled
	}
	kind, known := installed.kindOf(primitive)
	if !known || (kind != record.KindSkill && kind != record.KindCommand) {
		return record.Record{}, tagNotInstalled
	}
	entrypoint, mapped := entrypointFor(entry.Entrypoint)
	if !mapped {
		return record.Record{}, tagRefused
	}

	event := record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID(harness, typedSourceEvent(entry.UUID)),
		Timestamp:     timestamp,
		Harness:       harness,
		SessionID:     sessionID,
		Repo:          repo,
		Kind:          kind,
		Name:          primitive,
		Invoker:       record.InvokerUser,
		Entrypoint:    entrypoint,
		// Outcome is left nil deliberately. A typed invocation has no completion
		// boundary in the transcript — the tag records a person typing it, not the run
		// finishing — and a synthesized ok is exactly what ADR-0005 forbids. Unknown is
		// never success, so it is excluded from rate denominators rather than counted
		// (ADR-0023 §3 takes the same position for the Shape-A record).
	}
	// Best-effort, as call's are: a version or model outside its domain leaves the
	// optional field unset rather than refusing an invocation that happened.
	if version, err := record.BoundedVersion(entry.Version); err == nil {
		event.HarnessVersion = version
	}
	if model, err := record.BoundedIdentifier(entry.Message.Model); err == nil {
		event.Model = model
	}
	return event, tagAccepted
}
