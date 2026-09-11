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

	want := map[identity]identity{
		{harness: claudeCode, kind: record.KindSkill, name: "brainstorming"}: {harness: claudeCode, kind: record.KindSkill, name: "superpowers:brainstorming"},
		{harness: claudeCode, kind: record.KindSkill, name: "deploy"}:        {harness: claudeCode, kind: record.KindSkill, name: "vercel:deploy"},
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

// AC1. Where a session skill_listing names the composed spelling, the harness has
// stated the kind it will invoke that primitive under, and that declaration is the
// kind the folded row records — whatever kind the directory scan happened to assign
// (ADR-0041). Without it the command row is structurally unreachable by any event
// and reads invocations: 0 forever.
func TestClaudeCodeInScopeTakesTheKindFromTheListingDeclaration(t *testing.T) {
	claudeDir, root := pluginCommandFixture(t)

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

	from := identity{harness: claudeCode, kind: record.KindCommand, name: "code-review"}
	want := identity{harness: claudeCode, kind: record.KindSkill, name: "code-review:code-review"}
	if to, folded := got.canonical[from]; !folded || to != want {
		t.Fatalf("canonical[%+v] = %+v, %t; want %+v, true", from, to, folded, want)
	}
	// One row out, both spellings still admitted in: activation hands Primitives to
	// claudecode.NewInstalled, so folding that slice would lose collection.
	for _, spelling := range []primitiveKey{
		{kind: record.KindCommand, name: "code-review"},
		{kind: record.KindSkill, name: "code-review:code-review"},
	} {
		if !discovered(got)[spelling] {
			t.Fatalf("missing %+v in %+v", spelling, got.Primitives)
		}
	}
}

// AC2, the refusal half. Where no listing names the primitive, nothing has stated
// which kind the harness invokes it under, and choosing one from the directory a
// file happened to sit in would decide a record dimension by walk order (ADR-0005,
// ADR-0004). The bare row survives, and claudecode.NewInstalled's own refusal is
// still the whole answer for a contested name.
func TestClaudeCodeInScopeRefusesACrossKindFoldNoListingDeclares(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	root := t.TempDir()
	writeInstalledPlugins(t, claudeDir, map[string]string{
		"code-review@claude-plugins-official": pluginCommand(t, "code-review"),
	})

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

	if len(got.canonical) != 0 {
		t.Fatalf("canonical = %+v, want no entry", got.canonical)
	}
	if !discovered(got)[primitiveKey{kind: record.KindCommand, name: "code-review"}] {
		t.Fatalf("the plugin's own command was lost: %+v", got.Primitives)
	}
}

// The cross-kind fold keeps DG-106's provenance guard, which did not exist for
// commands before this ticket. A bare name two sources contribute is not provably
// one primitive, and folding it would make one primitive's counters absorb
// another's (ADR-0020).
func TestClaudeCodeInScopeRefusesToFoldACommandAnotherSourceContributes(t *testing.T) {
	cases := map[string]func(t *testing.T, claudeDir, root string){
		"the user's own commands directory": func(t *testing.T, claudeDir, _ string) {
			write(t, filepath.Join(claudeDir, "commands", "code-review.md"), "# mine")
		},
		"the project's commands directory": func(t *testing.T, _, root string) {
			write(t, filepath.Join(root, ".claude", "commands", "code-review.md"), "# ours")
		},
		"a second plugin shipping the same command": func(t *testing.T, claudeDir, _ string) {
			writeInstalledPlugins(t, claudeDir, map[string]string{
				"code-review@claude-plugins-official": pluginCommand(t, "code-review"),
				"other@market":                        pluginCommand(t, "code-review"),
			})
		},
	}
	for name, disputeIt := range cases {
		t.Run(name, func(t *testing.T) {
			claudeDir, root := pluginCommandFixture(t)
			disputeIt(t, claudeDir, root)

			got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

			key := identity{harness: claudeCode, kind: record.KindCommand, name: "code-review"}
			if to, folded := got.canonical[key]; folded {
				t.Fatalf("a disputed name was folded onto %+v", to)
			}
			for _, want := range []primitiveKey{
				{kind: record.KindCommand, name: "code-review"},
				{kind: record.KindSkill, name: "code-review:code-review"},
			} {
				if !discovered(got)[want] {
					t.Fatalf("a refused fold lost %+v from %+v", want, got.Primitives)
				}
			}
		})
	}
}

// The DG-108 twin of TestClaudeCodeInScopeRefusesAFoldOntoAnotherKind: generalising
// heldByAnotherKind to take the target kind must not open a hole. A listing declares
// the target a skill, but a subagent already holds that name, and nothing states
// which of the two the harness means — so the fold is refused rather than resolved
// (ADR-0020).
func TestClaudeCodeInScopeRefusesACrossKindFoldOntoAContestedName(t *testing.T) {
	claudeDir, root := pluginCommandFixture(t)
	write(t, filepath.Join(claudeDir, "projects", "agents.jsonl"),
		`{"cwd":"`+root+`","attachment":{"type":"agent_listing_delta","addedTypes":["code-review:code-review"]}}`)

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

	key := identity{harness: claudeCode, kind: record.KindCommand, name: "code-review"}
	if to, folded := got.canonical[key]; folded {
		t.Fatalf("a fold landed on a contested name: %+v", to)
	}
	for _, want := range []primitiveKey{
		{kind: record.KindCommand, name: "code-review"},
		{kind: record.KindSkill, name: "code-review:code-review"},
		{kind: record.KindSubagent, name: "code-review:code-review"},
	} {
		if !discovered(got)[want] {
			t.Fatalf("missing %+v in %+v", want, got.Primitives)
		}
	}
}

// A listing that could not be read collects nothing and licenses nothing: format
// drift degrades soft, and it must never be able to flip a primitive's kind
// (plan §4.3).
func TestClaudeCodeInScopeIgnoresADeclarationFromAnUnreadableListing(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	root := t.TempDir()
	writeInstalledPlugins(t, claudeDir, map[string]string{
		"code-review@claude-plugins-official": pluginCommand(t, "code-review"),
	})
	write(t, filepath.Join(claudeDir, "projects", "session.jsonl"),
		`{"cwd":"`+root+`","attachment":{"type":"skill_listing","content":"- code-review:code-review: `+strings.Repeat("A", 2*1024*1024)+`"}}`)

	got := ClaudeCodeInScope(Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}, names)

	if len(got.canonical) != 0 {
		t.Fatalf("canonical = %+v, want no entry", got.canonical)
	}
	if !discovered(got)[primitiveKey{kind: record.KindCommand, name: "code-review"}] {
		t.Fatalf("the plugin's own command was lost: %+v", got.Primitives)
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

// pluginCommandFixture is the machine shape DG-108 resolves: a real
// installed_plugins.json holding a plugin that ships a command, and a real
// skill_listing naming that same command under its namespaced spelling, as Claude
// Code writes both. Discovery therefore finds one primitive twice — bare under
// record.KindCommand, namespaced under record.KindSkill.
//
// It is separate from pluginFixture on purpose: that fixture's fold is asserted
// exhaustively with maps.Equal, and extending it would churn DG-106's tests for no
// gain.
func pluginCommandFixture(t *testing.T) (claudeDir, root string) {
	t.Helper()
	claudeDir = filepath.Join(t.TempDir(), ".claude")
	root = t.TempDir()
	writeInstalledPlugins(t, claudeDir, map[string]string{
		"code-review@claude-plugins-official": pluginCommand(t, "code-review"),
	})
	write(t, filepath.Join(claudeDir, "projects", "session.jsonl"),
		`{"cwd":"`+root+`","attachment":{"type":"skill_listing","content":"- code-review:code-review: Code review a pull request"}}`)
	return claudeDir, root
}

// pluginCommand writes one plugin install directory holding one command and returns
// the install path installed_plugins.json would name. A plugin command is discovered
// bare from <installPath>/commands, while the harness lists and invokes it
// namespaced — the disagreement DG-108 resolves.
func pluginCommand(t *testing.T, command string) string {
	t.Helper()
	installPath := filepath.Join(t.TempDir(), "install")
	write(t, filepath.Join(installPath, "commands", command+".md"), "# "+command)
	return installPath
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

	rows := map[identity]Usage{}
	for _, usage := range items {
		key := identity{kind: usage.Kind, name: usage.Name}
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
	used := rows[identity{kind: record.KindSkill, name: "superpowers:brainstorming"}]
	if used.Invocations != 3 {
		t.Fatalf("superpowers:brainstorming = %+v, want 3 invocations accumulated from both spellings", used)
	}
	for _, unused := range []record.Identifier{"vercel:deploy", "gather-context"} {
		row, found := rows[identity{kind: record.KindSkill, name: unused}]
		if !found || row.Invocations != 0 {
			t.Fatalf("%q = %+v, %t; want exactly one row with no invocations", unused, row, found)
		}
	}
}

// DG-108's criterion, asserted through the whole path below the renderer:
// discovery over a real installed_plugins.json holding a plugin command plus the
// real skill_listing that names it, then the join that produces the rows report
// and serve draw.
//
// The bare command row is what read invocations: 0 on a real machine while the
// primitive was used daily. After the fold it does not exist as a row at all, and
// the invocations recorded under both spellings — and both kinds — accumulate onto
// the one row the harness says it invokes.
func TestClaudeCodeDiscoveryYieldsOneRowForAPluginCommandTheListingCallsASkill(t *testing.T) {
	claudeDir, root := pluginCommandFixture(t)
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		inventoryRecord("one", "code-review:code-review", at),
		inventoryRecord("two", "code-review:code-review", at.Add(time.Minute)),
		commandRecord("three", "code-review", at.Add(2*time.Minute)),
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

	rows := map[identity]Usage{}
	for _, usage := range items {
		if usage.Name == "code-review" {
			t.Fatalf("the bare spelling survived as its own row: %+v", items)
		}
		if usage.Kind == record.KindCommand {
			t.Fatalf("a command row survived the fold: %+v", items)
		}
		key := identity{kind: usage.Kind, name: usage.Name}
		if _, duplicate := rows[key]; duplicate {
			t.Fatalf("%s %q is listed twice: %+v", usage.Kind, usage.Name, items)
		}
		rows[key] = usage
	}
	used := rows[identity{kind: record.KindSkill, name: "code-review:code-review"}]
	if used.Invocations != 3 {
		t.Fatalf("code-review:code-review = %+v, want 3 invocations accumulated from both spellings", used)
	}
}

// Order-independence is a property of the construction, not of one ordering: the
// fold is computed over the completed discovered set, so nothing about it may depend
// on which session file was walked first, which entry a listing carried first, or
// which order installed_plugins.json's map happened to yield (ADR-0004).
//
// Every variant below is the same logical machine — one folding plugin skill
// (DG-106) and one folding plugin command (DG-108) — assembled a different way, and
// each is repeated because Go walks both o.plugins and discovered as maps.
func TestClaudeCodeInScopeFoldsTheSameWayInAnyDiscoveryOrder(t *testing.T) {
	skillEntry := "- superpowers:brainstorming: You MUST use this"
	commandEntry := "- code-review:code-review: Code review a pull request"
	variants := map[string]func(t *testing.T) (claudeDir, root string){
		"one listing, skill entry first": func(t *testing.T) (string, string) {
			return orderFixture(t, map[string][]string{"session.jsonl": {skillEntry, commandEntry}})
		},
		"one listing, command entry first": func(t *testing.T) (string, string) {
			return orderFixture(t, map[string][]string{"session.jsonl": {commandEntry, skillEntry}})
		},
		"two listings, skill in the lexically first file": func(t *testing.T) (string, string) {
			return orderFixture(t, map[string][]string{"a-session.jsonl": {skillEntry}, "z-session.jsonl": {commandEntry}})
		},
		"two listings, command in the lexically first file": func(t *testing.T) (string, string) {
			return orderFixture(t, map[string][]string{"a-session.jsonl": {commandEntry}, "z-session.jsonl": {skillEntry}})
		},
	}

	var first *Discovery
	for name, build := range variants {
		t.Run(name, func(t *testing.T) {
			claudeDir, root := build(t)
			scope := Scope{ClaudeDir: claudeDir, Root: root, Project: ProjectConsented}
			for range 3 {
				got := ClaudeCodeInScope(scope, names)
				if first == nil {
					snapshot := got
					first = &snapshot
					continue
				}
				if !slices.Equal(got.Primitives, first.Primitives) {
					t.Fatalf("discovered %+v, want %+v", got.Primitives, first.Primitives)
				}
				if !maps.Equal(got.canonical, first.canonical) {
					t.Fatalf("folded %+v, want %+v", got.canonical, first.canonical)
				}
			}
		})
	}
	if first == nil || len(first.canonical) != 2 {
		t.Fatalf("the fixture folds %+v, want both a skill and a command fold", first)
	}
}

// orderFixture is one plugin skill and one plugin command, with the listing entries
// that name them distributed across the session files named. The plugins are written
// with marketplace qualifiers and install paths whose sorted order differs from their
// insertion order, so installedPlugins' own sort is what decides, not the map walk.
func orderFixture(t *testing.T, listings map[string][]string) (claudeDir, root string) {
	t.Helper()
	claudeDir = filepath.Join(t.TempDir(), ".claude")
	root = t.TempDir()
	writeInstalledPlugins(t, claudeDir, map[string]string{
		"superpowers@zz-marketplace":          pluginSkill(t, "brainstorming"),
		"code-review@aa-claude-plugins-first": pluginCommand(t, "code-review"),
	})
	for file, entries := range listings {
		write(t, filepath.Join(claudeDir, "projects", file),
			`{"cwd":"`+root+`","attachment":{"type":"skill_listing","content":"`+strings.Join(entries, `\n`)+`"}}`)
	}
	return claudeDir, root
}
