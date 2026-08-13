package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

func TestAppendIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	store := New(path)
	event := testRecord("one")

	first, err := store.Append([]record.Record{event})
	if err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	second, err := store.Append([]record.Record{event})
	if err != nil {
		t.Fatalf("second Append() error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if first.Written != 1 || second.Duplicate != 1 || string(before) != string(after) {
		t.Fatalf("Append() results = %+v, %+v; contents changed = %t", first, second, string(before) != string(after))
	}
}

func TestAppendDropsInvalidRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	store := New(path)
	event := testRecord("one")
	event.Name = "invalid name"

	result, err := store.Append([]record.Record{event})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if result.Dropped != 1 || result.Written != 0 {
		t.Fatalf("Append() = %+v, want one dropped record", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("store file exists after dropped record: %v", err)
	}
}

func TestEntriesIgnoreTrailingPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	store := New(path)
	if _, err := store.Append([]record.Record{testRecord("one")}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	if _, writeErr := file.WriteString(`{"schema_version":`); writeErr != nil {
		t.Fatalf("WriteString() error = %v", writeErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}

	entries, err := store.Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Position != 1 {
		t.Fatalf("Entries() = %+v, want one complete entry", entries)
	}
}

func TestConcurrentWritersDoNotDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	stores := []*Store{New(path), New(path)}
	event := testRecord("one")
	var group sync.WaitGroup
	errs := make(chan error, len(stores))
	for _, store := range stores {
		group.Add(1)
		go func(store *Store) {
			defer group.Done()
			_, err := store.Append([]record.Record{event})
			errs <- err
		}(store)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	entries, err := New(path).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Entries() count = %d, want 1", len(entries))
	}
}

func testRecord(source string) record.Record {
	outcome := record.OutcomeOK
	return record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID("claude-code", record.Identifier(source)),
		Timestamp:     time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC),
		Harness:       "claude-code",
		SessionID:     "session-1",
		Repo:          "0123456789abcdef0123456789abcdef",
		Kind:          record.KindSkill,
		Name:          "review",
		Invoker:       record.InvokerModel,
		Outcome:       &outcome,
	}
}
