package inventory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

func TestClaudeCodeDiscoversSessionListedAndConfiguredPrimitives(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	write(t, filepath.Join(home, ".claude", "projects", "session.jsonl"), `{"cwd":"`+root+`","attachment":{"type":"skill_listing","content":"- global: description\n- plugin:plugin-skill: description"}}
{"cwd":"`+root+`","attachment":{"type":"agent_listing_delta","addedTypes":["repo-agent"]}}
{"cwd":"`+root+`","attachment":{"type":"mcp_instructions_delta","addedNames":["listed-mcp"]}}
{"cwd":"/other","attachment":{"type":"skill_listing","content":"- ignored: description"}}`)
	write(t, filepath.Join(root, ".claude", "commands", "release.md"), "# ignored")
	write(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"repo-mcp":{"command":"secret"}}}`)

	got := ClaudeCode(home, root)
	want := map[primitiveKey]bool{
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

func TestClaudeCodeSkipsInvalidPrimitiveNames(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	write(t, filepath.Join(home, ".claude", "projects", "session.jsonl"), `{"cwd":"`+root+`","attachment":{"type":"skill_listing","content":"- contains space: description"}}`)
	if got := ClaudeCode(home, root); len(got) != 0 {
		t.Fatalf("ClaudeCode() = %+v", got)
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
