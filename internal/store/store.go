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
	"syscall"

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
// IDs not already in the spool. The lock covers both the duplicate check and
// append so two ingest processes cannot interleave records or double count.
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
	lock, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return result, fmt.Errorf("opening store lock: %w", err)
	}
	defer lock.Close()
	if lockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); lockErr != nil {
		return result, fmt.Errorf("locking store: %w", lockErr)
	}
	defer func() {
		if unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); unlockErr != nil {
			return
		}
	}()

	existing, err := s.ids()
	if err != nil {
		return result, err
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
		return result, fmt.Errorf("opening store: %w", err)
	}
	defer spool.Close()
	if _, err := spool.Write(appendData.Bytes()); err != nil {
		return result, fmt.Errorf("appending store: %w", err)
	}
	if err := spool.Sync(); err != nil {
		return result, fmt.Errorf("syncing store: %w", err)
	}
	return result, nil
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
