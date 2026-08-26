package claudecode

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// The delimiter is what the harness itself writes around a typed invocation's name,
// so reading it is pattern-matching a documented shape and never inference
// (ADR-0008, ADR-0036 §3). Both spellings are covered because both occur: the older
// one keeps the leading slash the person typed, the newer one does not.
func TestCommandTagReadsTheDelimitedName(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"the slashed spelling": {
			body: "<command-name>/pr-review</command-name>",
			want: "pr-review",
		},
		"the bare spelling": {
			body: "<command-name>plan-implementation</command-name>",
			want: "plan-implementation",
		},
		"a real multi-tag body": {
			body: "<command-message>foo</command-message>\n<command-name>/foo</command-name>\n<command-args></command-args>",
			want: "foo",
		},
		"padded by whitespace": {
			body: "<command-name>  /clear  </command-name>",
			want: "clear",
		},
		"no tag at all": {
			body: "carry on",
			want: "",
		},
		"an unterminated tag": {
			body: "<command-name>/foo",
			want: "",
		},
		"a close before an open": {
			body: "</command-name><command-name>",
			want: "",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := commandTag(testCase.body); got != testCase.want {
				t.Fatalf("commandTag(%q) = %q, want %q", testCase.body, got, testCase.want)
			}
		})
	}
}

// Only the first tag, so one entry is at most one typed invocation — which is what
// keeps typedSourceEvent unique per entry (ADR-0004).
func TestCommandTagReadsOnlyTheFirstTag(t *testing.T) {
	body := "<command-name>/first</command-name><command-name>/second</command-name>"
	if got := commandTag(body); got != "first" {
		t.Fatalf("commandTag(%q) = %q, want %q", body, got, "first")
	}
}

// A name can be installed under two kinds at once — ~/.claude/skills/review/ and
// ~/.claude/commands/review.md coexist. Picking one would be a scan-order-dependent
// guess at a record dimension, so the fold drops the name entirely and the tag lands
// on the skip counter instead. Both input orders are asserted because
// order-independence is the property, not the outcome of one ordering (ADR-0004).
func TestNewInstalledDropsANameInstalledUnderTwoKinds(t *testing.T) {
	orders := map[string][]InstalledPrimitive{
		"skill then command": {
			{Name: "review", Kind: record.KindSkill},
			{Name: "review", Kind: record.KindCommand},
		},
		"command then skill": {
			{Name: "review", Kind: record.KindCommand},
			{Name: "review", Kind: record.KindSkill},
		},
		"a repeat cannot reinstate the dropped name": {
			{Name: "review", Kind: record.KindSkill},
			{Name: "review", Kind: record.KindCommand},
			{Name: "review", Kind: record.KindSkill},
		},
	}
	for name, primitives := range orders {
		t.Run(name, func(t *testing.T) {
			if kind, known := NewInstalled(primitives).kindOf("review"); known {
				t.Fatalf("kindOf(%q) = %q, want not known", "review", kind)
			}
		})
	}

	repeated := NewInstalled([]InstalledPrimitive{
		{Name: "review", Kind: record.KindSkill},
		{Name: "review", Kind: record.KindSkill},
	})
	kind, known := repeated.kindOf("review")
	if !known || kind != record.KindSkill {
		t.Fatalf("kindOf(%q) = %q, %t, want %q, true", "review", kind, known, record.KindSkill)
	}
}

// The set is injected, so a caller could hand it anything. The gate is the name
// domain and nothing else: a name it refuses could not be persisted anyway, and using
// the same grammar on both sides is what keeps a tag's name comparable to an installed
// one (ADR-0007, ADR-0020).
//
// The corpus is asserted as an exact correspondence rather than a blanket refusal,
// because one of its values — the keyed scope digest "scope-<digest>:reviewer" — is a
// name the domain admits on purpose: it is the shape Namer.DerivedName produces for a
// directory-scoped primitive, so it is what a real inventory hands this set for one.
// Refusing it here would make a directory-scoped primitive's typed invocation
// uncollectable. What makes the corpus value hostile is a transcript hand-crafting it,
// which is the entry side's problem and is asserted there.
func TestNewInstalledAdmitsExactlyTheNameDomain(t *testing.T) {
	refused := 0
	for _, value := range hostileValues {
		name := record.Identifier(value)
		installed := NewInstalled([]InstalledPrimitive{{Name: name, Kind: record.KindSkill}})
		kind, known := installed.kindOf(name)
		if want := record.ValidName(name); known != want {
			t.Fatalf("kindOf(%q) known = %t, want %t (ValidName)", value, known, want)
		}
		if !known {
			refused++
			continue
		}
		if kind != record.KindSkill {
			t.Fatalf("kindOf(%q) = %q, want %q", value, kind, record.KindSkill)
		}
	}
	if refused == 0 {
		// Without this the correspondence above would pass vacuously if the corpus ever
		// stopped containing a value the name domain refuses.
		t.Fatal("no hostile value was refused: the name-domain assertion would be vacuous")
	}
}

// Fail closed: a caller that could not build an inventory collects no typed
// invocation at all, rather than admitting every name it reads (plan §3.4).
func TestZeroInstalledKnowsNothing(t *testing.T) {
	var installed Installed
	if kind, known := installed.kindOf("pr-review"); known {
		t.Fatalf("kindOf(%q) = %q, want not known", "pr-review", kind)
	}
}

// The fifth id shape has to be disjoint from the other four, or one entry that
// satisfies two derivation paths would write two logically different records onto one
// id — and ADR-0015 rejects upsert, so the first one written would win forever.
func TestTypedSourceEventIsDisjointFromEveryOtherIDShape(t *testing.T) {
	const uuid = "entry-1"
	typed := typedSourceEvent(uuid)
	others := map[string]record.Identifier{
		"a tool call":          callSourceEvent(uuid, "call-1"),
		"a session end":        sessionEndSourceEvent(record.Identifier(uuid)),
		"a subagent run":       subagentSourceEvent(record.Identifier(uuid)),
		"a Shape-A fallback":   record.Identifier(uuid),
		"the same typed shape": typedSourceEvent(uuid),
	}
	for name, other := range others {
		if name == "the same typed shape" {
			if other != typed {
				t.Fatalf("typedSourceEvent is not stable: %q != %q", other, typed)
			}
			continue
		}
		if other == typed {
			t.Fatalf("typedSourceEvent(%q) collides with %s: %q", uuid, name, typed)
		}
	}
	if !strings.Contains(string(typed), typedSeparator) {
		t.Fatalf("typedSourceEvent(%q) = %q, want it to carry the typed separator", uuid, typed)
	}
	if strings.ContainsAny(string(typed), callSeparator+sessionSeparator+subagentSeparator) {
		t.Fatalf("typedSourceEvent(%q) = %q, want no other shape's separator", uuid, typed)
	}
}

// typedEntry is an entry that satisfies every gate typedInvocation applies, so a test
// can break exactly one of them and say which.
func typedEntry(tag string) transcriptEntry {
	return transcriptEntry{
		UUID:       "entry-1",
		SessionID:  "session-1",
		CWD:        consentedPath,
		Timestamp:  callInstant,
		Version:    "1.0.0",
		Entrypoint: "cli",
		Message:    message{Model: "sonnet", Content: messageContent{command: tag}},
	}
}

// A subagent's own turn inherits the parent's message body, so it can carry the
// parent's tag without being a typed invocation at all. It is dropped on sight, the
// same rule attributedSkillCandidate applies (ADR-0023 §1).
func TestTypedInvocationSkipsASidechainCopy(t *testing.T) {
	entry := typedEntry("pr-review")
	entry.IsSidechain = true

	if _, status := entry.typedInvocation(resolver, names, installedPrimitives); status != tagAbsent {
		t.Fatalf("typedInvocation() status = %v, want tagAbsent", status)
	}
}

func TestTypedInvocationSkipsAnEntryWithNoTag(t *testing.T) {
	if _, status := typedEntry("").typedInvocation(resolver, names, installedPrimitives); status != tagAbsent {
		t.Fatalf("typedInvocation() status = %v, want tagAbsent", status)
	}
}

// Outside consent is outside collection, not lost from it, so it must not read as a
// skipped invocation either — a clean zero all the way through (ADR-0024, ADR-0025).
func TestTypedInvocationSkipsAnUnconsentedDirectory(t *testing.T) {
	if _, status := typedEntry("pr-review").typedInvocation(deny, names, installedPrimitives); status != tagAbsent {
		t.Fatalf("typedInvocation() status = %v, want tagAbsent", status)
	}
}

// The name grammar and the installed miss share one counter: a name the grammar
// refuses also fails ADR-0036 §3's gate, so there is one answer to give and it is the
// skip, never a refusal and never a record.
func TestTypedInvocationCountsANameTheGrammarRefuses(t *testing.T) {
	for _, value := range hostileValues {
		event, status := typedEntry(value).typedInvocation(resolver, names, installedPrimitives)
		if status != tagNotInstalled {
			t.Fatalf("typedInvocation(%q) status = %v, want tagNotInstalled", value, status)
		}
		if event.Name != "" || event.EventID != "" {
			t.Fatalf("typedInvocation(%q) built a record: %+v", value, event)
		}
	}
}

// ADR-0036 §3: a tag naming something the machine has no primitive for is a skip. A
// typed CLI built-in like /clear was never Wake's to collect, so nothing was lost.
func TestTypedInvocationCountsANameTheMachineDoesNotHave(t *testing.T) {
	installed := NewInstalled([]InstalledPrimitive{{Name: "pr-review", Kind: record.KindSkill}})

	if _, status := typedEntry("clear").typedInvocation(resolver, names, installed); status != tagNotInstalled {
		t.Fatalf("typedInvocation() status = %v, want tagNotInstalled", status)
	}
}

// D3: only a skill or a command is admissible, which is the whole of ADR-0036 §1's row
// for this canonical source. A subagent's canonical source is its own transcript and it
// gets Invoker: model there (§2), so admitting one here would report one run twice under
// two invokers.
func TestTypedInvocationCountsANameInstalledAsAnotherKind(t *testing.T) {
	kinds := []record.Kind{record.KindSubagent, record.KindMCPTool, record.KindPlugin, record.KindHook}
	for _, kind := range kinds {
		installed := NewInstalled([]InstalledPrimitive{{Name: "explorer", Kind: kind}})
		if _, status := typedEntry("explorer").typedInvocation(resolver, names, installed); status != tagNotInstalled {
			t.Fatalf("typedInvocation() status for kind %q = %v, want tagNotInstalled", kind, status)
		}
	}
}

// The one gate that counts as a refusal, and it is last for that reason: an entrypoint
// outside Wake's vocabulary loses an invocation this machine had a primitive for and
// this repository had consented to. That is the drift signal RefusedCalls exists for.
func TestTypedInvocationRefusesAnUnmappedEntrypoint(t *testing.T) {
	entry := typedEntry("pr-review")
	entry.Entrypoint = "sdk-ts"

	if _, status := entry.typedInvocation(resolver, names, installedPrimitives); status != tagRefused {
		t.Fatalf("typedInvocation() status = %v, want tagRefused", status)
	}
}

// The record's Kind follows the machine's inventory, which is what gives
// record.KindCommand its first producer. Outcome stays nil: a typed invocation has no
// completion boundary and a synthesized ok is forbidden (ADR-0005, ADR-0023 §3).
func TestTypedInvocationDerivesASkillAndACommand(t *testing.T) {
	for _, kind := range []record.Kind{record.KindSkill, record.KindCommand} {
		t.Run(string(kind), func(t *testing.T) {
			installed := NewInstalled([]InstalledPrimitive{{Name: "pr-review", Kind: kind}})
			event, status := typedEntry("pr-review").typedInvocation(resolver, names, installed)
			if status != tagAccepted {
				t.Fatalf("typedInvocation() status = %v, want tagAccepted", status)
			}
			if event.Kind != kind {
				t.Errorf("Kind = %q, want %q", event.Kind, kind)
			}
			if event.Name != "pr-review" || event.Invoker != record.InvokerUser || event.Outcome != nil {
				t.Errorf("record = %+v", event)
			}
			if event.Repo != repo || event.SessionID != "session-1" || event.Harness != harness {
				t.Errorf("record = %+v", event)
			}
			if event.HarnessVersion != "1.0.0" || event.Model != "sonnet" || event.Entrypoint != record.EntrypointCLI {
				t.Errorf("record = %+v", event)
			}
			if want := record.DeriveEventID(harness, record.Identifier("entry-1"+typedSeparator+typedSequence)); event.EventID != want {
				t.Errorf("EventID = %q, want %q", event.EventID, want)
			}
			if err := record.Validate(event); err != nil {
				t.Errorf("Validate() error = %v", err)
			}
		})
	}
}

// One hostile entry can satisfy both derivation paths at once. The two are logically
// different records, so they must not land on one id — ADR-0015 rejects upsert, so the
// first one written would win forever (D4).
func TestTypedInvocationIDDoesNotCollideWithTheAttributedRunID(t *testing.T) {
	entry := typedEntry("pr-review")
	entry.AttributionSkill = "pr-review"
	entry.Message.StopReason = "end_turn"

	typed, typedStatus := entry.typedInvocation(resolver, names, installedPrimitives)
	if typedStatus != tagAccepted {
		t.Fatalf("typedInvocation() status = %v, want tagAccepted", typedStatus)
	}
	attributed, attributedStatus := entry.attributedSkillCandidate(resolver, names)
	if attributedStatus != callAccepted {
		t.Fatalf("attributedSkillCandidate() status = %v, want callAccepted", attributedStatus)
	}
	if typed.EventID == attributed.EventID {
		t.Fatalf("both paths derived event id %q from one entry", typed.EventID)
	}
}

// typedTurn is one consented user turn whose message content is the plain string a
// typed invocation arrives on, carrying the command tag for name.
func typedTurn(uuid, name, at string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"sessionId":"session-1","cwd":"/repo","timestamp":%q,"type":"user","message":{"role":"user","content":"<command-name>/%s</command-name>"}}`,
		uuid, at, name,
	)
}

// AC 1: ADR-0036 §3 counts a typed invocation once per occurrence, because two
// occurrences are two invocations. ADR-0023 §4's collapse used to make three one.
func TestReadDerivesOneRecordPerTypedOccurrence(t *testing.T) {
	input := strings.Join([]string{
		typedTurn("entry-1", "pr-review", "2026-08-13T12:00:00Z"),
		typedTurn("entry-2", "pr-review", "2026-08-13T12:00:01Z"),
		typedTurn("entry-3", "pr-review", "2026-08-13T12:00:02Z"),
	}, "\n")

	result, err := read(strings.NewReader(input), resolver, names, closedSession)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 3 {
		t.Fatalf("Read() records = %d, want 3: %+v", len(result.Records), result.Records)
	}
	if result.SkippedTypedInvocations != 0 || result.Malformed != 0 || result.Refused != 0 {
		t.Errorf("Read() = %+v", result)
	}
	seen := map[record.Hash]struct{}{}
	for _, event := range result.Records {
		if event.Kind != record.KindSkill || event.Name != "pr-review" || event.Invoker != record.InvokerUser {
			t.Errorf("record = %+v", event)
		}
		if _, repeated := seen[event.EventID]; repeated {
			t.Errorf("event id %q derived twice", event.EventID)
		}
		seen[event.EventID] = struct{}{}
	}
}

// AC 4: a tag naming something the machine has no primitive for yields no record and
// is counted on its own counter — never on Refused, which would pin doctor to
// "collects nothing" on every scan forever (ADR-0036 §3).
func TestReadCountsATypedInvocationTheMachineDoesNotHave(t *testing.T) {
	input := typedTurn("entry-1", "clear", "2026-08-13T12:00:00Z")

	result, err := read(strings.NewReader(input), resolver, names, closedSession)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 {
		t.Fatalf("Read() records = %+v, want none", result.Records)
	}
	if result.SkippedTypedInvocations != 1 || result.Refused != 0 || result.Malformed != 0 {
		t.Fatalf("Read() = %+v", result)
	}
}
