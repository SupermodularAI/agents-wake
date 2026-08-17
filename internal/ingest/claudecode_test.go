package ingest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

func TestClaudeCodeIsIdempotent(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	destination := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string) (record.Hash, bool) { return repo, cwd == "/repo" }

	first, err := ClaudeCode(strings.NewReader(input), resolve, destination)
	if err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	second, err := ClaudeCode(strings.NewReader(input), resolve, destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	if first.Written != 1 || second.Duplicate != 1 {
		t.Fatalf("results = %+v, %+v", first, second)
	}
}

func TestClaudeCodePersistsNoPathShapedValue(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":"usr/local/bin"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","attributionSkill":"a/../secrets","attributionMcpServer":"plugin:a/../evil:tool","message":{"model":"C:/Users/me","content":[{"type":"tool_use","id":"call-2","name":"Bash"}]}}`,
		`{"uuid":"entry-4","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}`,
		`{"uuid":"entry-5","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:04Z","attributionAgent":"apps/web:reviewer","message":{"stop_reason":"end_turn"}}`,
	}, "\n")
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string) (record.Hash, bool) { return repo, cwd == "/repo" }

	result, err := ClaudeCode(strings.NewReader(input), resolve, destination)
	if err != nil {
		t.Fatalf("ClaudeCode() error = %v", err)
	}
	if result.Written != 2 {
		t.Fatalf("ClaudeCode() = %+v, want 2 written", result)
	}

	raw, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	lines := 0
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) > 0 {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("events.ndjson holds %d lines: %s", lines, raw)
	}
	for _, fragment := range []string{"/", `\`, "usr/local", "secrets", "Users", "evil", "apps", "web"} {
		if bytes.Contains(raw, []byte(fragment)) {
			t.Fatalf("events.ndjson contains %q: %s", fragment, raw)
		}
	}
	if !bytes.Contains(raw, []byte("reviewer")) {
		t.Fatalf("events.ndjson dropped the safe half of a scoped reference: %s", raw)
	}
}
