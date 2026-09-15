// Package sqlitex is the only way this codebase touches SQLite (ADR-0009).
//
// A harness's store belongs to the harness: this package opens it read-only and
// WAL-aware, never writes to it, never holds a lock a running harness needs, and
// never runs an unbounded statement against it. The guards are in guards.go so
// they are policy in one place rather than a constant per caller, and the driver
// import is in driver.go so the confinement is a fact imports_test.go can assert
// over the whole module.
//
// Its errors are three sentinels and nothing else. Driver text is never wrapped
// in and no path is ever formatted into a message, because an error message is
// exactly where the promise that nothing leaves the machine leaks (plan §4.2).
package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// ErrAbsent means there is no store at that location: the harness is not
// installed here, which is "not observed" and never "zero" (ADR-0046).
var ErrAbsent = errors.New("no store")

// ErrUnreadable means a store is present and this build could not read it:
// locked, permission-denied, or a file this driver does not recognise. It is
// blindness — observed, collects nothing (ADR-0009, plan §4.3, §12). It names no
// path and wraps no driver text.
var ErrUnreadable = errors.New("store unreadable")

// ErrRowCap means a scan hit MaxRowsPerScan and stopped. Partial collection, not
// a failure: every id is derived from the source event, so the next scan
// re-derives what this one did not reach (ADR-0004).
var ErrRowCap = errors.New("row cap reached")

// openTimeout bounds the handshake with a store that may be locked by a running
// harness. It is an internal constant rather than a config key: a timeout a user
// can raise is a timeout that stops bounding anything (ADR-0014).
const openTimeout = 5 * time.Second

// Row is one row of a paged result: the single method a caller needs, and the
// only one this package hands out. It exists so a caller can scan a row without
// importing database/sql, which ADR-0009's confinement reserves for this package
// alone — a callback typed *sql.Rows would put that import in every caller and
// make the confinement unassertable.
type Row interface {
	Scan(destination ...any) error
}

// DB is one read-only handle on a harness's store, with the scan budget it is
// allowed to spend.
type DB struct {
	db      *sql.DB
	budget  int
	cleanup func()
}

// Open opens a harness's store read-only and WAL-aware. It never writes to the
// store and never holds a lock a running harness needs: mode=ro plus
// query_only(1), one connection, and a bounded busy timeout.
//
// A store that will not open directly and is small enough is copied and read from
// the copy (ADR-0009's copy-then-read, bounded by CopyThreshold); one above that
// size is reported as blindness instead.
func Open(path string) (*DB, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, ErrAbsent
	case err != nil:
		return nil, ErrUnreadable
	}

	db, err := open(path)
	if err == nil {
		return db, nil
	}
	if info.Size() > CopyThreshold {
		return nil, ErrUnreadable
	}
	return openCopy(path)
}

// open builds the read-only DSN and completes the handshake, or fails. The DSN is
// assembled with net/url so a path containing "?" or "#" cannot smuggle a
// parameter into it — a store path is not attacker-controlled today, and the
// assembly that guarantees it stays harmless costs one line.
func open(path string) (*DB, error) {
	dsn := (&url.URL{
		Scheme:   "file",
		Opaque:   path,
		RawQuery: "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)",
	}).String()

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, ErrUnreadable
	}
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, ErrUnreadable
	}
	// A ping only proves the file opened. A store whose header is not SQLite's
	// fails on the first statement instead, so the handshake asks it one.
	if err := db.QueryRowContext(ctx, "select count(*) from sqlite_schema").Scan(new(int)); err != nil {
		db.Close()
		return nil, ErrUnreadable
	}
	return &DB{db: db, budget: MaxRowsPerScan}, nil
}

// openCopy is ADR-0009's copy-then-read for a small store the harness is holding:
// copy the database and its sidecars into a temporary directory, read the copy,
// and remove it on Close. The copy is never written to either.
func openCopy(path string) (*DB, error) {
	dir, err := os.MkdirTemp("", "wake-store-")
	if err != nil {
		return nil, ErrUnreadable
	}
	target := filepath.Join(dir, filepath.Base(path))
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if copyErr := copyFile(path+suffix, target+suffix); copyErr != nil {
			os.RemoveAll(dir)
			return nil, ErrUnreadable
		}
	}
	db, err := open(target)
	if err != nil {
		os.RemoveAll(dir)
		return nil, ErrUnreadable
	}
	db.cleanup = func() { os.RemoveAll(dir) }
	return db, nil
}

// copyFile copies source to target. A source that does not exist is not an error:
// a database with no -wal sidecar is the normal shape.
func copyFile(source, target string) error {
	in, err := os.Open(source)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Close releases the handle and removes any copy this DB was reading.
func (d *DB) Close() error {
	err := d.db.Close()
	if d.cleanup != nil {
		d.cleanup()
	}
	return err
}

// Page runs one bounded statement and hands each row to scan. limit is clamped to
// MaxRowsPerPage and charged against this DB's MaxRowsPerScan budget; it returns
// the number of rows read, so a caller pages by advancing its own key until a
// short page arrives.
//
// The query is expected to end in "limit ?": the clamped limit is appended to args
// as that statement's last parameter, so the cap is enforced by the statement
// itself rather than by the loop reading it.
func (d *DB) Page(query string, args []any, limit int, scan func(Row) error) (int, error) {
	if d.budget <= 0 {
		return 0, ErrRowCap
	}
	if limit > MaxRowsPerPage {
		limit = MaxRowsPerPage
	}
	if limit > d.budget {
		limit = d.budget
	}
	if limit < 1 {
		limit = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()

	rows, err := d.db.QueryContext(ctx, query, append(append([]any(nil), args...), limit)...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	read := 0
	for rows.Next() {
		read++
		if err := scan(rows); err != nil {
			return read, err
		}
	}
	if err := rows.Err(); err != nil {
		return read, err
	}
	d.budget -= read
	if d.budget <= 0 {
		return read, ErrRowCap
	}
	return read, nil
}
