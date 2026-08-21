package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

func TestReportCommandPrintsStoredActivity(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(root)
	skill := filepath.Join(root, ".claude", "skills")
	if err := os.MkdirAll(filepath.Join(skill, "report"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(skill, "unused"), 0o700); err != nil {
		t.Fatalf("MkdirAll() unused skill error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(skill, "report", "SKILL.md"), []byte("# report\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(skill, "unused", "SKILL.md"), []byte("# unused\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() unused skill error = %v", err)
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() error = %v", err)
	}
	consent(t, paths, root)
	ok := record.OutcomeOK
	item := record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID("claude-code", "report-command"),
		Timestamp:     time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC),
		Harness:       "claude-code",
		SessionID:     "session-1",
		Repo:          "0123456789abcdef0123456789abcdef",
		Kind:          record.KindSkill,
		Name:          "report",
		Invoker:       record.InvokerModel,
		Outcome:       &ok,
	}
	_, err = store.New(filepath.Join(paths.DataDir, "events.ndjson")).Append([]record.Record{item})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	out, err := run(t, "report", "--usage")
	if err != nil {
		t.Fatalf("wake report error = %v: %s", err, out)
	}
	for _, want := range []string{"WAKE REPORT", "Last observed: 2026-08-13T12:00:00Z", "USED PRIMITIVES", "report"} {
		if !strings.Contains(out, want) {
			t.Errorf("wake report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nUNUSED PRIMITIVES\n") {
		t.Fatalf("wake report --usage included unused primitives:\n%s", out)
	}

	out, err = run(t, "report", "--unused")
	if err != nil {
		t.Fatalf("wake report --unused error = %v: %s", err, out)
	}
	if !strings.Contains(out, "\nUNUSED PRIMITIVES\n") || strings.Contains(out, "\nUSED PRIMITIVES\n") || !strings.Contains(out, "unused") {
		t.Fatalf("wake report --unused output = %s", out)
	}
}

func TestReportInsideAConsentedRepositoryPersistsItsLocalPrimitive(t *testing.T) {
	paths, root := reportFixture(t)
	consent(t, paths, root)

	if out, err := run(t, "report"); err != nil {
		t.Fatalf("wake report error = %v: %s", err, out)
	}
	raw := readPrimitives(t, paths)
	if !strings.Contains(raw, `"name": "report"`) {
		t.Fatalf("primitives.json lost the project-local skill: %s", raw)
	}
}

func TestReportOutsideAConsentedRepositoryPersistsNoProjectPrimitive(t *testing.T) {
	paths, root := reportFixture(t)

	out, err := run(t, "report")
	if err != nil {
		t.Fatalf("wake report error = %v: %s", err, out)
	}
	raw := readPrimitives(t, paths)
	for _, forbidden := range []string{`"name": "report"`, `"name": "unused"`, root} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("primitives.json contains %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(raw, `"name": "global-skill"`) {
		t.Fatalf("primitives.json skipped the global refresh: %s", raw)
	}
	if !strings.Contains(out, "not a consented repository") {
		t.Fatalf("wake report printed no notice:\n%s", out)
	}
}

func TestReportOutsideAConsentedRepositoryLeavesConsentedUsageIntact(t *testing.T) {
	paths, _ := reportFixture(t)
	appendReportRecord(t, paths)

	out, err := run(t, "report")
	if err != nil {
		t.Fatalf("wake report error = %v: %s", err, out)
	}
	if !strings.Contains(out, "Last observed: 2026-08-13T12:00:00Z") {
		t.Fatalf("wake report withheld event-derived counters:\n%s", out)
	}
}

func TestReportOutsideAConsentedRepositoryKeepsItsProjectPrimitives(t *testing.T) {
	paths, root := reportFixture(t)
	consent(t, paths, root)
	appendReportRecord(t, paths)

	// Inside the consented repository the snapshot holds its project-local skills.
	if out, err := run(t, "report"); err != nil {
		t.Fatalf("wake report inside the repository error = %v: %s", err, out)
	}
	raw := readPrimitives(t, paths)
	for _, want := range []string{`"name": "report"`, `"name": "unused"`, `"name": "global-skill"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("primitives.json is missing %s: %s", want, raw)
		}
	}

	// A command run somewhere Wake was never consented to cannot see them, and
	// therefore cannot have learned they are gone. Replacing the snapshot from that
	// view would erase the repository's primitives and their counters until a
	// command happened to run inside it again.
	t.Chdir(t.TempDir())
	out, err := run(t, "report")
	if err != nil {
		t.Fatalf("wake report outside the repository error = %v: %s", err, out)
	}
	raw = readPrimitives(t, paths)
	for _, want := range []string{`"name": "report"`, `"name": "unused"`, `"name": "global-skill"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("an unconsented scan replaced the snapshot, losing %s: %s", want, raw)
		}
	}
	if !strings.Contains(raw, `"invocations": 1`) {
		t.Fatalf("primitives.json lost the counter of a carried primitive: %s", raw)
	}
	if strings.Contains(raw, root) {
		t.Fatalf("primitives.json contains the working directory: %s", raw)
	}
}

// reportFixture isolates Wake's state, writes one global and two project-local
// skills, and moves into the project directory without consenting to it.
func reportFixture(t *testing.T) (config.Paths, string) {
	t.Helper()
	t.Setenv(config.EnvDataDir, t.TempDir())
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(root)
	for _, name := range []string{"report", "unused"} {
		writeSkill(t, filepath.Join(root, ".claude", "skills", name))
	}
	writeSkill(t, filepath.Join(claudeHome(t), "skills", "global-skill"))
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() error = %v", err)
	}
	return paths, root
}

func writeSkill(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("# skill\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

// consent registers the directory a command will run in, standing in for the
// wake init the user would have run there.
func consent(t *testing.T, paths config.Paths, root string) {
	t.Helper()
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	if _, err := repos.Register(root, filepath.Base(root), time.Time{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
}

// TestReportAggregatesEveryConsentedRepositoryNotOnlyTheOneItRunsIn is the
// machine-wide surface plan §8 promises ("served dashboard, navigation and
// filters"): a repo consented to earlier must not disappear just because a
// later `wake init` in a different repo, or simply a `wake report` run from
// somewhere else, becomes the invocation report resolves its scope from.
func TestReportAggregatesEveryConsentedRepositoryNotOnlyTheOneItRunsIn(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	repoA := t.TempDir()
	writeSkill(t, filepath.Join(repoA, ".claude", "skills", "skill-a"))
	repoB := t.TempDir()
	writeSkill(t, filepath.Join(repoB, ".claude", "skills", "skill-b"))

	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() error = %v", err)
	}
	consent(t, paths, repoA)
	consent(t, paths, repoB)

	ok := record.OutcomeOK
	events := []record.Record{
		{
			SchemaVersion: record.SchemaVersion,
			EventID:       record.DeriveEventID("claude-code", "skill-a-run"),
			Timestamp:     time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC),
			Harness:       "claude-code",
			SessionID:     "session-a",
			Repo:          "0123456789abcdef0123456789abcdef",
			Kind:          record.KindSkill,
			Name:          "skill-a",
			Invoker:       record.InvokerModel,
			Outcome:       &ok,
		},
		{
			SchemaVersion: record.SchemaVersion,
			EventID:       record.DeriveEventID("claude-code", "skill-b-run"),
			Timestamp:     time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC),
			Harness:       "claude-code",
			SessionID:     "session-b",
			Repo:          "fedcba9876543210fedcba9876543210",
			Kind:          record.KindSkill,
			Name:          "skill-b",
			Invoker:       record.InvokerModel,
			Outcome:       &ok,
		},
	}
	if _, appendErr := store.New(filepath.Join(paths.DataDir, "events.ndjson")).Append(events); appendErr != nil {
		t.Fatalf("Append() error = %v", appendErr)
	}

	// Standing inside repo B only, the way the user does right after the second
	// `wake init` — repo A is not cwd, and is not even a subdirectory of it.
	t.Chdir(repoB)

	out, err := run(t, "report", "--usage")
	if err != nil {
		t.Fatalf("wake report error = %v: %s", err, out)
	}
	if !strings.Contains(out, "skill-b") {
		t.Fatalf("wake report lost the repository it is running in: %s", out)
	}
	if !strings.Contains(out, "skill-a") {
		t.Fatalf("wake report only shows the current directory's repo, not every consented one: %s", out)
	}
}

func appendReportRecord(t *testing.T, paths config.Paths) {
	t.Helper()
	ok := record.OutcomeOK
	item := record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID("claude-code", "report-command"),
		Timestamp:     time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC),
		Harness:       "claude-code",
		SessionID:     "session-1",
		Repo:          "0123456789abcdef0123456789abcdef",
		Kind:          record.KindSkill,
		Name:          "report",
		Invoker:       record.InvokerModel,
		Outcome:       &ok,
	}
	if _, err := store.New(filepath.Join(paths.DataDir, "events.ndjson")).Append([]record.Record{item}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
}

func readPrimitives(t *testing.T, paths config.Paths) string {
	t.Helper()
	raw, err := os.ReadFile(paths.PrimitivesFile)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	return string(raw)
}
