package store

import (
	"bytes"
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

func TestAppendRecoversAnUnterminatedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	store := New(path)
	if _, err := store.Append([]record.Record{testRecord("one")}); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	appendRaw(t, path, `{"schema_version":`)

	second := testRecord("two")
	result, err := store.Append([]record.Record{second})
	if err != nil {
		t.Fatalf("second Append() error = %v", err)
	}
	if result.Written != 1 || result.Duplicate != 0 || result.Dropped != 0 {
		t.Fatalf("Append() = %+v, want one written record", result)
	}

	entries, err := store.Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 2 || entries[0].Position != 1 || entries[1].Position != 2 {
		t.Fatalf("Entries() = %+v, want two complete entries at positions 1 and 2", entries)
	}

	encoded, err := record.Marshal(second)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if want := string(before) + string(encoded) + "\n"; string(after) != want {
		t.Fatalf("store contents = %q, want %q", after, want)
	}
}

func TestAppendLeavesACompleteButInvalidLineInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	store := New(path)
	if _, err := store.Append([]record.Record{testRecord("one")}); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	const invalid = `{"schema_version":9}`
	appendRaw(t, path, invalid+"\n")

	result, err := store.Append([]record.Record{testRecord("two")})
	if err != nil {
		t.Fatalf("second Append() error = %v", err)
	}
	if result.Written != 1 {
		t.Fatalf("Append() = %+v, want one written record", result)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Contains(raw, []byte(invalid)) {
		t.Errorf("a complete line that fails decoding was rewritten: %q", raw)
	}
	entries, err := store.Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Entries() count = %d, want 2", len(entries))
	}
}

func TestConcurrentAppendersRecoverWithoutDuplicating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	if _, err := New(path).Append([]record.Record{testRecord("one")}); err != nil {
		t.Fatalf("seed Append() error = %v", err)
	}
	appendRaw(t, path, `{"schema_version":`)

	const appenders = 4
	event := testRecord("two")
	results := make(chan Result, appenders)
	errs := make(chan error, appenders)
	var group sync.WaitGroup
	for range appenders {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := New(path).Append([]record.Record{event})
			results <- result
			errs <- err
		}()
	}
	group.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	written, duplicate := 0, 0
	for result := range results {
		written += result.Written
		duplicate += result.Duplicate
	}
	if written != 1 || duplicate != appenders-1 {
		t.Errorf("written = %d, duplicate = %d; want 1 and %d", written, duplicate, appenders-1)
	}
	entries, err := New(path).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Entries() count = %d, want 2", len(entries))
	}
}

func appendRaw(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
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
