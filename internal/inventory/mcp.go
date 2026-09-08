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

// serverKey is one tool-name spelling under one harness. Both halves are needed:
// the roll-up row this index feeds is per harness, so a server name one harness
// discovered accounts for nothing observed under another. Keyed on the spelling
// alone, a segment would resolve as discovered because some other harness happened
// to configure that name, and the calls would be neither rolled up nor flagged —
// they would simply vanish, which is DG-99's own bug from the other side.
type serverKey struct {
	harness  record.Identifier
	spelling string
}

// serverIndex is the match from an observed tool-name segment to the discovered
// server it belongs to, plus the set of servers discovery actually found. Both are
// keyed per harness.
//
// A spelling two distinct config keys of one harness produce is dropped from the
// index rather than resolved to one of them: an ambiguous match is no match, so
// neither key absorbs the other's counters (fail closed, plan §3.4) — the same
// refusal canonicalNames makes when two sources contribute one bare skill
// name. A key that is already its own spelling is unaffected: nothing about it is
// ambiguous, and a segment equal to it names it and no one else.
func serverIndex(available []Primitive) (map[serverKey]record.Identifier, map[identity]struct{}) {
	index := map[serverKey]record.Identifier{}
	ambiguous := map[serverKey]struct{}{}
	discovered := map[identity]struct{}{}
	for _, primitive := range available {
		if primitive.Kind != record.KindMCPServer {
			continue
		}
		discovered[identity{harness: primitive.Harness, kind: record.KindMCPServer, name: primitive.Name}] = struct{}{}
		key := serverKey{harness: primitive.Harness, spelling: serverSpelling(string(primitive.Name))}
		if held, seen := index[key]; seen && held != primitive.Name {
			ambiguous[key] = struct{}{}
			continue
		}
		index[key] = primitive.Name
	}
	for key := range ambiguous {
		delete(index, key)
	}
	return index, discovered
}

// serverName resolves a segment observed under one harness onto the server that
// harness discovered under it, or keeps the observed segment itself when nothing
// matches. The row that results then carries the segment the harness really used,
// which is the only honest name for a server nothing in the inventory accounts for.
func serverName(index map[serverKey]record.Identifier, harness, segment record.Identifier) record.Identifier {
	if name, matched := index[serverKey{harness: harness, spelling: string(segment)}]; matched {
		return name
	}
	return segment
}
