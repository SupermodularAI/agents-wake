package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter/opencode"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// openCodeInstant is when this file's opencode fixtures happen.
var openCodeInstant = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// openCodeRepo is a repository id in the domain record.ValidRepo admits.
const openCodeRepo = record.Hash("0123456789abcdef0123456789abcdef")

func openCodeConsents(string, time.Time) (record.Hash, bool) { return openCodeRepo, true }

func openCodeSession(id string) opencode.Session {
	return opencode.Session{
		ID:        id,
		Directory: "/home/dev/project",
		Version:   "1.18.30",
		UpdatedMS: openCodeInstant.UnixMilli(),
	}
}

func openCodePart(id, tool, status string) opencode.ToolPart {
	return opencode.ToolPart{
		ID:        id,
		SessionID: "ses_abc",
		Tool:      tool,
		Status:    status,
		StartMS:   openCodeInstant.UnixMilli(),
		EndMS:     openCodeInstant.UnixMilli() + 100,
		HasEnd:    true,
		UpdatedMS: openCodeInstant.UnixMilli(),
	}
}

// driveOpenCode runs one walk over the parts given against the store at spool.
func driveOpenCode(t *testing.T, spool string, parts ...opencode.ToolPart) OpenCodeResult {
	t.Helper()
	scan := NewOpenCodeScan(openCodeConsents, opencode.NewServers(nil), opencode.Staleness{}, opencode.Idleness{}, store.New(spool))
	scan.Session(openCodeSession("ses_abc"))
	for _, part := range parts {
		scan.Part(part)
	}
	result, err := scan.Close()
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return result
}

func TestOpenCodeScanPersistsTerminalRecords(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	result := driveOpenCode(t, spool,
		openCodePart("prt_1", "bash", "completed"),
		openCodePart("prt_2", "read", "completed"),
		openCodePart("prt_3", "grep", "error"),
	)
	if result.Parsed != 3 || result.Written != 3 {
		t.Fatalf("parsed = %d, written = %d, want 3 and 3", result.Parsed, result.Written)
	}
	if lines := spoolLines(t, spool); lines != 3 {
		t.Fatalf("spool lines = %d, want 3", lines)
	}
}

func TestOpenCodeScanDeduplicatesARescan(t *testing.T) {
	// The acceptance criterion: scanning the same opencode store twice produces
	// byte-identical store contents. Every id is derived from its source event, so
	// the second walk recognises every record the first wrote (ADR-0004).
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	parts := []opencode.ToolPart{
		openCodePart("prt_1", "bash", "completed"),
		openCodePart("prt_2", "atlassian_search", "completed"),
	}
	first := driveOpenCode(t, spool, parts...)
	if first.Written != 2 {
		t.Fatalf("first walk wrote %d records, want 2", first.Written)
	}
	before, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	second := driveOpenCode(t, spool, parts...)
	if second.Written != 0 || second.Duplicate != 2 {
		t.Fatalf("second walk wrote %d and deduplicated %d, want 0 and 2", second.Written, second.Duplicate)
	}
	after, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("the spool changed on a rescan of the same rows")
	}
}

func TestOpenCodeScanCountsARefusedRecord(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	result := driveOpenCode(t, spool, openCodePart("prt_1", "../etc/passwd", "completed"))
	if result.Refused != 1 || result.Written != 0 {
		t.Fatalf("refused = %d, written = %d, want 1 and 0", result.Refused, result.Written)
	}
	if lines := spoolLines(t, spool); lines != 0 {
		t.Fatalf("spool lines = %d, want none", lines)
	}
}

func TestOpenCodeScanReportsTheReaderCounters(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	scan := NewOpenCodeScan(openCodeConsents, opencode.NewServers(nil), opencode.Staleness{}, opencode.Idleness{}, store.New(spool))
	scan.Session(openCodeSession("ses_abc"))
	scan.Part(openCodePart("prt_1", "bash", "running"))
	scan.Part(openCodePart("prt_2", "bash", "cancelled"))
	if buffered := scan.Buffered(); buffered != 2 {
		t.Fatalf("buffered = %d, want 2", buffered)
	}
	result, err := scan.Close()
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if result.Pending != 1 || result.UnknownOutcomes != 1 {
		t.Fatalf("pending = %d, unknown outcomes = %d, want 1 and 1", result.Pending, result.UnknownOutcomes)
	}
}

func TestAnEmptyOpenCodeWalkCreatesNoSpool(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "events.ndjson")
	scan := NewOpenCodeScan(openCodeConsents, opencode.NewServers(nil), opencode.Staleness{}, opencode.Idleness{}, store.New(spool))
	if _, err := scan.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(spool); err == nil {
		t.Fatal("an empty walk created a spool")
	}
}
