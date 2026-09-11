package inventory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// openCodeConfig writes one opencode configuration file and returns its path.
func openCodeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}
	return path
}

// openCodeNames is a Namer with a key, standing in for the one config derives.
var openCodeNames = record.NewNamer([]byte("test scope key"))

func serverNames(discovery Discovery) []string {
	names := []string{}
	for _, primitive := range discovery.Primitives {
		if primitive.Kind == record.KindMCPServer && primitive.Harness == openCode {
			names = append(names, string(primitive.Name))
		}
	}
	return names
}

func TestOpenCodeServersListsEveryConfiguredServer(t *testing.T) {
	discovery := OpenCodeServers(openCodeConfig(t, `{"$schema":"https://opencode.ai/config.json",
		"mcp":{"atlassian":{"type":"remote"},"notion":{"type":"remote"}}}`), openCodeNames)

	names := serverNames(discovery)
	if len(names) != 2 || names[0] != "atlassian" || names[1] != "notion" {
		t.Fatalf("servers = %v, want [atlassian notion]", names)
	}
	if len(discovery.Observed) != 1 || !discovery.Observed[0].Observed || discovery.Observed[0].Harness != openCode {
		t.Fatalf("observation = %+v, want opencode observed", discovery.Observed)
	}
}

func TestOpenCodeServersReadsACommentedConfig(t *testing.T) {
	// The file is named .jsonc. A strict decode of a commented one reports "no
	// servers", which is a wrong answer that looks like an absence.
	discovery := OpenCodeServers(openCodeConfig(t, `{
		// the schema this file follows
		"$schema": "https://opencode.ai/config.json", // note the // in the URL
		/* the servers */
		"mcp": {"atlassian": {"type":"remote"}, "notion": {"type":"remote"}}
	}`), openCodeNames)

	if names := serverNames(discovery); len(names) != 2 {
		t.Fatalf("servers = %v, want both", names)
	}
	if !discovery.Observed[0].Observed {
		t.Fatal("a commented config was reported as not observed")
	}
}

func TestOpenCodeServersIgnoresEverythingButMCP(t *testing.T) {
	discovery := OpenCodeServers(openCodeConfig(t, `{"$schema":"x","plugin":["a","b"],"mcp":{"atlassian":{}}}`), openCodeNames)
	if names := serverNames(discovery); len(names) != 1 || names[0] != "atlassian" {
		t.Fatalf("servers = %v, want [atlassian]", names)
	}
	if len(discovery.Primitives) != 1 {
		t.Fatalf("primitives = %v, want only the declared server: a plugin array is not a primitive kind this file declares", discovery.Primitives)
	}
}

func TestAnAbsentConfigIsNotObserved(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing.jsonc")} {
		discovery := OpenCodeServers(path, openCodeNames)
		if len(discovery.Primitives) != 0 {
			t.Errorf("primitives = %v, want none", discovery.Primitives)
		}
		if len(discovery.Observed) != 1 || discovery.Observed[0].Observed {
			t.Errorf("observation = %+v, want not observed", discovery.Observed)
		}
	}
}

func TestAMalformedConfigIsNotObserved(t *testing.T) {
	// Fail closed: a file this build cannot parse is one it did not observe, and
	// never a partial guess at what it might have said.
	discovery := OpenCodeServers(openCodeConfig(t, "{{{"), openCodeNames)
	if len(discovery.Primitives) != 0 || discovery.Observed[0].Observed {
		t.Fatalf("primitives = %v, observed = %t, want none and false", discovery.Primitives, discovery.Observed[0].Observed)
	}
}

func TestADisabledServerIsStillDeclared(t *testing.T) {
	// Declared is what fixes the kind (ADR-0041). Whether it ran is the invocation
	// grain's question, and answering it here would make an unused server vanish
	// from the one report that exists to name unused things.
	discovery := OpenCodeServers(openCodeConfig(t, `{"mcp":{"atlassian":{"enabled":false}}}`), openCodeNames)
	if names := serverNames(discovery); len(names) != 1 {
		t.Fatalf("servers = %v, want the disabled server listed", names)
	}
}

func TestAnUnnameableServerKeyIsDropped(t *testing.T) {
	discovery := OpenCodeServers(openCodeConfig(t, `{"mcp":{"../etc/passwd":{},"atlassian":{}}}`), openCodeNames)
	if names := serverNames(discovery); len(names) != 1 || names[0] != "atlassian" {
		t.Fatalf("servers = %v, want only [atlassian]", names)
	}
}

func TestMergeKeepsBothHarnesses(t *testing.T) {
	base := Discovery{
		Primitives:     []Primitive{{Harness: claudeCode, Kind: record.KindSkill, Name: "pr-review"}},
		ProjectScanned: true,
		Observed:       []HarnessObservation{{Harness: claudeCode, Observed: true}},
	}
	extra := Discovery{
		Primitives: []Primitive{{Harness: openCode, Kind: record.KindMCPServer, Name: "atlassian"}},
		Observed:   []HarnessObservation{{Harness: openCode, Observed: true}},
	}

	merged := Merge(base, extra)
	if len(merged.Primitives) != 2 {
		t.Fatalf("primitives = %v, want both harnesses", merged.Primitives)
	}
	if !merged.ProjectScanned {
		t.Error("ProjectScanned was lost")
	}
	if len(merged.Observed) != 2 {
		t.Fatalf("observations = %v, want one per harness", merged.Observed)
	}
}

func TestMergeNeverFoldsAcrossHarnesses(t *testing.T) {
	// One name, one kind, two harnesses. Folding them would credit one harness's
	// calls to the other's server, which is a wrong number rather than a missing
	// one.
	base := Discovery{Primitives: []Primitive{{Harness: claudeCode, Kind: record.KindMCPServer, Name: "atlassian"}}}
	extra := Discovery{Primitives: []Primitive{{Harness: openCode, Kind: record.KindMCPServer, Name: "atlassian"}}}

	if merged := Merge(base, extra); len(merged.Primitives) != 2 {
		t.Fatalf("primitives = %v, want two rows", merged.Primitives)
	}
}

func TestMergeNeverWidensProjectScanned(t *testing.T) {
	base := Discovery{ProjectScanned: false}
	extra := Discovery{ProjectScanned: true}
	if Merge(base, extra).ProjectScanned {
		t.Fatal("a second harness flipped a Claude Code notion")
	}
}

func TestMergeIsOrderStable(t *testing.T) {
	base := Discovery{Primitives: []Primitive{
		{Harness: openCode, Kind: record.KindMCPServer, Name: "notion"},
		{Harness: claudeCode, Kind: record.KindSkill, Name: "pr-review"},
	}}
	extra := Discovery{Primitives: []Primitive{{Harness: openCode, Kind: record.KindMCPServer, Name: "atlassian"}}}

	first, second := Merge(base, extra), Merge(base, extra)
	for index := range first.Primitives {
		if first.Primitives[index] != second.Primitives[index] {
			t.Fatalf("merge order is not stable at %d", index)
		}
	}
	if first.Primitives[0].Harness != claudeCode {
		t.Fatalf("rows are not sorted by harness first: %v", first.Primitives)
	}
}
