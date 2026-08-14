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
	for _, want := range []string{"WAKE REPORT", "Terminal invocations  1", "USED PRIMITIVES", "report"} {
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
