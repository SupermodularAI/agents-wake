package sqlitex

import (
	"database/sql"
	"errors"
	"fmt"
)

// CreateFixture builds a SQLite database at path by executing statements against
// it, and is for tests only. It lives here, in a shipped file rather than a
// _test.go, because ADR-0009 makes this package "the only way this codebase
// touches SQLite" and a test elsewhere opening its own handle would be a second
// way. Keeping it here lets imports_test.go assert the driver import is absent
// from every other file in the module, test files included — a rule that would
// otherwise have a hole exactly where a hostile fixture would be written.
//
// It is the one write path this package has, it reaches no command (asserted by
// TestNoCommandPathCreatesAStore), and it is deliberately tiny: open, set WAL,
// execute, close.
func CreateFixture(path string, statements ...string) error {
	db, err := sql.Open(driverName, "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("opening fixture: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// The real store is WAL, so a fixture that was not would test a shape no
	// machine has.
	if _, err := db.Exec("pragma journal_mode=wal"); err != nil {
		return fmt.Errorf("setting journal mode: %w", err)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("executing fixture statement: %w", err)
		}
	}
	return errors.Join(db.Close())
}
