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
	"sync"

	"github.com/SupermodularAI/agents-wake/internal/lockfile"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

const (
	fileMode fs.FileMode = 0o600
	dirMode  fs.FileMode = 0o700
)

// Store owns one spool and its adjacent lock file.
//
// A *Store is safe to append from more than one goroutine: mu serialises the
// read-modify-write inside this process, and the lock file serialises it against
// other processes. Sharing one value is also how an ingest run pays for its dedup
// index once — see the index fields below — so callers are expected to reuse one
// *Store for a whole scan rather than construct one per transcript.
type Store struct {
	path     string
	lockPath string

	// mu guards index, indexedTo and indexed. The lock file cannot stand in for it:
	// a file lock creates no happens-before edge the race detector can see.
	mu sync.Mutex
	// index is the set of event ids the spool already holds, and indexedTo is the
	// spool offset it covers — always a line boundary. A refresh decodes only
	// [indexedTo, size), which is what keeps an N-transcript scan from decoding the
	// whole spool N times. indexed is the spool as it was when the index was last
	// refreshed, so a spool that was replaced or truncated is noticed.
	//
	// The index is an optimisation and never the correctness mechanism (ADR-0015):
	// event ids are derived from the source event (ADR-0004), so discarding this
	// state costs one full re-decode of the spool and never a different store.
	index     map[record.Hash]struct{}
	indexedTo int64
	indexed   fs.FileInfo
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
	// mu is always taken before the spool lock, on this one code path, so no
	// deadlock is reachable; blocking on the spool lock while holding mu only
	// serialises appends on one *Store, which is what the store already did.
	s.mu.Lock()
	defer s.mu.Unlock()
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

	if err := s.refreshIndex(); err != nil {
		return Result{}, err
	}

	var appendData bytes.Buffer
	// Buffered rather than merged into s.index as we go: an id cached for a record
	// the spool does not hold would make a later Append report it as a duplicate and
	// never write it. They are merged only once the bytes are on disk and measured.
	added := make([]record.Hash, 0, len(valid))
	for _, candidate := range valid {
		if _, found := s.index[candidate.EventID]; found {
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
		// Cached immediately so duplicates within one batch still collapse; recorded
		// in added so they can be un-cached if the write never lands.
		s.index[candidate.EventID] = struct{}{}
		added = append(added, candidate.EventID)
		result.Written++
	}
	if appendData.Len() == 0 {
		return result, nil
	}

	spool, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		s.rollbackIndex(added)
		return Result{}, fmt.Errorf("opening store: %w", err)
	}
	defer spool.Close()
	// A failed Write or Sync may still have put bytes on disk, so those two paths
	// drop the whole cache and rebuild from the file rather than un-caching ids that
	// may well be there.
	if _, err := spool.Write(appendData.Bytes()); err != nil {
		s.dropIndex()
		return Result{}, fmt.Errorf("appending store: %w", err)
	}
	if err := spool.Sync(); err != nil {
		s.dropIndex()
		return Result{}, fmt.Errorf("syncing store: %w", err)
	}
	// The index advances only on a spool that grew by exactly what was written.
	// Anything else — a short write, a stat that fails — drops the cache, which
	// costs the next append one full decode and never a wrong answer. The records
	// did land, so this still reports result, not an error.
	info, statErr := spool.Stat()
	if statErr != nil || info.Size() != s.indexedTo+int64(appendData.Len()) {
		s.dropIndex()
		return result, nil
	}
	s.indexedTo, s.indexed = info.Size(), info
	return result, nil
}

// refreshIndex brings the dedup index up to the spool's current length, decoding
// only the bytes appended since the last refresh. The caller holds both mu and the
// spool lock, so the file cannot change under it, and recoverPartialTail has
// already removed any unterminated tail.
//
// It re-reads from disk on every append deliberately: the index may be stale with
// respect to another process that appended since, and a stale index produces a
// duplicate record rather than a slow scan (ADR-0004).
func (s *Store) refreshIndex() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		// No spool: `wake rebuild` removes it, so anything cached describes a file
		// that is gone and every id has to be written again.
		s.index, s.indexedTo, s.indexed = make(map[record.Hash]struct{}), 0, nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting store: %w", err)
	}
	if s.index == nil || s.indexed == nil || !os.SameFile(s.indexed, info) || info.Size() < s.indexedTo {
		s.index, s.indexedTo = make(map[record.Hash]struct{}), 0
	}
	s.indexed = info
	if info.Size() == s.indexedTo {
		return nil
	}

	file, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer file.Close()
	tail := make([]byte, info.Size()-s.indexedTo)
	// ReadAt, like recoverPartialTail's, so only the new bytes are ever in memory —
	// the whole point of the change is not reading the spool again.
	if _, err := file.ReadAt(tail, s.indexedTo); err != nil {
		return fmt.Errorf("reading store: %w", err)
	}
	// Complete lines only, exactly as Entries does: a trailing partial line belongs
	// to nobody yet and indexing it would cache an id for a record that is not there.
	end := bytes.LastIndexByte(tail, '\n')
	if end < 0 {
		return nil
	}
	for _, line := range bytes.Split(tail[:end], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		candidate, decodeErr := record.Decode(line)
		if decodeErr != nil {
			// A complete line that does not decode is an invalid record, not damage:
			// it stays on disk untouched and contributes no id, which is what reading
			// the spool through Entries did before.
			continue
		}
		s.index[candidate.EventID] = struct{}{}
	}
	s.indexedTo += int64(end) + 1
	return nil
}

// dropIndex discards the cached index. The next append rebuilds it from the spool:
// slower, never different (ADR-0015).
func (s *Store) dropIndex() {
	s.index, s.indexedTo, s.indexed = nil, 0, nil
}

// rollbackIndex un-caches ids whose bytes never reached the spool.
func (s *Store) rollbackIndex(added []record.Hash) {
	for _, id := range added {
		delete(s.index, id)
	}
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
