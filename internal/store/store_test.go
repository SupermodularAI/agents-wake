package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
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

// A second Store value over the same spool stands in for a second process: its
// cached index must be refreshed under the lock against what the first one wrote,
// or ADR-0004's "same logical event twice is a no-op" becomes a duplicate record.
func TestAppendSeesRecordsWrittenByAnotherStoreValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	first, second := New(path), New(path)

	if _, err := first.Append([]record.Record{testRecord("one")}); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	// second builds its index here, covering only record one.
	if result, err := second.Append([]record.Record{testRecord("one")}); err != nil || result.Duplicate != 1 {
		t.Fatalf("second Append() = %+v, %v; want one duplicate", result, err)
	}
	// first writes again behind second's back; second must still see it.
	if _, err := first.Append([]record.Record{testRecord("two")}); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	result, err := second.Append([]record.Record{testRecord("two")})
	if err != nil || result.Duplicate != 1 || result.Written != 0 {
		t.Fatalf("second Append() = %+v, %v; want one duplicate", result, err)
	}

	entries, err := New(path).Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Entries() count = %d, want 2", len(entries))
	}
}

// Rebuild removes the spool (activation.Rebuild) and a long-lived Store value must
// not answer from an index describing a file that no longer exists.
func TestAppendRewritesRecordsAfterTheSpoolIsRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	store := New(path)
	event := testRecord("one")
	if _, err := store.Append([]record.Record{event}); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	result, err := store.Append([]record.Record{event})
	if err != nil || result.Written != 1 || result.Duplicate != 0 {
		t.Fatalf("Append() = %+v, %v; want one written record", result, err)
	}
	entries, err := store.Entries(0)
	if err != nil {
		t.Fatalf("Entries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Entries() count = %d, want 1", len(entries))
	}
}

// One Store value appended from several goroutines: the spool lock serialises the
// write, but only an in-process mutex gives the race detector the happens-before
// edge a file lock cannot express (T120 acceptance: `go test ./... -race`).
func TestConcurrentAppendsOnOneStoreValueDoNotDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	store := New(path)
	event := testRecord("one")

	const appenders = 4
	results := make(chan Result, appenders)
	errs := make(chan error, appenders)
	var group sync.WaitGroup
	for range appenders {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := store.Append([]record.Record{event})
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
	if len(entries) != 1 {
		t.Fatalf("Entries() count = %d, want 1", len(entries))
	}
}

// The index must cover the whole spool after every Append: the refresh reads only
// the bytes from indexedTo to end-of-file, so an offset equal to the file size is
// the proof that the next Append re-decodes nothing (T120).
func TestAppendIndexesTheSpoolTailOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	store := New(path)

	if _, err := store.Append([]record.Record{testRecord("one"), testRecord("two")}); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	assertIndexCoversSpool(t, store, path, 2)

	if _, err := store.Append([]record.Record{testRecord("three")}); err != nil {
		t.Fatalf("second Append() error = %v", err)
	}
	assertIndexCoversSpool(t, store, path, 3)

	// A line another writer appended is picked up by the tail refresh, not by a
	// second full decode: the offset still ends at the file size afterwards.
	encoded, err := record.Marshal(testRecord("four"))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	appendRaw(t, path, string(encoded)+"\n")

	result, err := store.Append([]record.Record{testRecord("four")})
	if err != nil || result.Duplicate != 1 || result.Written != 0 {
		t.Fatalf("third Append() = %+v, %v; want one duplicate", result, err)
	}
	assertIndexCoversSpool(t, store, path, 4)
}

func assertIndexCoversSpool(t *testing.T, store *Store, path string, wantIDs int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if store.indexedTo != info.Size() {
		t.Errorf("indexedTo = %d, want the spool size %d — the next Append would re-decode", store.indexedTo, info.Size())
	}
	if len(store.index) != wantIDs {
		t.Errorf("index size = %d, want %d", len(store.index), wantIDs)
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

// benchRecord builds a record whose derived event id is unique per index, so a
// benchmark append is always a real write rather than a duplicate check.
func benchRecord(index int) record.Record {
	event := testRecord("bench")
	event.EventID = record.DeriveEventID("claude-code", record.Identifier("bench-"+strconv.Itoa(index)))
	return event
}

// BenchmarkAppendIntoLargeSpool is the measurement T120's acceptance asks for: one
// Append of a single record into a spool that already holds thousands. Before the
// incremental index it decoded the whole spool every iteration; after, it decodes
// only what another writer appended since — normally nothing.
//
// It runs at two spool sizes on purpose. Absolute ns/op is dominated by a fixed
// per-append floor this ticket does not touch (the lock file, three opens and an
// fsync — ~4 ms on the machine this was written on), so the figure that shows the
// defect is fixed is how the cost moves with the stored event count: roughly linear
// in it before, flat after.
func BenchmarkAppendIntoLargeSpool(b *testing.B) {
	for _, seeded := range []int{5000, 20000} {
		b.Run(strconv.Itoa(seeded)+"-events", func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "events.ndjson")
			store := New(path)
			seed := make([]record.Record, 0, seeded)
			for index := range seeded {
				seed = append(seed, benchRecord(index))
			}
			if _, err := store.Append(seed); err != nil {
				b.Fatalf("seed Append() error = %v", err)
			}

			index := 0
			for b.Loop() {
				if _, err := store.Append([]record.Record{benchRecord(seeded + index)}); err != nil {
					b.Fatalf("Append() error = %v", err)
				}
				index++
			}
		})
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
