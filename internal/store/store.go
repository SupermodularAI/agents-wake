// Package store persists validated Wake records in an append-only local NDJSON
// spool. The spool is a derived index: deleting it and re-ingesting source
// history is the migration path for any future record-format change.
package store

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/SupermodularAI/agents-wake/internal/lockfile"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

const (
	fileMode fs.FileMode = 0o600
	dirMode  fs.FileMode = 0o700
)

// Store owns one spool and its adjacent lock file.
type Store struct {
	path     string
	lockPath string
}

// Result reports what an append attempt did without exposing source content.
type Result struct {
	Written   int
	Duplicate int
	Dropped   int
}

// Entry is one persisted record and its monotonically increasing write position.
type Entry struct {
	Position uint64
	Record   record.Record
}

// New creates a Store over path. It creates nothing until Append is called.
func New(path string) *Store {
	return &Store{path: path, lockPath: path + ".lock"}
}

// Append validates all supplied records, drops invalid ones, and appends only
// IDs not already in the spool. The lock covers the recovery of an interrupted
// write, the duplicate check and the append, so two ingest processes cannot
// interleave records, double count, or recover the same tail twice.
func (s *Store) Append(records []record.Record) (Result, error) {
	result := Result{}
	valid := make([]record.Record, 0, len(records))
	for _, candidate := range records {
		if err := record.Validate(candidate); err != nil {
			result.Dropped++
			continue
		}
		valid = append(valid, candidate)
	}
	if len(valid) == 0 {
		return result, nil
	}

	if err := os.MkdirAll(filepath.Dir(s.path), dirMode); err != nil {
		return result, fmt.Errorf("creating store directory: %w", err)
	}

	locked := Result{}
	if err := lockfile.WithLock(s.lockPath, func() error {
		var appendErr error
		locked, appendErr = s.appendLocked(valid)
		return appendErr
	}); err != nil {
		// Never a false Written: an append that did not land reports only the
		// records dropped before the lock was taken. A caller that advanced a
		// cursor on a Written it did not get would lose those records for good.
		return Result{Dropped: result.Dropped}, err
	}
	result.Written, result.Duplicate = locked.Written, locked.Duplicate
	result.Dropped += locked.Dropped
	return result, nil
}

// appendLocked does the read-modify-write the caller holds the lock for.
func (s *Store) appendLocked(valid []record.Record) (Result, error) {
	result := Result{}
	if err := s.recoverPartialTail(); err != nil {
		return Result{}, err
	}

	existing, err := s.ids()
	if err != nil {
		return Result{}, err
	}

	var appendData bytes.Buffer
	for _, candidate := range valid {
		if _, found := existing[candidate.EventID]; found {
			result.Duplicate++
			continue
		}
		encoded, marshalErr := record.Marshal(candidate)
		if marshalErr != nil {
			result.Dropped++
			continue
		}
		appendData.Write(encoded)
		appendData.WriteByte('\n')
		existing[candidate.EventID] = struct{}{}
		result.Written++
	}
	if appendData.Len() == 0 {
		return result, nil
	}

	spool, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		return Result{}, fmt.Errorf("opening store: %w", err)
	}
	defer spool.Close()
	if _, err := spool.Write(appendData.Bytes()); err != nil {
		return Result{}, fmt.Errorf("appending store: %w", err)
	}
	if err := spool.Sync(); err != nil {
		return Result{}, fmt.Errorf("syncing store: %w", err)
	}
	return result, nil
}

// recoverPartialTail truncates a final line that no newline terminates, so a new
// record cannot be concatenated onto the remains of an interrupted write. Only
// the unterminated tail is removed and the truncation is synced before anything
// is appended: every preceding complete line stays byte-identical, including one
// that fails record decoding — that is an invalid record, not damage to repair.
func (s *Store) recoverPartialTail() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting store: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}

	file, err := os.OpenFile(s.path, os.O_RDWR, fileMode)
	if err != nil {
		return fmt.Errorf("opening store for recovery: %w", err)
	}
	defer file.Close()

	last := make([]byte, 1)
	if _, readErr := file.ReadAt(last, info.Size()-1); readErr != nil {
		return fmt.Errorf("reading store tail: %w", readErr)
	}
	if last[0] == '\n' {
		return nil
	}
	boundary, err := lastRecordBoundary(file, info.Size())
	if err != nil {
		return err
	}
	if err := file.Truncate(boundary); err != nil {
		return fmt.Errorf("truncating store tail: %w", err)
	}
	return file.Sync()
}

// lastRecordBoundary returns the offset just past the last newline in file, or 0
// when the whole file is one unterminated line. It scans backwards in a bounded
// window so recovery does not depend on the spool fitting in memory.
func lastRecordBoundary(file *os.File, size int64) (int64, error) {
	const window = 64 * 1024
	buffer := make([]byte, window)
	for end := size; end > 0; {
		start := max(end-window, 0)
		chunk := buffer[:end-start]
		if _, err := file.ReadAt(chunk, start); err != nil {
			return 0, fmt.Errorf("scanning store tail: %w", err)
		}
		if index := bytes.LastIndexByte(chunk, '\n'); index >= 0 {
			return start + int64(index) + 1, nil
		}
		end = start
	}
	return 0, nil
}

// Entries returns complete, valid NDJSON records in write order. A trailing
// partial line is ignored so readers remain safe while another process writes.
func (s *Store) Entries(after uint64) ([]Entry, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading store: %w", err)
	}

	lines := bytes.Split(raw, []byte{'\n'})
	entries := make([]Entry, 0, len(lines))
	var position uint64
	for index, line := range lines {
		if index == len(lines)-1 && len(line) > 0 {
			break
		}
		if len(line) == 0 {
			continue
		}
		candidate, err := record.Decode(line)
		if err != nil {
			continue
		}
		position++
		if position > after {
			entries = append(entries, Entry{Position: position, Record: candidate})
		}
	}
	return entries, nil
}

// Head returns the last valid record position.
func (s *Store) Head() (uint64, error) {
	entries, err := s.Entries(0)
	if err != nil || len(entries) == 0 {
		return 0, err
	}
	return entries[len(entries)-1].Position, nil
}

func (s *Store) ids() (map[record.Hash]struct{}, error) {
	entries, err := s.Entries(0)
	if err != nil {
		return nil, err
	}
	ids := make(map[record.Hash]struct{}, len(entries))
	for _, entry := range entries {
		ids[entry.Record.EventID] = struct{}{}
	}
	return ids, nil
}
