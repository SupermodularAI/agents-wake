package opencode

import (
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

func servers(names ...string) Servers {
	configured := make([]record.Identifier, 0, len(names))
	for _, name := range names {
		configured = append(configured, record.Identifier(name))
	}
	return NewServers(configured)
}

func TestMatchResolvesARealServerPrefix(t *testing.T) {
	server, matched := servers("atlassian", "notion").Match("atlassian_search")
	if !matched || server != "atlassian" {
		t.Fatalf("Match(atlassian_search) = (%q, %t), want (atlassian, true)", server, matched)
	}
}

func TestMatchRefusesABuiltinShapedLikeAPrefix(t *testing.T) {
	// The match runs forward only, from a configured server to the spelling a tool
	// name would carry (ADR-0039). Nothing here is configured under "apply" or
	// "list", so no underscore in these names means anything.
	index := servers("atlassian", "notion")
	for _, tool := range []string{
		"apply_patch", "list_mcp_resources", "list_mcp_resource_templates",
		"todowrite", "webfetch", "read", "bash", "grep", "glob", "task",
		"skill", "edit", "write", "question",
	} {
		if server, matched := index.Match(tool); matched {
			t.Errorf("Match(%q) = (%q, true), want no match", tool, server)
		}
	}
}

func TestMatchSanitisesAConfiguredKey(t *testing.T) {
	server, matched := servers("plugin:context7:context7").Match("plugin_context7_context7_query")
	if !matched || server != "plugin_context7_context7" {
		t.Fatalf("Match = (%q, %t), want (plugin_context7_context7, true)", server, matched)
	}
}

func TestMatchDropsAnAmbiguousSpelling(t *testing.T) {
	// Two configured keys sanitising onto one spelling resolve to neither: an
	// ambiguous match is no match, so neither key absorbs the other's calls.
	if server, matched := servers("a:b", "a b").Match("a_b_thing"); matched {
		t.Fatalf("Match(a_b_thing) = (%q, true), want no match", server)
	}
}

func TestMatchPrefersTheLongerServer(t *testing.T) {
	server, matched := servers("foo", "foo_bar").Match("foo_bar_baz")
	if !matched || server != "foo_bar" {
		t.Fatalf("Match(foo_bar_baz) = (%q, %t), want (foo_bar, true)", server, matched)
	}
}

func TestMatchNeverParsesAnUnderscore(t *testing.T) {
	// With nothing configured, an underscore is just a character in a builtin's
	// name. A reader that split on it would invent servers out of tool names.
	empty := NewServers(nil)
	for _, tool := range []string{"atlassian_search", "apply_patch", "a_b_c", "foo_bar_baz"} {
		if server, matched := empty.Match(tool); matched {
			t.Errorf("Match(%q) with nothing configured = (%q, true), want no match", tool, server)
		}
	}
}

func TestMatchRefusesASpellingOutsideTheTokenDomain(t *testing.T) {
	// The spelling is persisted as record.MCPServer, a bounded token. A configured
	// key whose spelling is not one is dropped rather than stored — fail closed.
	if server, matched := servers("_leading").Match("_leading_tool"); matched {
		t.Fatalf("Match = (%q, true), want a spelling outside the token domain to be dropped", server)
	}
}

func TestMatchNeedsASeparator(t *testing.T) {
	// A server's own name is not a tool call on it, and a name that merely starts
	// with the same letters is not either.
	index := servers("atlassian")
	for _, tool := range []string{"atlassian", "atlassiansearch", "atlassian-search"} {
		if server, matched := index.Match(tool); matched {
			t.Errorf("Match(%q) = (%q, true), want no match", tool, server)
		}
	}
}
