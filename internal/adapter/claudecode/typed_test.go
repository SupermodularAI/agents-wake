package claudecode

import (
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
