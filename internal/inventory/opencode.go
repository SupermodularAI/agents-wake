package inventory

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// openCode is the harness slug every primitive this file discovers carries.
const openCode = record.Identifier("opencode")

// openCodeCommandDirName is opencode's own name for the directory holding one
// file per skill — "command" in its layout, "skill" in the tool spelling
// state.go's derivation reads (internal/adapter/opencode, DG-121). It sits
// beside the configuration file, never somewhere this build has to guess at,
// which is why it is derived from configFile's own directory rather than
// resolved independently.
const openCodeCommandDirName = "command"

// OpenCodeInScope discovers what opencode itself declares: the MCP servers
// named in its configuration file, and the skills named by one file each in
// the command directory beside it (ADR-0041: a harness's own declaration fixes
// a primitive's kind; a directory scan never does — and a harness's own single,
// unambiguous directory for one kind is exactly such a declaration, the same
// reading claudeCodeGlobal already gives Claude Code's own skills and agents
// directories).
//
// Subagents are deliberately not discovered here. opencode names two of them,
// "general" and "explore", in real activity, but declares neither in a file,
// a directory, or its configuration — nothing this build can read states the
// set. Naming them from this file anyway would be inventing a declaration
// nothing made (ADR-0041); real subagent activity is not lost collection for
// it, because derive's discoveredKinds rule (internal/inventory/state.go)
// publishes an observed subagent invocation flagged Unmatched wherever a
// harness's discovery says nothing about the kind at all, exactly this case.
//
// An absent or unreadable configuration file no longer ends discovery outright:
// the command directory is checked independently, because a minimal opencode
// install can carry skills with no MCP configuration at all. Observed is true
// the moment either source was actually read, whatever either one contained —
// an empty command directory is a real declaration of zero skills, not a
// failure to read one. Observed stays false only when neither could be read at
// all, which is "not observed" and never "zero" (ADR-0046).
func OpenCodeInScope(configFile string, names record.Namer) Discovery {
	items := map[primitiveKey]Primitive{}
	observed := false

	if configFile != "" {
		if source, err := os.ReadFile(configFile); err == nil {
			// The file is named .jsonc and may legally carry comments, so a strict
			// decode of it would silently report "no servers" on a commented config.
			var document struct {
				MCP map[string]json.RawMessage `json:"mcp"`
			}
			// Fail closed: a file this build cannot parse is one it did not observe,
			// never a partial guess at what it might have said.
			if json.Unmarshal(stripJSONComments(source), &document) == nil {
				observed = true
				for name := range document.MCP {
					// A key the name grammar refuses is dropped rather than repaired.
					// A server whose object sets "enabled": false is still listed: it
					// is declared, and whether it ran is the invocation grain's
					// question.
					identifier, err := names.DerivedName(name)
					if err != nil {
						continue
					}
					items[primitiveKey{kind: record.KindMCPServer, name: identifier}] = Primitive{
						Harness: openCode, Kind: record.KindMCPServer, Name: identifier,
					}
				}
			}
		}
		commandDir := filepath.Join(filepath.Dir(configFile), openCodeCommandDirName)
		if scanPrimitives(commandDir, "", record.KindSkill, func(kind record.Kind, name string) {
			identifier, err := names.DerivedName(name)
			if err != nil {
				return
			}
			items[primitiveKey{kind: kind, name: identifier}] = Primitive{Harness: openCode, Kind: kind, Name: identifier}
		}) {
			observed = true
		}
	}

	if !observed {
		return Discovery{Observed: []HarnessObservation{{Harness: openCode, Observed: false}}}
	}
	return Discovery{
		Primitives: sortedPrimitives(items),
		Observed:   []HarnessObservation{{Harness: openCode, Observed: true}},
	}
}
