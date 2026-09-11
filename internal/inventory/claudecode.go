// Package inventory discovers locally available primitives without retaining
// configuration contents, paths, or other free text.
package inventory

import (
	"cmp"
	"encoding/json"
	"io"
	"maps"
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
//
// It also reports which bare plugin-primitive names are the same primitive as their
// namespaced spelling, so the inventory join can render one row for one primitive
// without the collection side losing either spelling (ADR-0020). Where a session
// listing declared the kind the harness invokes that spelling under, the fold carries
// that kind too (ADR-0041).
func ClaudeCodeInScope(scope Scope, names record.Namer) Discovery {
	items := map[primitiveKey]Primitive{}
	origins := newPrimitiveOrigins()
	add := func(kind record.Kind, name string) {
		identifier, err := names.DerivedName(name)
		if err != nil {
			return
		}
		items[primitiveKey{kind: kind, name: identifier}] = Primitive{Harness: claudeCode, Kind: kind, Name: identifier}
	}

	claudeCodeGlobal(scope.ClaudeDir, add, origins)
	scanned := scope.allowsProject()
	if scanned {
		claudeCodeProject(scope.ClaudeDir, scope.Root, add, origins)
	}
	return Discovery{Primitives: sortedPrimitives(items), ProjectScanned: scanned, canonical: origins.canonicalNames(names, items)}
}

// ClaudeCodeAcrossRepos discovers global primitives once, then project-local
// primitives for every root given, merging everything into one Discovery.
//
// ClaudeCodeInScope can only ever see one consented root per call, which is
// right for a single command run inside a repository but wrong for a
// machine-wide surface: report and serve are meant to show every repo `wake
// init` has registered on this machine, not only the one the command happens to
// run in (plan §8, "served dashboard, navigation and filters"). ProjectScanned
// is unconditionally true here because every root offered is already consented
// — there is no partial-scan case to carry a previous snapshot forward from,
// the way a single unconsented cwd has.
func ClaudeCodeAcrossRepos(claudeDir string, roots []string, names record.Namer) Discovery {
	items := map[primitiveKey]Primitive{}
	add := func(kind record.Kind, name string) {
		identifier, err := names.DerivedName(name)
		if err != nil {
			return
		}
		items[primitiveKey{kind: kind, name: identifier}] = Primitive{Harness: claudeCode, Kind: kind, Name: identifier}
	}
	origins := newPrimitiveOrigins()
	claudeCodeGlobal(claudeDir, add, origins)
	for _, root := range roots {
		claudeCodeProject(claudeDir, root, add, origins)
	}
	return Discovery{Primitives: sortedPrimitives(items), ProjectScanned: true, canonical: origins.canonicalNames(names, items)}
}

// claudeCodeGlobal scans the harness's own directory and its installed plugins.
// It never reads a working directory, so it needs no consent answer.
func claudeCodeGlobal(claudeDir string, add func(record.Kind, string), origins *primitiveOrigins) {
	scanPrimitives(filepath.Join(claudeDir, "skills"), "SKILL.md", record.KindSkill, origins.fromElsewhere(add))
	scanPrimitives(filepath.Join(claudeDir, "agents"), "", record.KindSubagent, add)
	scanPrimitives(filepath.Join(claudeDir, "commands"), "", record.KindCommand, origins.fromElsewhere(add))
	for _, plugin := range installedPlugins(filepath.Join(claudeDir, "plugins", "installed_plugins.json")) {
		scanPrimitives(filepath.Join(plugin.installPath, "skills"), "SKILL.md", record.KindSkill, origins.fromPlugin(plugin.namespace, add))
		scanPrimitives(filepath.Join(plugin.installPath, "agents"), "", record.KindSubagent, add)
		scanPrimitives(filepath.Join(plugin.installPath, "commands"), "", record.KindCommand, origins.fromPlugin(plugin.namespace, add))
		scanMCP(filepath.Join(plugin.installPath, ".mcp.json"), add)
	}
	scanMCP(filepath.Join(claudeDir, "settings.json"), add)
}

// claudeCodeProject scans one consented working directory. Every source it reads
// belongs to that directory — including the harness's session listings, which are
// filtered to it — so the caller must have resolved consent first.
func claudeCodeProject(claudeDir, root string, add func(record.Kind, string), origins *primitiveOrigins) {
	scanListings(filepath.Join(claudeDir, "projects"), root, origins.fromElsewhere(add), origins.declaredSkill)
	scanPrimitives(filepath.Join(root, ".claude", "skills"), "SKILL.md", record.KindSkill, origins.fromElsewhere(add))
	scanPrimitives(filepath.Join(root, ".claude", "agents"), "", record.KindSubagent, add)
	scanPrimitives(filepath.Join(root, ".claude", "commands"), "", record.KindCommand, origins.fromElsewhere(add))
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

// pluginInstall is one installed plugin version as installed_plugins.json states
// it: the namespace Claude Code invokes its primitives under, and the directory
// they live in.
//
// The namespace used to be discarded. That is why one plugin skill was discovered
// twice — once bare from <installPath>/skills, once namespaced from a session
// listing — and the bare row could never be reached by an event, which reported a
// skill used daily as unused (DG-106).
type pluginInstall struct {
	namespace   string
	installPath string
}

// installedPlugins reads installed_plugins.json and returns one entry per
// installed version.
//
// The map key is "<plugin>@<marketplace>" and only the plugin half is the
// namespace a primitive is invoked under ("superpowers:brainstorming"), so the
// marketplace qualifier is cut here; a key carrying no "@" is its own namespace.
// Keeping the whole key would compose a name nothing ever invokes, which is a
// worse failure than the one this fixes — the phantom row would be renamed rather
// than removed.
//
// The result is sorted because Go walks a map in a random order and discovery must
// not depend on the order it saw plugins in (ADR-0004).
func installedPlugins(path string) []pluginInstall {
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
	installs := make([]pluginInstall, 0, len(document.Plugins))
	for key, versions := range document.Plugins {
		namespace, _, _ := strings.Cut(key, "@")
		for _, plugin := range versions {
			if plugin.InstallPath == "" {
				continue
			}
			installs = append(installs, pluginInstall{namespace: namespace, installPath: plugin.InstallPath})
		}
	}
	slices.SortFunc(installs, func(left, right pluginInstall) int {
		return cmp.Or(strings.Compare(left.namespace, right.namespace), strings.Compare(left.installPath, right.installPath))
	})
	return installs
}

// originKey is one bare name under one kind. Provenance is per kind because a
// primitive's kind is part of its identity: ~/.claude/commands/deploy.md and a
// plugin's own skills/deploy/ are two primitives, and proving one says nothing
// about the other.
type originKey struct {
	kind record.Kind
	name string
}

// primitiveOrigins records which source contributed each bare name, so the
// inventory join can tell a plugin's own primitive from a name that merely looks
// like one. Provenance is the whole proof: ADR-0020 refuses to fold two spellings
// onto one name unless they are provably the same primitive, because a wrong fold
// makes one primitive's counters absorb another's.
//
// Every kind a plugin ships is tracked, not skills alone: a command shipped by a
// plugin needs the same proof a skill does, because the fold ADR-0041 licenses
// crosses a kind boundary and a wrong one merges two primitives' counters
// (ADR-0020).
type primitiveOrigins struct {
	plugins  map[originKey]map[string]struct{} // bare name+kind -> namespaces that contributed it
	others   map[originKey]struct{}            // bare name+kind some non-plugin source contributed
	declared map[string]record.Kind            // name a session skill_listing carried -> the kind it declared
}

func newPrimitiveOrigins() *primitiveOrigins {
	return &primitiveOrigins{
		plugins:  map[originKey]map[string]struct{}{},
		others:   map[originKey]struct{}{},
		declared: map[string]record.Kind{},
	}
}

// fromPlugin wraps add so every name passing through it is recorded as contributed
// by namespace.
func (o *primitiveOrigins) fromPlugin(namespace string, add func(record.Kind, string)) func(record.Kind, string) {
	return func(kind record.Kind, name string) {
		key := originKey{kind: kind, name: name}
		namespaces, seen := o.plugins[key]
		if !seen {
			namespaces = map[string]struct{}{}
			o.plugins[key] = namespaces
		}
		namespaces[namespace] = struct{}{}
		add(kind, name)
	}
}

// fromElsewhere wraps add so every name passing through it is recorded as
// contributed by something that is not a plugin's own directory — the user's
// ~/.claude, a project's .claude, or a session listing. A bare name two sources
// contribute is not provably one primitive, so it is never folded.
func (o *primitiveOrigins) fromElsewhere(add func(record.Kind, string)) func(record.Kind, string) {
	return func(kind record.Kind, name string) {
		o.others[originKey{kind: kind, name: name}] = struct{}{}
		add(kind, name)
	}
}

// declaredSkill records that a session skill_listing carried name. A listing is a
// source that says: it states the kind Claude Code will invoke that name under, and
// that statement — not the directory a file happened to sit in — is what fixes the
// kind the inventory records (ADR-0041, ADR-0005). The kind is record.KindSkill by
// construction: it is the only kind this attachment declares.
//
// The name is trimmed because a listing line may carry a trailing \r, while the
// composed candidate canonicalNames looks up is built from validated tokens and
// never does.
func (o *primitiveOrigins) declaredSkill(name string) {
	o.declared[strings.TrimSpace(name)] = record.KindSkill
}

// canonicalNames returns the fold from a plugin primitive's bare directory name
// onto the namespaced spelling every invocation of it carries.
//
// A name folds only where discovery proved it: exactly one plugin contributed it and
// no other source did, the namespace is a plain token, the composed name is one
// record.Namer will persist — which refuses the scope-<digest>: shape ADR-0020
// reserves for a directory scope — and no primitive of the wrong kind already holds
// that name. It folds only where the kind is one the harness declared or one it never
// changed (foldKind). Every other case is refused rather than resolved. Refusing
// leaves one pre-existing phantom row; folding wrongly merges two primitives'
// counters, which is the metric corruption ADR-0020 exists to prevent.
//
// The result is a function of the discovered set, never of the order it was walked
// in (ADR-0004).
func (o *primitiveOrigins) canonicalNames(names record.Namer, discovered map[primitiveKey]Primitive) map[identity]identity {
	canonical := map[identity]identity{}
	for key, contributors := range o.plugins {
		if _, elsewhere := o.others[key]; elsewhere {
			continue
		}
		namespaces := slices.Sorted(maps.Keys(contributors))
		if len(namespaces) != 1 || strings.ContainsAny(key.name, ":/") {
			continue
		}
		if _, err := record.BoundedToken(namespaces[0]); err != nil {
			continue
		}
		composed := namespaces[0] + ":" + key.name
		kind, licensed := o.foldKind(key.kind, composed)
		if !licensed {
			continue
		}
		from, err := names.DerivedName(key.name)
		if err != nil {
			continue
		}
		to, err := names.DerivedName(composed)
		if err != nil {
			continue
		}
		if _, found := discovered[primitiveKey{kind: key.kind, name: from}]; !found {
			continue
		}
		if heldByAnotherKind(discovered, to, kind) {
			continue
		}
		canonical[identity{harness: claudeCode, kind: key.kind, name: from}] = identity{harness: claudeCode, kind: kind, name: to}
	}
	return canonical
}

// foldKind reports the kind a folded row carries, and whether the fold is licensed
// at all.
//
// Two cases, and the second is ADR-0041. Where a session skill_listing names the
// composed spelling, the harness has stated the kind it will invoke that primitive
// under; reading that statement is mapping the harness's own vocabulary, which
// ADR-0005 distinguishes from decoding a silence. Where no listing names it, only
// DG-106's same-kind fold is licensed: choosing a kind from the directory a file
// happened to sit in would be a record dimension decided by walk order, which
// ADR-0005 forbids and ADR-0004 rules out.
//
// The licence is a positive, closed set with the refusal as the default outside it,
// so a primitive nothing declares is exactly as it was before this rule existed.
func (o *primitiveOrigins) foldKind(discovered record.Kind, composed string) (record.Kind, bool) {
	if kind, stated := o.declared[composed]; stated {
		return kind, true
	}
	if discovered == record.KindSkill {
		return record.KindSkill, true
	}
	return "", false
}

// heldByAnotherKind reports whether the discovered set already holds name under a
// kind other than the one this fold would land it on. Landing on it would make one
// primitive's counters absorb another's, which is the merge ADR-0020 exists to
// prevent — and unlike the kind question ADR-0041 settles, nothing states which of
// the two the harness means, so it is refused rather than resolved.
func heldByAnotherKind(discovered map[primitiveKey]Primitive, name record.Identifier, kind record.Kind) bool {
	for key := range discovered {
		if key.name == name && key.kind != kind {
			return true
		}
	}
	return false
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

func scanListings(path, root string, add func(record.Kind, string), declare func(string)) {
	if err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(current) != ".jsonl" {
			return nil
		}
		file, err := os.Open(current)
		if err != nil {
			return nil
		}
		defer file.Close()
		readListings(file, root, add, declare)
		return nil
	}); err != nil {
		return
	}
}

func readListings(reader io.Reader, root string, add func(record.Kind, string), declare func(string)) {
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
					// The declaration, recorded only for a line that is a listing
					// entry: the harness has stated the kind it will invoke this
					// name under, and that statement is what fixes the kind of a
					// folded row (ADR-0041). agent_listing_delta and
					// mcp_instructions_delta declare nothing — the rule is scoped to
					// this attachment.
					declare(name)
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
