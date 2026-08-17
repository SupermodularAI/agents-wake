package inventory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// names keys the scope digest for this package's tests, standing in for the
// subkey config.Repos.NameKey derives in production.
var names = record.NewNamer([]byte("test scope key"))

func TestClaudeCodeInScopeDiscoversProjectPrimitivesWhenConsented(t *testing.T) {
	claudeDir, root := discoveryFixture(t)

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)
	want := map[primitiveKey]bool{
		{kind: record.KindSkill, name: "global-skill"}:        true,
		{kind: record.KindSkill, name: "global"}:              true,
		{kind: record.KindSkill, name: "plugin:plugin-skill"}: true,
		{kind: record.KindSubagent, name: "repo-agent"}:       true,
		{kind: record.KindCommand, name: "release"}:           true,
		{kind: record.KindMCPServer, name: "listed-mcp"}:      true,
		{kind: record.KindMCPServer, name: "repo-mcp"}:        true,
	}
	for _, item := range got {
		delete(want, primitiveKey{kind: item.Kind, name: item.Name})
	}
	if len(want) != 0 {
		t.Fatalf("missing primitives: %+v; got %+v", want, got)
	}
}

func TestClaudeCodeInScopeWithholdsProjectPrimitivesWhenUnconsented(t *testing.T) {
	claudeDir, _ := discoveryFixture(t)

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Project: ProjectUnconsented}, names)
	assertOnlyGlobalDiscovery(t, got)
}

func TestClaudeCodeInScopeIgnoresARootItWasNotConsentedFor(t *testing.T) {
	claudeDir, root := discoveryFixture(t)

	for _, project := range []ProjectScope{ProjectUnconsented, ProjectUnresolved} {
		got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: project}, names)
		assertOnlyGlobalDiscovery(t, got)
	}
}

func TestClaudeCodeInScopeSkipsInvalidPrimitiveNames(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	root := t.TempDir()
	write(t, filepath.Join(claudeDir, "projects", "session.jsonl"), `{"cwd":"`+root+`","attachment":{"type":"skill_listing","content":"- contains space: description\n- ../escape: description\n- /etc/passwd: description"}}`)

	if got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names); len(got) != 0 {
		t.Fatalf("ClaudeCodeInScope() = %+v", got)
	}
}

// discoveryFixture writes one global Claude directory and one project directory,
// each holding primitives only its own discovery path can reach.
func discoveryFixture(t *testing.T) (claudeDir, root string) {
	t.Helper()
	claudeDir = filepath.Join(t.TempDir(), ".claude")
	root = t.TempDir()
	write(t, filepath.Join(claudeDir, "skills", "global-skill", "SKILL.md"), "# global")
	write(t, filepath.Join(claudeDir, "projects", "session.jsonl"), `{"cwd":"`+root+`","attachment":{"type":"skill_listing","content":"- global: description\n- plugin:plugin-skill: description"}}
{"cwd":"`+root+`","attachment":{"type":"agent_listing_delta","addedTypes":["repo-agent"]}}
{"cwd":"`+root+`","attachment":{"type":"mcp_instructions_delta","addedNames":["listed-mcp"]}}
{"cwd":"/other","attachment":{"type":"skill_listing","content":"- ignored: description"}}`)
	write(t, filepath.Join(root, ".claude", "commands", "release.md"), "# ignored")
	write(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"repo-mcp":{"command":"secret"}}}`)
	return claudeDir, root
}

// assertOnlyGlobalDiscovery pins that the global skill survived and that every
// primitive reachable only through the project directory or its session listings
// was withheld.
func assertOnlyGlobalDiscovery(t *testing.T, got []Primitive) {
	t.Helper()
	found := map[primitiveKey]bool{}
	for _, item := range got {
		found[primitiveKey{kind: item.Kind, name: item.Name}] = true
	}
	if !found[primitiveKey{kind: record.KindSkill, name: "global-skill"}] {
		t.Fatalf("global discovery did not run: %+v", got)
	}
	withheld := []primitiveKey{
		{kind: record.KindCommand, name: "release"},
		{kind: record.KindMCPServer, name: "repo-mcp"},
		{kind: record.KindMCPServer, name: "listed-mcp"},
		{kind: record.KindSkill, name: "global"},
		{kind: record.KindSkill, name: "plugin:plugin-skill"},
		{kind: record.KindSubagent, name: "repo-agent"},
	}
	for _, key := range withheld {
		if found[key] {
			t.Fatalf("project-local primitive %+v was discovered without consent: %+v", key, got)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
