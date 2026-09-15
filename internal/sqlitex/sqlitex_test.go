package sqlitex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture builds a store under t.TempDir() through CreateFixture, which is the
// only write path this package has and the only one any test in this module may
// use: imports_test.go asserts no other file anywhere opens a SQLite handle, so a
// test that built its own would be the second way ADR-0009 forbids.
func fixture(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	if err := CreateFixture(path, statements...); err != nil {
		t.Fatalf("creating fixture: %v", err)
	}
	return path
}

func TestOpenOfAMissingFileIsAbsent(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "nothing.db"))
	if !errors.Is(err, ErrAbsent) {
		t.Fatalf("Open of a missing file = %v, want ErrAbsent", err)
	}
}

func TestOpenOfAFileThatIsNotADatabaseIsUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("not a database", 8)), 0o600); err != nil {
		t.Fatalf("writing decoy: %v", err)
	}
	_, err := Open(path)
	if !errors.Is(err, ErrUnreadable) {
		t.Fatalf("Open of a non-database = %v, want ErrUnreadable", err)
	}
}

func TestOpenIsReadOnly(t *testing.T) {
	path := fixture(t, "create table t (n integer)", "insert into t values (1)")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Page("insert into t values (2)", nil, 10, func(Row) error { return nil }); err == nil {
		t.Fatal("a write through a read-only handle succeeded")
	}
	count := 0
	if _, err := db.Page("select n from t", nil, 10, func(row Row) error {
		count++
		var n int64
		return row.Scan(&n)
	}); err != nil {
		t.Fatalf("Page: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows after the refused write = %d, want 1", count)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOpenReadsAWALDatabase(t *testing.T) {
	// The real store is journal_mode=wal, so a read path that only worked on a
	// rollback-journal database would collect nothing on every machine that has
	// opencode. CreateFixture puts the file in WAL mode; the mode is persisted in
	// the file itself, which is what mode=ro has to cope with.
	path := fixture(t, "create table t (n integer)", "insert into t values (1)", "insert into t values (2)")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	mode := ""
	if _, err := db.Page("pragma journal_mode", nil, 1, func(row Row) error { return row.Scan(&mode) }); err != nil {
		t.Fatalf("reading journal mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal mode = %q, want wal", mode)
	}
	rows := 0
	if _, err := db.Page("select n from t order by n", nil, 10, func(Row) error { rows++; return nil }); err != nil {
		t.Fatalf("Page: %v", err)
	}
	if rows != 2 {
		t.Fatalf("rows from a WAL database = %d, want 2", rows)
	}
}

func TestPageClampsToTheRowCap(t *testing.T) {
	path := fixture(t,
		"create table t (n integer)",
		"with recursive s(n) as (select 1 union all select n+1 from s where n < 1200) insert into t select n from s",
	)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	seen := 0
	read, err := db.Page("select n from t order by n limit ?", nil, 10_000, func(Row) error { seen++; return nil })
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if seen != MaxRowsPerPage || read != MaxRowsPerPage {
		t.Fatalf("Page read %d rows (callback ran %d times), want %d", read, seen, MaxRowsPerPage)
	}
}

func TestScanBudgetStops(t *testing.T) {
	path := fixture(t,
		"create table t (n integer)",
		"with recursive s(n) as (select 1 union all select n+1 from s where n < 500) insert into t select n from s",
	)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	total := 0
	for {
		read, err := db.Page("select n from t order by n limit ?", nil, MaxRowsPerPage, func(Row) error { return nil })
		total += read
		if errors.Is(err, ErrRowCap) {
			break
		}
		if err != nil {
			t.Fatalf("Page: %v", err)
		}
		if total > MaxRowsPerScan {
			t.Fatalf("read %d rows without ErrRowCap, budget is %d", total, MaxRowsPerScan)
		}
	}
	if total != MaxRowsPerScan {
		t.Fatalf("rows read before the cap = %d, want %d", total, MaxRowsPerScan)
	}
}

func TestErrorsCarryNoPathOrContent(t *testing.T) {
	// An error message is where this promise leaks (plan §4.2): no sentinel may
	// name the store it came from, so none may contain a path separator at all.
	for _, err := range []error{ErrAbsent, ErrUnreadable, ErrRowCap} {
		if strings.Contains(err.Error(), "/") {
			t.Errorf("%v carries a path separator", err)
		}
	}
	dir := t.TempDir()
	if _, err := Open(filepath.Join(dir, "nothing.db")); err == nil || strings.Contains(err.Error(), dir) {
		t.Errorf("Open's absent error = %v, which names its path", err)
	}
	decoy := filepath.Join(dir, "decoy.db")
	if err := os.WriteFile(decoy, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("writing decoy: %v", err)
	}
	if _, err := Open(decoy); err == nil || strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "not a database") {
		t.Errorf("Open's unreadable error = %v, which names its path or its content", err)
	}
}
