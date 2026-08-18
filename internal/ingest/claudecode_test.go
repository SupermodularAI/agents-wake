package ingest

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// names keys the scope digest for this package's tests, standing in for the
// subkey config.Repos.NameKey derives in production.
var names = record.NewNamer([]byte("test scope key"))

func TestClaudeCodeIsIdempotent(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	destination := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string) (record.Hash, bool) { return repo, cwd == "/repo" }

	first, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, destination)
	if err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	second, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	if first.Written != 1 || second.Duplicate != 1 {
		t.Fatalf("results = %+v, %+v", first, second)
	}
}

func TestClaudeCodePersistsBothToolCallsFromOneSourceEntry(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"},{"type":"tool_use","id":"call-2","name":"Task","input":{"subagent_type":"explorer"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false},{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}`,
	}, "\n")
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string) (record.Hash, bool) { return repo, cwd == "/repo" }

	first, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, destination)
	if err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	if first.Written != 2 || first.Duplicate != 0 || first.Dropped != 0 {
		t.Fatalf("first ClaudeCode() = %+v, want two written records", first)
	}

	before, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	second, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	if second.Written != 0 || second.Duplicate != 2 {
		t.Fatalf("second ClaudeCode() = %+v, want both records recognised as duplicates", second)
	}
	after, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("re-ingest changed the spool:\nbefore %s\nafter  %s", before, after)
	}

	lines := 0
	for _, line := range bytes.Split(after, []byte("\n")) {
		if len(line) > 0 {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("events.ndjson holds %d lines, want 2: %s", lines, after)
	}
}

func TestClaudeCodeCountsARefusedSubagentCallWithoutWritingIt(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Task"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string) (record.Hash, bool) { return repo, cwd == "/repo" }

	result, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, destination)
	if err != nil {
		t.Fatalf("ClaudeCode() error = %v", err)
	}
	if result.Refused != 1 {
		t.Errorf("Refused = %d, want 1: the reader's refusal has to reach the caller", result.Refused)
	}
	// The refusal is its own fail-closed point: not a record the store dropped, not
	// an unusable line, and nothing parsed or written.
	if result.Written != 0 || result.Parsed != 0 || result.Malformed != 0 || result.Dropped != 0 {
		t.Errorf("ClaudeCode() = %+v, want nothing parsed, written or dropped", result)
	}
	if _, statErr := os.Stat(spool); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("Stat(spool) error = %v, want the spool never created", statErr)
	}
}

func TestClaudeCodePersistsNoPathShapedValue(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":"usr/local/bin"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","attributionSkill":"a/../secrets","attributionMcpServer":"plugin:a/../evil:tool","message":{"model":"C:/Users/me","content":[{"type":"tool_use","id":"call-2","name":"Bash"}]}}`,
		`{"uuid":"entry-4","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}`,
		// A directory-scoped subagent reference, carried on the Task call that is the
		// subagent invocation: only the keyed digest of the scope may be persisted,
		// never the path fragment it was derived from (ADR-0020).
		`{"uuid":"entry-5","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:04Z","message":{"content":[{"type":"tool_use","id":"call-3","name":"Task","input":{"subagent_type":"apps/web:reviewer"}}]}}`,
		`{"uuid":"entry-6","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-3","is_error":false}]}}`,
	}, "\n")
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	destination := store.New(spool)
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string) (record.Hash, bool) { return repo, cwd == "/repo" }

	result, err := ClaudeCode(strings.NewReader(input), resolve, names, claudecode.Staleness{}, destination)
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

func TestClaudeCodeWritesAnInterruptedCallExactlyOnce(t *testing.T) {
	// A session killed mid-call leaves a tool_use with no tool_result. Once the
	// staleness threshold has passed, the reader resolves it to outcome interrupted —
	// and because the id comes from the source event rather than from the scan
	// (ADR-0004), a second scan of the same transcript writes nothing more. That is
	// what makes a retry, a rescan and two concurrent scans all safe.
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`
	destination := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	resolve := func(cwd string) (record.Hash, bool) { return repo, cwd == "/repo" }
	stale := claudecode.Staleness{
		Timeout: time.Hour,
		Now:     time.Date(2026, 8, 13, 14, 0, 0, 0, time.UTC),
	}

	first, err := ClaudeCode(strings.NewReader(input), resolve, names, stale, destination)
	if err != nil {
		t.Fatalf("first ClaudeCode() error = %v", err)
	}
	if first.Written != 1 || first.Interrupted != 1 || first.Duplicate != 0 || first.Pending != 0 {
		t.Fatalf("first ClaudeCode() = %+v", first)
	}

	second, err := ClaudeCode(strings.NewReader(input), resolve, names, stale, destination)
	if err != nil {
		t.Fatalf("second ClaudeCode() error = %v", err)
	}
	if second.Written != 0 || second.Duplicate != 1 || second.Interrupted != 1 {
		t.Fatalf("second ClaudeCode() = %+v", second)
	}

	entries, err := destination.Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Entries() = %+v, want exactly one record", entries)
	}
	if outcome := entries[0].Record.Outcome; outcome == nil || *outcome != record.OutcomeInterrupted {
		t.Fatalf("Outcome = %v, want interrupted", outcome)
	}
}
