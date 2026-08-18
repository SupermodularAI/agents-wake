// Package inventory discovers locally available primitives without retaining
// configuration contents, paths, or other free text.
package inventory

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/jsonl"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

const claudeCode = record.Identifier("claude-code")

// maxListingLineBytes is the largest session line this discovery pass reads. It
// stays an internal constant, not a config key: ADR-0014 keeps the config surface
// deliberately small, and a limit a user can raise is a limit that stops bounding
// anything.
const maxListingLineBytes = 2 * 1024 * 1024

// Primitive is a locally available harness primitive. Discovery never persists
// it; it is merged with the event store only while rendering the dashboard.
type Primitive struct {
	Harness record.Identifier
	Kind    record.Kind
	Name    record.Identifier
}

// ClaudeCodeInScope discovers the primitives one invocation is allowed to see.
//
// It is the only entry point: there is deliberately no overload taking a bare
// root, so no code path can scan a project directory without first stating a
// consent answer for it — the same "the type is the boundary" property ADR-0007
// gives the record.
//
// It prefers primitives Claude Code listed in sessions for the consented root,
// then supplements them from configured directories because Claude Code does not
// emit a listing for every primitive kind.
//
// Global discovery and project-local discovery are separate calls, and the second
// runs only for a consented working directory: a value read out of an
// unconsented project must never reach the persisted inventory (ADR-0010,
// ADR-0019 §2). Unreadable or malformed sources contribute nothing, so a
// configuration problem cannot break the dashboard.
//
// names keys the digest that stands in for a directory-scoped primitive's scope,
// which a session listing states as a path prefix (ADR-0020).
func ClaudeCodeInScope(scope Scope, names record.Namer) Discovery {
	items := map[primitiveKey]Primitive{}
	add := func(kind record.Kind, name string) {
		identifier, err := names.DerivedName(name)
		if err != nil {
			return
		}
		items[primitiveKey{kind: kind, name: identifier}] = Primitive{Harness: claudeCode, Kind: kind, Name: identifier}
	}

	claudeCodeGlobal(scope.ClaudeDir, add)
	scanned := scope.allowsProject()
	if scanned {
		claudeCodeProject(scope.ClaudeDir, scope.Root, add)
	}
	return Discovery{Primitives: sortedPrimitives(items), ProjectScanned: scanned}
}

// claudeCodeGlobal scans the harness's own directory and its installed plugins.
// It never reads a working directory, so it needs no consent answer.
func claudeCodeGlobal(claudeDir string, add func(record.Kind, string)) {
	scanPrimitives(filepath.Join(claudeDir, "skills"), "SKILL.md", record.KindSkill, add)
	scanPrimitives(filepath.Join(claudeDir, "agents"), "", record.KindSubagent, add)
	scanPrimitives(filepath.Join(claudeDir, "commands"), "", record.KindCommand, add)
	for _, installPath := range installedPluginPaths(filepath.Join(claudeDir, "plugins", "installed_plugins.json")) {
		scanPrimitives(filepath.Join(installPath, "skills"), "SKILL.md", record.KindSkill, add)
		scanPrimitives(filepath.Join(installPath, "agents"), "", record.KindSubagent, add)
		scanPrimitives(filepath.Join(installPath, "commands"), "", record.KindCommand, add)
		scanMCP(filepath.Join(installPath, ".mcp.json"), add)
	}
	scanMCP(filepath.Join(claudeDir, "settings.json"), add)
}

// claudeCodeProject scans one consented working directory. Every source it reads
// belongs to that directory — including the harness's session listings, which are
// filtered to it — so the caller must have resolved consent first.
func claudeCodeProject(claudeDir, root string, add func(record.Kind, string)) {
	scanListings(filepath.Join(claudeDir, "projects"), root, add)
	scanPrimitives(filepath.Join(root, ".claude", "skills"), "SKILL.md", record.KindSkill, add)
	scanPrimitives(filepath.Join(root, ".claude", "agents"), "", record.KindSubagent, add)
	scanPrimitives(filepath.Join(root, ".claude", "commands"), "", record.KindCommand, add)
	scanMCP(filepath.Join(root, ".claude", "settings.json"), add)
	scanMCP(filepath.Join(root, ".mcp.json"), add)
}

// sortedPrimitives returns the deduplicated items in a deterministic order.
func sortedPrimitives(items map[primitiveKey]Primitive) []Primitive {
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
	visit := func(_ int64, line []byte) {
		var entry struct {
			CWD        string `json:"cwd"`
			Attachment struct {
				Type       string   `json:"type"`
				Content    string   `json:"content"`
				AddedNames []string `json:"addedNames"`
				AddedTypes []string `json:"addedTypes"`
			} `json:"attachment"`
		}
		if json.Unmarshal(line, &entry) != nil || !underRoot(entry.CWD, root) {
			return
		}
		switch entry.Attachment.Type {
		case "skill_listing":
			for _, listed := range strings.Split(entry.Attachment.Content, "\n") {
				name, found := strings.CutPrefix(listed, "- ")
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
	// "Could not read" means "collects nothing", never an error that breaks a
	// command: this listing contributes what it already produced. The count of
	// discarded lines is deliberately dropped rather than reported — a per-read
	// health counter must not enter the snapshot, whose rows are one per change of
	// state (ADR-0014), and surfacing partial discovery is a later phase's scope.
	// The line's offset is discarded with it: the discovery snapshot has no cursor to
	// floor (ADR-0014's rows are one per change of state, not per read).
	if _, err := jsonl.Lines(reader, maxListingLineBytes, visit); err != nil {
		return
	}
}

// underRoot is a subtree filter inside an already-consented project, not a
// consent check: consent is resolved by config.Repos.Identify before this
// package is called (ADR-0019 §1).
func underRoot(cwd, root string) bool {
	return cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator))
}
