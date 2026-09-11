package inventory

import (
	"encoding/json"
	"os"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// openCode is the harness slug every primitive this file discovers carries.
const openCode = record.Identifier("opencode")

// OpenCodeServers discovers the MCP servers opencode itself declares, from
// opencode's own configuration file and nowhere else (ADR-0041: a harness's own
// declaration fixes a primitive's kind; a directory scan never does).
//
// So this reads exactly one file and lists exactly one kind. opencode's skills,
// subagents and commands are not discovered here, because this file does not
// declare them and inferring them from a directory layout would be the guess
// ADR-0041 forbids.
//
// An absent or unreadable file returns an empty Discovery with Observed false: a
// harness whose declaration this build cannot read declared nothing it can see,
// which is "not observed" and never "zero" (ADR-0046). configFile is "" on a
// machine that has no such file at all, which is the same answer by a shorter
// route.
func OpenCodeServers(configFile string, names record.Namer) Discovery {
	discovery := Discovery{Observed: []HarnessObservation{{Harness: openCode, Observed: false}}}
	if configFile == "" {
		return discovery
	}
	source, err := os.ReadFile(configFile)
	if err != nil {
		return discovery
	}
	// The file is named .jsonc and may legally carry comments, so a strict decode
	// of it would silently report "no servers" on a commented config.
	var document struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if json.Unmarshal(stripJSONComments(source), &document) != nil {
		// Fail closed: a file this build cannot parse is one it did not observe,
		// never a partial guess at what it might have said.
		return discovery
	}

	items := map[primitiveKey]Primitive{}
	for name := range document.MCP {
		// A key the name grammar refuses is dropped rather than repaired. A server
		// whose object sets "enabled": false is still listed: it is declared, and
		// whether it ran is the invocation grain's question.
		identifier, err := names.DerivedName(name)
		if err != nil {
			continue
		}
		items[primitiveKey{kind: record.KindMCPServer, name: identifier}] = Primitive{
			Harness: openCode, Kind: record.KindMCPServer, Name: identifier,
		}
	}
	return Discovery{
		Primitives: sortedPrimitives(items),
		Observed:   []HarnessObservation{{Harness: openCode, Observed: true}},
	}
}
