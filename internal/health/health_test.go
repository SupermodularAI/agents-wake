package health

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecordScanKeepsTheHookCounters(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "health.json"))
	if err := store.RecordHooks(Hooks{At: time.Now().UTC(), Installed: 2}); err != nil {
		t.Fatalf("RecordHooks() error = %v", err)
	}

	if err := store.RecordScan(Scan{At: time.Now().UTC(), Transcripts: 7}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	got, err := store.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Hooks.Installed != 2 {
		t.Errorf("Hooks.Installed = %d, want 2 — a scan must not erase what init recorded", got.Hooks.Installed)
	}
	if got.Scan.Transcripts != 7 {
		t.Errorf("Scan.Transcripts = %d, want 7", got.Scan.Transcripts)
	}
}

func TestRecordHooksKeepsTheScanCounters(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "health.json"))
	if err := store.RecordScan(Scan{At: time.Now().UTC(), EventsWritten: 5}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	if err := store.RecordHooks(Hooks{At: time.Now().UTC(), Removed: 1}); err != nil {
		t.Fatalf("RecordHooks() error = %v", err)
	}

	got, err := store.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Scan.EventsWritten != 5 {
		t.Errorf("Scan.EventsWritten = %d, want 5 — a hook change is not a scan", got.Scan.EventsWritten)
	}
	if got.Hooks.Removed != 1 {
		t.Errorf("Hooks.Removed = %d, want 1", got.Hooks.Removed)
	}
}

// A fresh install has no counter file, and doctor renders that as "never
// scanned". Reading must not create one: a diagnostic that wrote state would be
// changing what it reports.
func TestReadOfAMissingFileIsAZeroReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "health.json")

	got, err := New(path).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if got != (Report{}) {
		t.Errorf("Read() = %+v, want the zero Report", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Read() created %d entries, want none", len(entries))
	}
}

// A counter file this build cannot read is an error, not all-zeroes: reporting a
// corrupt file as zero would render "collects zero" for a state nobody measured,
// which is the exact distinction this package exists to draw.
func TestReadRejectsAnUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := New(path).Read(); err == nil {
		t.Fatal("Read() error = nil, want a refusal for an unknown version")
	}
}

func TestReportIsWrittenAt0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	if err := New(path).RecordScan(Scan{At: time.Now().UTC()}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600", perm)
	}
}

// The file is republished whole, so a scan and a hook change recording at the same
// time is the shape that loses one. A trigger firing while the user runs `wake
// remove` is exactly that.
func TestConcurrentRecordsDoNotLoseEachOther(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "health.json"))

	var writers sync.WaitGroup
	failures := make(chan error, 8)
	for i := range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			var err error
			if i%2 == 0 {
				err = store.RecordScan(Scan{At: time.Now().UTC(), Transcripts: 1})
			} else {
				err = store.RecordHooks(Hooks{At: time.Now().UTC(), Installed: 1})
			}
			if err != nil {
				failures <- err
			}
		}()
	}
	writers.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("record error = %v", err)
	}

	got, err := store.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Scan.Transcripts == 0 {
		t.Error("Scan.Transcripts = 0; a hook record erased every scan record")
	}
	if got.Hooks.Installed == 0 {
		t.Error("Hooks.Installed = 0; a scan record erased every hook record")
	}
}

// ADR-0007's "the type is the allowlist", applied to diagnostics. doctor output is
// what people paste into issues, so a counter carries a count and never a line, a
// path or a label — and the way to guarantee that is a struct with no field that
// could hold one.
func TestEveryCounterFieldIsACountOrATime(t *testing.T) {
	// The type check above is the guarantee; this list is a second tripwire against
	// a name that reads like free text even where the type would already refuse it.
	// "error" is deliberately absent: ParseErrors is a count of errors, and a count
	// of something is not that something.
	forbidden := []string{"path", "root", "label", "dir", "cwd", "name", "line", "message", "reason"}

	for _, sample := range []any{Report{}, Scan{}, Hooks{}} {
		typ := reflect.TypeOf(sample)
		t.Run(typ.Name(), func(t *testing.T) {
			for i := range typ.NumField() {
				field := typ.Field(i)
				switch field.Type.Kind() {
				case reflect.Int:
				case reflect.Struct:
					if name := field.Type.Name(); name != "Time" && name != "Scan" && name != "Hooks" {
						t.Errorf("field %s is a %s; only a time or another counter section is allowed", field.Name, name)
					}
				default:
					t.Errorf("field %s is a %s; a counter is an int or a time", field.Name, field.Type.Kind())
				}
				for _, word := range forbidden {
					if strings.Contains(strings.ToLower(field.Name), word) {
						t.Errorf("field %s is named after %q; a counter never carries one", field.Name, word)
					}
				}
			}
		})
	}
}
