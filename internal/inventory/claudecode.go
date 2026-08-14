// Package inventory discovers locally available primitives without retaining
// configuration contents, paths, or other free text.
package inventory

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

const claudeCode = record.Identifier("claude-code")

// Primitive is a locally available harness primitive. Discovery never persists
// it; it is merged with the event store only while rendering the dashboard.
type Primitive struct {
	Harness record.Identifier
	Kind    record.Kind
	Name    record.Identifier
}

// ClaudeCode prefers primitives Claude Code listed in sessions for root, then
// supplements them from configured directories because Claude Code does not
// emit a listing for every primitive kind. Unreadable or malformed sources
// contribute nothing so a configuration problem cannot break the dashboard.
func ClaudeCode(home, root string) []Primitive {
	return ClaudeCodeAt(filepath.Join(home, ".claude"), root)
}

// ClaudeCodeAt discovers the primitives configured under one Claude Code
// directory. It exists for Wake's hook path, whose configured Claude directory
// may differ from the invoking user's home directory in tests or embedding.
func ClaudeCodeAt(claudeDir, root string) []Primitive {
	items := map[primitiveKey]Primitive{}
	add := func(kind record.Kind, name string) {
		identifier, err := record.BoundedIdentifier(name)
		if err != nil {
			return
		}
		item := Primitive{Harness: claudeCode, Kind: kind, Name: identifier}
		items[primitiveKey{kind: kind, name: identifier}] = item
	}

	scanListings(filepath.Join(claudeDir, "projects"), root, add)
	for _, base := range []string{claudeDir, filepath.Join(root, ".claude")} {
		scanPrimitives(filepath.Join(base, "skills"), "SKILL.md", record.KindSkill, add)
		scanPrimitives(filepath.Join(base, "agents"), "", record.KindSubagent, add)
		scanPrimitives(filepath.Join(base, "commands"), "", record.KindCommand, add)
	}
	for _, installPath := range installedPluginPaths(filepath.Join(claudeDir, "plugins", "installed_plugins.json")) {
		scanPrimitives(filepath.Join(installPath, "skills"), "SKILL.md", record.KindSkill, add)
		scanPrimitives(filepath.Join(installPath, "agents"), "", record.KindSubagent, add)
		scanPrimitives(filepath.Join(installPath, "commands"), "", record.KindCommand, add)
		scanMCP(filepath.Join(installPath, ".mcp.json"), add)
	}
	for _, settings := range []string{filepath.Join(claudeDir, "settings.json"), filepath.Join(root, ".claude", "settings.json"), filepath.Join(root, ".mcp.json")} {
		scanMCP(settings, add)
	}

	result := make([]Primitive, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	slices.SortFunc(result, func(left, right Primitive) int {
		return strings.Compare(string(left.Kind)+":"+string(left.Name), string(right.Kind)+":"+string(right.Name))
	})
	return result
}

type primitiveKey struct {
	kind record.Kind
	name record.Identifier
}

func scanPrimitives(path, exactName string, kind record.Kind, add func(record.Kind, string)) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if exactName != "" {
			if entry.IsDir() {
				if _, err := os.Stat(filepath.Join(resolved, entry.Name(), exactName)); err == nil {
					add(kind, entry.Name())
				}
			}
			continue
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".md" {
			add(kind, strings.TrimSuffix(entry.Name(), ".md"))
		}
	}
}

func installedPluginPaths(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var document struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(data, &document) != nil {
		return nil
	}
	paths := make([]string, 0)
	for _, versions := range document.Plugins {
		for _, plugin := range versions {
			if plugin.InstallPath != "" {
				paths = append(paths, plugin.InstallPath)
			}
		}
	}
	return paths
}

func scanMCP(path string, add func(record.Kind, string)) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var document struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &document) != nil {
		return
	}
	for name := range document.MCPServers {
		add(record.KindMCPServer, name)
	}
}

func scanListings(path, root string, add func(record.Kind, string)) {
	if err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(current) != ".jsonl" {
			return nil
		}
		file, err := os.Open(current)
		if err != nil {
			return nil
		}
		defer file.Close()
		readListings(file, root, add)
		return nil
	}); err != nil {
		return
	}
}

func readListings(reader io.Reader, root string, add func(record.Kind, string)) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		var entry struct {
			CWD        string `json:"cwd"`
			Attachment struct {
				Type       string   `json:"type"`
				Content    string   `json:"content"`
				AddedNames []string `json:"addedNames"`
				AddedTypes []string `json:"addedTypes"`
			} `json:"attachment"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || !underRoot(entry.CWD, root) {
			continue
		}
		switch entry.Attachment.Type {
		case "skill_listing":
			for _, line := range strings.Split(entry.Attachment.Content, "\n") {
				name, found := strings.CutPrefix(line, "- ")
				if !found {
					continue
				}
				name, _, found = strings.Cut(name, ": ")
				if found {
					add(record.KindSkill, name)
				}
			}
		case "agent_listing_delta":
			for _, name := range entry.Attachment.AddedTypes {
				add(record.KindSubagent, name)
			}
		case "mcp_instructions_delta":
			for _, name := range entry.Attachment.AddedNames {
				add(record.KindMCPServer, name)
			}
		}
	}
}

func underRoot(cwd, root string) bool {
	return cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator))
}
