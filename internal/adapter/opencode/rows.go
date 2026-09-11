package opencode

import (
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// ToolPart is one row of opencode's `part` table whose data.type is "tool" — the
// canonical source event for an opencode invocation.
//
// It is the allowlist one layer before record.Record: state.input, state.output,
// state.title, state.error, state.raw and both metadata objects have no field
// here, so no code path can carry one into a record by accident (ADR-0007). The
// query that fills it selects exactly these columns for the same reason — free
// text never enters the process at all.
type ToolPart struct {
	// ID is part.id, the row's primary key and the value event_id derives from.
	// Never callID: measured against a real store, callID is not unique (3727
	// distinct values over 3756 tool parts), and an id two invocations share is an
	// invocation the store would silently lose (ADR-0004).
	ID        string
	SessionID string
	// Tool is data.tool, the observed spelling exactly as opencode wrote it.
	Tool string
	// Status is data.state.status, mapped by outcome.go and never guessed at.
	Status string
	// StartMS and EndMS are data.state.time.start / .end, epoch milliseconds.
	// HasStart is false where the harness reported no start instant, and HasEnd
	// false where it reported no end. Neither absence is ever substituted for: a
	// part with no start has no instant to stamp a record with, and an epoch put
	// there would be a measurement nothing measured.
	StartMS  int64
	HasStart bool
	EndMS    int64
	HasEnd   bool
	// UpdatedMS is part.time_updated: the row's own liveness, never written to a
	// record. HasUpdated is false where the harness recorded none — modelled rather
	// than coalesced, on the same rule as the two instants above, so nothing that
	// later reads it is handed an epoch nothing recorded.
	UpdatedMS  int64
	HasUpdated bool
}

// Session is the allowlisted subset of one opencode `session` row: a directory the
// consent resolver consumes and never stores, a version, and five totals.
//
// session.cost is deliberately absent. record.Record has no field for it, adding
// one is a schema change, and nothing in this change needs it — so the column is
// not selected, not modelled, and not silently dropped either: it is named here
// as deferred.
type Session struct {
	ID               string
	Directory        string
	Version          string
	TokensInput      int64
	TokensOutput     int64
	TokensReasoning  int64
	TokensCacheRead  int64
	TokensCacheWrite int64
	// UpdatedMS is session.time_updated, the session's last activity. HasUpdated is
	// false where the harness reported none, and no value is substituted: it is
	// both the session grain's whole timestamp and the idleness comparison's whole
	// input, so an epoch put here would stamp a record nothing measured and call
	// every such session finished the moment it was read.
	UpdatedMS  int64
	HasUpdated bool
}

// serverSeparator is what opencode puts between an MCP server's spelling and the
// tool's own name. It is a single character that tool names also contain freely,
// which is exactly why the match below never runs backwards.
const serverSeparator = "_"

// Servers is the set of MCP server spellings this machine has configured for
// opencode, injected as data because derivation may not read the filesystem
// (ADR-0019 §1) and because ADR-0039's match runs forward only — from a configured
// server to the spelling a tool name would carry, never by parsing an observed
// name.
//
// The zero value matches nothing, which is what a caller that could not read the
// machine's configuration must collect: every tool as a builtin, rather than every
// underscore as a server.
type Servers struct {
	spellings map[string]record.Identifier
}

// NewServers builds the forward index from the configured server keys discovery
// found.
//
// Each key is sanitised the way a harness composes a tool name from it —
// everything outside [A-Za-z0-9_-] becomes "_" — which is the same map
// internal/inventory/mcp.go applies for Claude Code, stated once per package
// because the two share no code here.
//
// A spelling two different configured keys both produce is dropped: an ambiguous
// match is no match, so neither key absorbs the other's calls (fail closed). A
// spelling outside the token domain is dropped too — it is what gets persisted as
// record.MCPServer, and a value the record type would refuse must never be carried
// to the point of refusal.
func NewServers(configured []record.Identifier) Servers {
	spellings := map[string]record.Identifier{}
	claimed := map[string]record.Identifier{}
	ambiguous := map[string]struct{}{}
	for _, key := range configured {
		spelling := serverSpelling(string(key))
		token, err := record.BoundedToken(spelling)
		if err != nil {
			continue
		}
		// Keyed on the configured key rather than on the spelling: two keys
		// producing one spelling produce the same token too, so comparing tokens
		// would never see the collision it is there to catch.
		if held, seen := claimed[spelling]; seen && held != key {
			ambiguous[spelling] = struct{}{}
			continue
		}
		claimed[spelling] = key
		spellings[spelling] = token
	}
	for spelling := range ambiguous {
		delete(spellings, spelling)
	}
	return Servers{spellings: spellings}
}

// Match reports the configured server a tool name is prefixed by, if any. It
// answers "does any configured server, sanitised, prefix this name followed by a
// separator" — so apply_patch matches nothing, because no server is named "apply",
// while atlassian_search matches the configured "atlassian".
//
// The longest match wins, so a configured "foo" and a configured "foo_bar" cannot
// both claim "foo_bar_baz". Two spellings of equal length can never both prefix
// one name, so there is no tie to break.
//
// What it returns is the spelling, not the config key: that is what the tool name
// carried, it is what record.MCPServer is documented to hold, and a config key
// ("plugin:context7:context7") is not even in the token domain the field validates
// against.
func (s Servers) Match(tool string) (record.Identifier, bool) {
	var longest record.Identifier
	for spelling, server := range s.spellings {
		if !strings.HasPrefix(tool, spelling+serverSeparator) {
			continue
		}
		if len(server) > len(longest) {
			longest = server
		}
	}
	return longest, longest != ""
}

// serverSpelling maps a configured key onto the segment a tool name carries for
// it. Forward only: reading it backwards is ambiguous — "foo_bar" could be either
// spelling of itself — so the inverse is never attempted and a name this index
// does not hold is a builtin, never a guessed server (ADR-0008, plan §3.3).
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
