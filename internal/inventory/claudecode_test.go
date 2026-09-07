package inventory

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// names keys the scope digest for this package's tests, standing in for the
// subkey config.Repos.NameKey derives in production.
var names = record.NewNamer([]byte("test scope key"))

func TestClaudeCodeInScopeDiscoversProjectPrimitivesWhenConsented(t *testing.T) {
	claudeDir, root := discoveryFixture(t)

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)
	if !got.ProjectScanned {
		t.Fatal("a consented pass reported that it did not scan the project")
	}
	want := map[primitiveKey]bool{
		{kind: record.KindSkill, name: "global-skill"}:        true,
		{kind: record.KindSkill, name: "global"}:              true,
		{kind: record.KindSkill, name: "plugin:plugin-skill"}: true,
		{kind: record.KindSubagent, name: "repo-agent"}:       true,
		{kind: record.KindCommand, name: "release"}:           true,
		{kind: record.KindMCPServer, name: "listed-mcp"}:      true,
		{kind: record.KindMCPServer, name: "repo-mcp"}:        true,
	}
	for _, item := range got.Primitives {
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

func TestClaudeCodeAcrossReposMergesEveryRootsProjectPrimitivesWithGlobal(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	write(t, filepath.Join(claudeDir, "skills", "global-skill", "SKILL.md"), "# global")

	rootA := t.TempDir()
	write(t, filepath.Join(rootA, ".claude", "commands", "deploy-a.md"), "# a")
	rootB := t.TempDir()
	write(t, filepath.Join(rootB, ".claude", "commands", "deploy-b.md"), "# b")

	got := ClaudeCodeAcrossRepos(claudeDir, []string{rootA, rootB}, names)
	if !got.ProjectScanned {
		t.Fatal("a pass over consented roots reported that it did not scan the project")
	}
	found := map[primitiveKey]bool{}
	for _, item := range got.Primitives {
		found[primitiveKey{kind: item.Kind, name: item.Name}] = true
	}
	for _, want := range []primitiveKey{
		{kind: record.KindSkill, name: "global-skill"},
		{kind: record.KindCommand, name: "deploy-a"},
		{kind: record.KindCommand, name: "deploy-b"},
	} {
		if !found[want] {
			t.Fatalf("missing %+v in %+v", want, got)
		}
	}
}

func TestClaudeCodeInScopeSkipsInvalidPrimitiveNames(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	root := t.TempDir()
	write(t, filepath.Join(claudeDir, "projects", "session.jsonl"), `{"cwd":"`+root+`","attachment":{"type":"skill_listing","content":"- contains space: description\n- ../escape: description\n- /etc/passwd: description"}}`)

	if got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names); len(got.Primitives) != 0 {
		t.Fatalf("ClaudeCodeInScope() = %+v", got)
	}
}

func TestClaudeCodeInScopeKeepsListingsAroundAnOversizedLine(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	root := t.TempDir()
	oversized := `{"cwd":"` + root + `","attachment":{"type":"skill_listing","content":"- oversized-skill: ` + strings.Repeat("A", 2*1024*1024) + `"}}`
	write(t, filepath.Join(claudeDir, "projects", "session.jsonl"), strings.Join([]string{
		`{"cwd":"` + root + `","attachment":{"type":"skill_listing","content":"- before-skill: description"}}`,
		oversized,
		`{"cwd":"` + root + `","attachment":{"type":"agent_listing_delta","addedTypes":["after-agent"]}}`,
	}, "\n"))

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

	found := map[primitiveKey]bool{}
	for _, item := range got.Primitives {
		found[primitiveKey{kind: item.Kind, name: item.Name}] = true
	}
	if !found[primitiveKey{kind: record.KindSkill, name: "before-skill"}] {
		t.Errorf("the listing before the oversized line was lost: %+v", got)
	}
	if !found[primitiveKey{kind: record.KindSubagent, name: "after-agent"}] {
		t.Errorf("the listing after the oversized line was lost: %+v", got)
	}
	if found[primitiveKey{kind: record.KindSkill, name: "oversized-skill"}] {
		t.Errorf("the oversized line was parsed in part rather than discarded: %+v", got)
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

// assertOnlyGlobalDiscovery pins that the global skill survived, that every
// primitive reachable only through the project directory or its session listings
// was withheld, and that the pass reports itself as partial so the snapshot writer
// does not read the absences as deletions.
func assertOnlyGlobalDiscovery(t *testing.T, got Discovery) {
	t.Helper()
	if got.ProjectScanned {
		t.Fatalf("a withheld pass reported a scanned project: %+v", got)
	}
	found := map[primitiveKey]bool{}
	for _, item := range got.Primitives {
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

// The map key in installed_plugins.json is "<plugin>@<marketplace>" and only the
// plugin half is the namespace a primitive is invoked under. Keeping the whole key
// would compose a name nothing ever invokes, which renames the phantom row rather
// than removing it — a worse failure than the one DG-106 fixes.
func TestInstalledPluginsKeepsThePluginNamespaceFromTheMapKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installed_plugins.json")
	write(t, path, `{"version":1,"plugins":{
  "superpowers@claude-plugins-official":[{"scope":"user","installPath":"/p/superpowers/6.3.0","version":"6.3.0","installedAt":"2026-06-18T14:38:10.185Z"}],
  "vercel@claude-plugins-official":[{"scope":"user","installPath":"/p/vercel/1.1.0","version":"1.1.0"},{"scope":"user","installPath":"/p/vercel/1.0.0","version":"1.0.0"}],
  "local-plugin":[{"scope":"user","installPath":"/p/local"}],
  "empty@market":[{"scope":"user","installPath":""}]}}`)

	want := []pluginInstall{
		{namespace: "local-plugin", installPath: "/p/local"},
		{namespace: "superpowers", installPath: "/p/superpowers/6.3.0"},
		{namespace: "vercel", installPath: "/p/vercel/1.0.0"},
		{namespace: "vercel", installPath: "/p/vercel/1.1.0"},
	}
	if got := installedPlugins(path); !slices.Equal(got, want) {
		t.Fatalf("installedPlugins() = %+v, want %+v", got, want)
	}
}

// "Could not read" means "collects nothing", never an error that breaks a command
// (plan §4.3).
func TestInstalledPluginsCollectsNothingFromAnUnreadableFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	if got := installedPlugins(missing); got != nil {
		t.Fatalf("installedPlugins(missing) = %+v, want nil", got)
	}
	malformed := filepath.Join(t.TempDir(), "installed_plugins.json")
	write(t, malformed, "not json")
	if got := installedPlugins(malformed); got != nil {
		t.Fatalf("installedPlugins(malformed) = %+v, want nil", got)
	}
}

// Discovery keeps both spellings on purpose. This slice is what
// activation.installedFrom converts for claudecode.NewInstalled, so folding it
// would stop wake collecting a person who types the bare form — a wrong report
// turned into lost collection. One row out, both spellings still admitted in.
func TestClaudeCodeInScopeKeepsBothSpellingsOfAPluginSkill(t *testing.T) {
	claudeDir, root := pluginFixture(t)

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

	for _, want := range []primitiveKey{
		{kind: record.KindSkill, name: "brainstorming"},
		{kind: record.KindSkill, name: "superpowers:brainstorming"},
		{kind: record.KindSkill, name: "deploy"},
		{kind: record.KindSkill, name: "vercel:deploy"},
	} {
		if !discovered(got)[want] {
			t.Fatalf("missing %+v in %+v", want, got.Primitives)
		}
	}
}

func TestClaudeCodeInScopeCanonicalisesAPluginSkillOntoItsNamespace(t *testing.T) {
	claudeDir, root := pluginFixture(t)

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

	want := map[identity]record.Identifier{
		{harness: claudeCode, kind: record.KindSkill, name: "brainstorming"}: "superpowers:brainstorming",
		{harness: claudeCode, kind: record.KindSkill, name: "deploy"}:        "vercel:deploy",
	}
	if !maps.Equal(got.canonical, want) {
		t.Fatalf("canonical = %+v, want %+v", got.canonical, want)
	}
}

// Provenance is the whole proof. A bare name two sources contribute is not
// provably one primitive, so it is refused rather than resolved: refusing costs
// one pre-existing phantom row, while folding wrongly makes one primitive's
// counters absorb another's (ADR-0020).
func TestClaudeCodeInScopeRefusesToFoldANameAnotherSourceContributes(t *testing.T) {
	cases := map[string]func(t *testing.T, claudeDir, root string){
		"the user's own skills directory": func(t *testing.T, claudeDir, _ string) {
			write(t, filepath.Join(claudeDir, "skills", "brainstorming", "SKILL.md"), "# mine")
		},
		"the project's skills directory": func(t *testing.T, _, root string) {
			write(t, filepath.Join(root, ".claude", "skills", "brainstorming", "SKILL.md"), "# ours")
		},
		"a bare entry in a session listing": func(t *testing.T, claudeDir, root string) {
			write(t, filepath.Join(claudeDir, "projects", "extra.jsonl"),
				`{"cwd":"`+root+`","attachment":{"type":"skill_listing","content":"- brainstorming: something else"}}`)
		},
		"a second plugin shipping the same directory name": func(t *testing.T, claudeDir, _ string) {
			other := pluginSkill(t, "brainstorming")
			writeInstalledPlugins(t, claudeDir, map[string]string{
				"superpowers@claude-plugins-official": pluginSkill(t, "brainstorming"),
				"other@market":                        other,
			})
		},
	}
	for name, disputeIt := range cases {
		t.Run(name, func(t *testing.T) {
			claudeDir, root := pluginFixture(t)
			disputeIt(t, claudeDir, root)

			got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

			key := identity{harness: claudeCode, kind: record.KindSkill, name: "brainstorming"}
			if to, folded := got.canonical[key]; folded {
				t.Fatalf("a disputed name was folded onto %q", to)
			}
			for _, want := range []primitiveKey{
				{kind: record.KindSkill, name: "brainstorming"},
				{kind: record.KindSkill, name: "superpowers:brainstorming"},
			} {
				if !discovered(got)[want] {
					t.Fatalf("a refused fold lost %+v from %+v", want, got.Primitives)
				}
			}
		})
	}
}

// Landing a skill's canonical name on a name another kind already holds would
// change a primitive's kind. DG-106 refuses that case rather than resolving it;
// deciding it is DG-108's (ADR-0005).
func TestClaudeCodeInScopeRefusesAFoldOntoAnotherKind(t *testing.T) {
	claudeDir, root := pluginFixture(t)
	write(t, filepath.Join(claudeDir, "projects", "agents.jsonl"),
		`{"cwd":"`+root+`","attachment":{"type":"agent_listing_delta","addedTypes":["superpowers:brainstorming"]}}`)

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

	key := identity{harness: claudeCode, kind: record.KindSkill, name: "brainstorming"}
	if to, folded := got.canonical[key]; folded {
		t.Fatalf("a fold landed on another kind's name: %q", to)
	}
	for _, want := range []primitiveKey{
		{kind: record.KindSkill, name: "brainstorming"},
		{kind: record.KindSubagent, name: "superpowers:brainstorming"},
	} {
		if !discovered(got)[want] {
			t.Fatalf("missing %+v in %+v", want, got.Primitives)
		}
	}
}

// The namespace is persisted as part of a name, so it passes the same domain
// gate every other identifier does; and the composed name goes through
// record.Namer, which refuses the scope-<12 hex>: shape ADR-0020 reserves for a
// directory scope.
func TestClaudeCodeInScopeRefusesANamespaceOutsideTheTokenDomain(t *testing.T) {
	for _, key := range []string{"has space@market", "../escape@market", "scope-0123456789ab@market"} {
		t.Run(key, func(t *testing.T) {
			claudeDir := filepath.Join(t.TempDir(), ".claude")
			root := t.TempDir()
			writeInstalledPlugins(t, claudeDir, map[string]string{key: pluginSkill(t, "brainstorming")})

			got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

			if len(got.canonical) != 0 {
				t.Fatalf("canonical = %+v, want no entry", got.canonical)
			}
			if !discovered(got)[primitiveKey{kind: record.KindSkill, name: "brainstorming"}] {
				t.Fatalf("the plugin's own skill was lost: %+v", got.Primitives)
			}
		})
	}
}

// installed_plugins.json is a map, and Go walks a map in a random order. Neither
// the discovered set nor the fold may depend on the order this pass saw plugins
// in (ADR-0004).
func TestClaudeCodeInScopeDiscoveryIsIdenticalAcrossRuns(t *testing.T) {
	claudeDir, root := pluginFixture(t)
	scope := Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}

	first := ClaudeCodeInScope(scope, names)
	for run := range 4 {
		got := ClaudeCodeInScope(scope, names)
		if !slices.Equal(got.Primitives, first.Primitives) {
			t.Fatalf("run %d discovered %+v, want %+v", run, got.Primitives, first.Primitives)
		}
		if !maps.Equal(got.canonical, first.canonical) {
			t.Fatalf("run %d folded %+v, want %+v", run, got.canonical, first.canonical)
		}
	}
}

// discovered indexes a pass by the key the assertions above compare on.
func discovered(got Discovery) map[primitiveKey]bool {
	found := map[primitiveKey]bool{}
	for _, item := range got.Primitives {
		found[primitiveKey{kind: item.Kind, name: item.Name}] = true
	}
	return found
}

// pluginFixture writes a Claude directory holding two installed plugins in the
// real installed_plugins.json shape, each with its own skills directory, and a
// session listing spelling the same skills the way an invocation does. It is built
// here rather than under testdata/ because a fixture there must be captured through
// the redaction tooling (AGENTS.md § Off-limits paths).
func pluginFixture(t *testing.T) (claudeDir, root string) {
	t.Helper()
	claudeDir = filepath.Join(t.TempDir(), ".claude")
	root = t.TempDir()
	writeInstalledPlugins(t, claudeDir, map[string]string{
		"superpowers@claude-plugins-official": pluginSkill(t, "brainstorming"),
		"vercel@claude-plugins-official":      pluginSkill(t, "deploy"),
	})
	write(t, filepath.Join(claudeDir, "skills", "gather-context", "SKILL.md"), "# user skill")
	write(t, filepath.Join(claudeDir, "projects", "session.jsonl"), `{"cwd":"`+root+`","attachment":{"type":"skill_listing","content":"- gather-context: Use as the first pipeline stage\n- superpowers:brainstorming: You MUST use this\n- vercel:deploy: Deploy the current project"}}`)
	return claudeDir, root
}

// pluginSkill writes one plugin install directory holding one skill and returns
// the install path installed_plugins.json would name.
func pluginSkill(t *testing.T, skill string) string {
	t.Helper()
	installPath := filepath.Join(t.TempDir(), "install")
	write(t, filepath.Join(installPath, "skills", skill, "SKILL.md"), "# "+skill)
	return installPath
}

// writeInstalledPlugins writes installed_plugins.json in the shape Claude Code
// writes it: one entry per installed version, keyed "<plugin>@<marketplace>".
func writeInstalledPlugins(t *testing.T, claudeDir string, installs map[string]string) {
	t.Helper()
	entries := make([]string, 0, len(installs))
	for _, key := range slices.Sorted(maps.Keys(installs)) {
		entry, err := json.Marshal(map[string]string{
			"scope":       "user",
			"installPath": installs[key],
			"version":     "1.0.0",
			"installedAt": "2026-06-18T14:38:10.185Z",
		})
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		name, err := json.Marshal(key)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		entries = append(entries, string(name)+":["+string(entry)+"]")
	}
	write(t, filepath.Join(claudeDir, "plugins", "installed_plugins.json"),
		`{"version":1,"plugins":{`+strings.Join(entries, ",")+`}}`)
}

// DG-106's `--unused` criterion, asserted through the whole path below the
// renderer: discovery over a real installed_plugins.json and a real skill_listing,
// then the join that produces the rows report and serve draw.
//
// One row in, one line out is what makes this sufficient evidence: report renders
// []inventory.Usage — derive's output — one line per row with no invocations, and
// internal/cli/report.go passes Store.Read() straight in. Nothing between them can
// duplicate a skill.
func TestClaudeCodeDiscoveryYieldsOneInventoryRowPerPluginSkill(t *testing.T) {
	claudeDir, root := pluginFixture(t)
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		inventoryRecord("one", "superpowers:brainstorming", at),
		inventoryRecord("two", "superpowers:brainstorming", at.Add(time.Minute)),
		inventoryRecord("three", "brainstorming", at.Add(2*time.Minute)),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))

	discovery := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)
	if err := primitives.Refresh(events, discovery, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	rows := map[usageKey]Usage{}
	for _, usage := range items {
		key := usageKey{identity: identity{kind: usage.Kind, name: usage.Name}, repo: usage.Repo}
		if _, duplicate := rows[key]; duplicate {
			t.Fatalf("%s %q is listed twice: %+v", usage.Kind, usage.Name, items)
		}
		rows[key] = usage
	}
	for _, phantom := range []record.Identifier{"brainstorming", "deploy"} {
		for _, usage := range items {
			if usage.Name == phantom {
				t.Fatalf("the bare spelling %q survived as its own row: %+v", phantom, items)
			}
		}
	}
	used := rows[usageKey{identity: identity{kind: record.KindSkill, name: "superpowers:brainstorming"}, repo: "0123456789abcdef0123456789abcdef"}]
	if used.Invocations != 3 {
		t.Fatalf("superpowers:brainstorming = %+v, want 3 invocations accumulated from both spellings", used)
	}
	for _, unused := range []record.Identifier{"vercel:deploy", "gather-context"} {
		row, found := rows[usageKey{identity: identity{kind: record.KindSkill, name: unused}}]
		if !found || row.Invocations != 0 {
			t.Fatalf("%q = %+v, %t; want exactly one row with no invocations", unused, row, found)
		}
	}
}
