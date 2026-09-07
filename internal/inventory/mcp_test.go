package inventory

import (
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// TestServerSpellingMatchesTheRealToolNameSpellings pins the rule against the
// spellings this repo's own harness config really produces. It is the whole of the
// normalisation: if a case here is wrong, the join credits the wrong server.
//
// The space case is defensive rather than reachable: a discovered name arrives
// through record.Namer, whose grammar refuses a space, so no Primitive.Name can
// carry one. It is pinned because the rule it states is Claude Code's, not Wake's
// — the harness composes the tool name from whatever the config key holds — and a
// rule that silently stopped covering a character class would be discovered by a
// mismatched row rather than by a test.
func TestServerSpellingMatchesTheRealToolNameSpellings(t *testing.T) {
	for _, testCase := range []struct{ key, want string }{
		{"claude-in-chrome", "claude-in-chrome"},
		{"plugin:context7:context7", "plugin_context7_context7"},
		{"claude.ai Atlassian Rovo", "claude_ai_Atlassian_Rovo"},
		{"linear", "linear"},
		{"supabase", "supabase"},
	} {
		if got := serverSpelling(testCase.key); got != testCase.want {
			t.Errorf("serverSpelling(%q) = %q, want %q", testCase.key, got, testCase.want)
		}
	}
}

// TestServerIndexDropsAnAmbiguousSpelling pins the fail-closed rule: two distinct
// config keys that sanitise to one spelling resolve to neither, so no server
// absorbs another's counters. Neither key is that spelling here, so the observed
// segment matches nothing and its row is published unmatched.
func TestServerIndexDropsAnAmbiguousSpelling(t *testing.T) {
	index, discovered := serverIndex([]Primitive{
		{Harness: "claude-code", Kind: record.KindMCPServer, Name: "a:b"},
		{Harness: "claude-code", Kind: record.KindMCPServer, Name: "a.b"},
	})
	if _, held := index[serverKey{harness: "claude-code", spelling: "a_b"}]; held {
		t.Fatalf("index resolved an ambiguous spelling: %+v", index)
	}
	if got := serverName(index, "claude-code", "a_b"); got != "a_b" {
		t.Errorf("serverName() = %q, want the observed segment %q", got, "a_b")
	}
	if len(discovered) != 2 {
		t.Errorf("discovered = %+v, want both keys", discovered)
	}
}

// A key that is already its own spelling is not made ambiguous by another key
// sanitising onto it: the segment is that key, letter for letter, and naming it
// requires no inverse. The index still drops the spelling, and serverName returns
// the segment — which is the same string, so the calls land on the discovered row
// rather than on an unmatched one.
func TestServerIndexStillNamesAKeyThatIsItsOwnSpelling(t *testing.T) {
	exact := record.Identifier("a_b")
	index, discovered := serverIndex([]Primitive{
		{Harness: "claude-code", Kind: record.KindMCPServer, Name: "a:b"},
		{Harness: "claude-code", Kind: record.KindMCPServer, Name: exact},
	})
	if got := serverName(index, "claude-code", "a_b"); got != exact {
		t.Errorf("serverName() = %q, want %q", got, exact)
	}
	if _, found := discovered[identity{harness: "claude-code", kind: record.KindMCPServer, name: exact}]; !found {
		t.Errorf("discovered = %+v, want the exactly spelled key", discovered)
	}
}

// The index is per harness: a name one harness configured never resolves a segment
// observed under another, and never makes it look discovered.
func TestServerIndexKeepsHarnessesApart(t *testing.T) {
	index, discovered := serverIndex([]Primitive{
		{Harness: "claude-code", Kind: record.KindMCPServer, Name: "linear"},
	})
	if got := serverName(index, "codex", "linear"); got != "linear" {
		t.Errorf("serverName() = %q, want the observed segment unchanged", got)
	}
	if _, found := discovered[identity{harness: "codex", kind: record.KindMCPServer, name: "linear"}]; found {
		t.Errorf("discovered = %+v, want no codex entry", discovered)
	}
}

// TestServerIndexIgnoresPrimitivesOfOtherKinds keeps the index cross-kind-free: a
// skill named "linear" must never lend its name to an MCP server segment.
func TestServerIndexIgnoresPrimitivesOfOtherKinds(t *testing.T) {
	index, discovered := serverIndex([]Primitive{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "linear"},
	})
	if len(index) != 0 || len(discovered) != 0 {
		t.Fatalf("index = %+v, discovered = %+v, want both empty", index, discovered)
	}
	if got := serverName(index, "claude-code", "linear"); got != "linear" {
		t.Errorf("serverName() = %q, want the observed segment unchanged", got)
	}
}
