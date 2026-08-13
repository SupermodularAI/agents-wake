package ingest

import (
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
