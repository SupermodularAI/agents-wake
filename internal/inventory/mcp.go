package inventory

import (
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// serverSpelling is the tool-name spelling of one MCP server config key.
//
// It runs forward, from the config key discovery found to the segment a tool name
// would carry, because only that direction is a function: Claude Code composes a
// tool name by replacing every character of the key outside [A-Za-z0-9_-] with
// "_", so "plugin:context7:context7" is invoked as "plugin_context7_context7" and
// "claude.ai Atlassian Rovo" as "claude_ai_Atlassian_Rovo", while "claude-in-chrome"
// is invoked unchanged. Reading that backwards is ambiguous — "foo_bar" could be
// either spelling of itself — so the inverse is never attempted, and a segment this
// index does not hold is reported as unmatched rather than guessed at (ADR-0008,
// plan §3.3, §12). "linear-server" against a configured "linear" is exactly that
// case: no rule connects them, so none is invented.
func serverSpelling(key string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, key)
}

// serverIndex is the match from an observed tool-name segment to the discovered
// server it belongs to, plus the set of server names discovery actually found.
//
// A spelling two distinct config keys produce is dropped from the index rather
// than resolved to one of them: an ambiguous match is no match, so both keys stay
// unmatched instead of one absorbing the other's counters (fail closed, plan §3.4)
// — the same refusal canonicalSkillNames makes when two sources contribute one
// bare skill name.
func serverIndex(available []Primitive) (map[string]record.Identifier, map[record.Identifier]struct{}) {
	index := map[string]record.Identifier{}
	ambiguous := map[string]struct{}{}
	discovered := map[record.Identifier]struct{}{}
	for _, primitive := range available {
		if primitive.Kind != record.KindMCPServer {
			continue
		}
		discovered[primitive.Name] = struct{}{}
		spelling := serverSpelling(string(primitive.Name))
		if held, seen := index[spelling]; seen && held != primitive.Name {
			ambiguous[spelling] = struct{}{}
			continue
		}
		index[spelling] = primitive.Name
	}
	for spelling := range ambiguous {
		delete(index, spelling)
	}
	return index, discovered
}

// serverName resolves an observed segment onto the discovered server it names, or
// keeps the observed segment itself when nothing matches. The row that results then
// carries the segment the harness really used, which is the only honest name for a
// server nothing in the inventory accounts for.
func serverName(index map[string]record.Identifier, segment record.Identifier) record.Identifier {
	if name, matched := index[string(segment)]; matched {
		return name
	}
	return segment
}
