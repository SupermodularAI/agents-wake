package inventory

import (
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// TestServerSpellingMatchesTheRealToolNameSpellings pins the rule against the
// spellings this repo's own harness config really produces. It is the whole of the
// normalisation: if a case here is wrong, the join credits the wrong server.
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
// absorbs another's counters. Both stay unmatched instead.
func TestServerIndexDropsAnAmbiguousSpelling(t *testing.T) {
	index, discovered := serverIndex([]Primitive{
		{Harness: "claude-code", Kind: record.KindMCPServer, Name: "a:b"},
		{Harness: "claude-code", Kind: record.KindMCPServer, Name: "a_b"},
	})
	if _, held := index["a_b"]; held {
		t.Fatalf("index resolved an ambiguous spelling: %+v", index)
	}
	if got := serverName(index, "a_b"); got != "a_b" {
		t.Errorf("serverName() = %q, want the observed segment %q", got, "a_b")
	}
	if len(discovered) != 2 {
		t.Errorf("discovered = %+v, want both keys", discovered)
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
	if got := serverName(index, "linear"); got != "linear" {
		t.Errorf("serverName() = %q, want the observed segment unchanged", got)
	}
}
