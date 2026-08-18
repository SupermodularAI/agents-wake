package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Acceptance criterion 2: every command that reads Claude Code's directory reads
// the same one.
//
// It is asserted by behaviour rather than by reading the source, because the
// failure this guards against is behavioural: a command pointed at a directory
// Claude Code never writes does not fail — it silently reports collecting zero.
// So each step drives one command against a single sentinel directory and looks
// for the effect *inside that directory*: the settings file init disclosed and
// created, the live hook count doctor read back out of it, the one event ingest
// found in the transcript under it, the skill discovery picked up from it, and the
// hook removal remove performed on it. A command resolving anywhere else stops
// witnessing at its own step.
//
// The pairing with internal/config is what makes this non-circular: this test
// pins all five commands to whatever config.ClaudeCodeDir returns, and
// TestClaudeCodeDirIsTheHomeRelativeHarnessDirectory independently pins that
// return value to ~/.claude.
func TestEveryCommandResolvesTheSameClaudeCodeDirectory(t *testing.T) {
	paths := isolate(t)
	dir := claudeHome(t)
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() root error = %v", err)
	}
	t.Chdir(root)
	// The resolved form, which is what init registers: on darwin t.TempDir() sits
	// behind a symlink, and a transcript naming the unresolved path would resolve to
	// no consented repository and be skipped for the wrong reason.
	consented, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	// init — the disclosure names the settings file under dir, and the file lands
	// there. Naming it is the ADR-0010 half; creating it is the resolution half.
	settings := filepath.Join(dir, "settings.json")
	out, err := run(t, "init")
	if err != nil {
		t.Fatalf("init error = %v: %s", err, out)
	}
	if !strings.Contains(out, settings) {
		t.Errorf("init disclosed no settings file under %q:\n%s", dir, out)
	}
	if _, statErr := os.Stat(settings); statErr != nil {
		t.Fatalf("init wrote no settings file under %q: %v", dir, statErr)
	}

	// doctor — the count is read live out of its own resolved settings file, and in
	// a fresh isolated HOME that file exists in exactly one place.
	out, _, err = runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "hooks installed: 2") {
		t.Errorf("doctor read no hooks from %q:\n%s", dir, out)
	}

	// ingest — one terminal call, in a transcript written under dir after init
	// imported nothing, so the import can only be this event.
	transcriptDir := filepath.Join(dir, "projects", "session")
	if err = os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() transcript error = %v", err)
	}
	transcript := `{"uuid":"entry-1","sessionId":"session-1","cwd":"` + consented + `","timestamp":"2026-08-17T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}
{"uuid":"entry-2","sessionId":"session-1","cwd":"` + consented + `","timestamp":"2026-08-17T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`
	if err = os.WriteFile(filepath.Join(transcriptDir, "session.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatalf("WriteFile() transcript error = %v", err)
	}
	out, _, err = runSplit(t, "ingest")
	if err != nil {
		t.Fatalf("ingest error = %v", err)
	}
	if !strings.Contains(out, "Imported 1 terminal events.") {
		t.Errorf("ingest found no transcript under %q:\n%s", dir, out)
	}

	// The discovery scope report and serve share — global primitive discovery reads
	// <its own directory>/skills and nothing else, so the sentinel appearing in the
	// snapshot is that directory being dir.
	writeSkill(t, filepath.Join(dir, "skills", "resolver-sentinel"))
	out, err = run(t, "report")
	if err != nil {
		t.Fatalf("report error = %v: %s", err, out)
	}
	if raw := readPrimitives(t, paths); !strings.Contains(raw, `"name": "resolver-sentinel"`) {
		t.Errorf("report discovered no global skill under %q: %s", dir, raw)
	}

	// remove last, because it strips the hooks the steps above rely on. It
	// republishes only its own resolved settings file, so the ownership marker
	// disappearing from *that* file is where remove was looking.
	out, _, err = runSplit(t, "remove")
	if err != nil {
		t.Fatalf("remove error = %v", err)
	}
	if !strings.Contains(out, "Removed Wake's Claude Code integration.") {
		t.Errorf("remove found no integration under %q:\n%s", dir, out)
	}
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("ReadFile() settings error = %v", err)
	}
	if strings.Contains(string(raw), `"wake"`) {
		t.Errorf("remove left its hooks in %q: %s", settings, raw)
	}
}
