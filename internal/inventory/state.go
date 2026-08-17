package inventory

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

const (
	primitiveFileVersion = 1
	fileMode             = fs.FileMode(0o600)
	dirMode              = fs.FileMode(0o700)
)

// Usage is a locally available primitive and its activity derived from the
// event spool. Its fields are identifiers, enums, timestamps, and counters only.
type Usage struct {
	Harness     record.Identifier `json:"harness"`
	Kind        record.Kind       `json:"kind"`
	Name        record.Identifier `json:"name"`
	Invocations uint64            `json:"invocations"`
	LastUsed    time.Time         `json:"last_used,omitempty"`
}

// Store owns the derived primitive inventory at one local path.
type Store struct{ path string }

// New creates a primitive inventory store. It creates nothing until Refresh.
func New(path string) *Store { return &Store{path: path} }

// Refresh records all currently discovered primitives and their current usage.
// Replacing the snapshot removes primitives no longer found by the harness — but
// only the ones this pass was in a position to look for; see available.
func (s *Store) Refresh(source *store.Store, discovered Discovery) error {
	entries, err := source.Entries(0)
	if err != nil {
		return err
	}
	records := make([]record.Record, 0, len(entries))
	for _, entry := range entries {
		records = append(records, entry.Record)
	}
	return s.write(derive(metrics.Aggregate(records), s.available(discovered)))
}

// available is what this pass may write: everything it discovered, plus — when
// project-local discovery was withheld — everything the previous snapshot held.
//
// A pass that was not allowed to look inside a consented repository has not
// learned that its primitives are gone. Replacing the snapshot from that partial
// view would erase them and their counters from the report and the dashboard until
// a command happened to run inside that repository again, which is why the ticket
// asks that an unconsented scan never replace an existing snapshot.
//
// Carrying an entry does not freeze it: every counter is re-derived from the event
// store below, for carried and discovered entries alike, and the event store is
// already consent-gated. Nothing about the withheld directory is read or written —
// what is carried is a name Wake had already recorded.
//
// A previous snapshot Read refuses is not carried. Fail closed: bytes that do not
// validate are not worth preserving (plan §3.4).
func (s *Store) available(discovered Discovery) []Primitive {
	if discovered.ProjectScanned {
		return discovered.Primitives
	}
	previous, err := s.Read()
	if err != nil {
		return discovered.Primitives
	}
	carried := slices.Clone(discovered.Primitives)
	for _, usage := range previous {
		carried = append(carried, Primitive{Harness: usage.Harness, Kind: usage.Kind, Name: usage.Name})
	}
	return carried
}

// Read returns the last successful inventory snapshot. A missing snapshot is an
// empty inventory so existing installs gain this state file on their next refresh.
func (s *Store) Read() ([]Usage, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading primitive inventory: %w", err)
	}
	var snapshot primitiveFile
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Version != primitiveFileVersion || snapshot.RefreshedAt.IsZero() {
		return nil, errors.New("invalid primitive inventory")
	}
	for _, usage := range snapshot.Primitives {
		if !usage.valid() {
			return nil, errors.New("invalid primitive inventory")
		}
	}
	return snapshot.Primitives, nil
}

type primitiveFile struct {
	Version     int       `json:"version"`
	RefreshedAt time.Time `json:"refreshed_at"`
	Primitives  []Usage   `json:"primitives"`
}

func derive(summary metrics.Summary, available []Primitive) []Usage {
	observed := make(map[usageKey]Usage)
	for _, primitive := range summary.Primitives {
		if primitive.Kind == record.KindBuiltinTool {
			continue
		}
		key := usageKey{harness: primitive.Harness, kind: primitive.Kind, name: primitive.Name}
		usage := observed[key]
		usage.Harness, usage.Kind, usage.Name = primitive.Harness, primitive.Kind, primitive.Name
		usage.Invocations += primitive.Invocations
		if primitive.LastUsed.After(usage.LastUsed) {
			usage.LastUsed = primitive.LastUsed
		}
		observed[key] = usage
	}

	items := make(map[usageKey]Usage, len(available))
	for _, primitive := range available {
		if primitive.Kind == record.KindBuiltinTool {
			continue
		}
		key := usageKey{harness: primitive.Harness, kind: primitive.Kind, name: primitive.Name}
		usage := observed[key]
		usage.Harness, usage.Kind, usage.Name = primitive.Harness, primitive.Kind, primitive.Name
		// Fail closed: a name the record contract would refuse must not reach the
		// snapshot either, whatever a caller handed us (ADR-0007, plan §3.4).
		if !usage.valid() {
			continue
		}
		items[key] = usage
	}
	result := make([]Usage, 0, len(items))
	for _, usage := range items {
		result = append(result, usage)
	}
	slices.SortFunc(result, func(left, right Usage) int {
		if left.Invocations != right.Invocations {
			return -cmp.Compare(left.Invocations, right.Invocations)
		}
		return cmp.Or(cmp.Compare(string(left.Harness), string(right.Harness)), cmp.Compare(string(left.Kind), string(right.Kind)), cmp.Compare(string(left.Name), string(right.Name)))
	})
	return result
}

type usageKey struct {
	harness record.Identifier
	kind    record.Kind
	name    record.Identifier
}

func (s *Store) write(primitives []Usage) error {
	snapshot := primitiveFile{Version: primitiveFileVersion, RefreshedAt: time.Now().UTC(), Primitives: primitives}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding primitive inventory: %w", err)
	}
	raw = append(raw, '\n')
	if makeErr := os.MkdirAll(filepath.Dir(s.path), dirMode); makeErr != nil {
		return fmt.Errorf("creating primitive inventory directory: %w", makeErr)
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), "primitives-*.json")
	if err != nil {
		return fmt.Errorf("creating primitive inventory: %w", err)
	}
	temporary := file.Name()
	if _, err := file.Write(raw); err != nil {
		return errors.Join(fmt.Errorf("writing primitive inventory: %w", err), file.Close(), os.Remove(temporary))
	}
	if err := file.Chmod(fileMode); err != nil {
		return errors.Join(fmt.Errorf("setting primitive inventory mode: %w", err), file.Close(), os.Remove(temporary))
	}
	if err := file.Close(); err != nil {
		return errors.Join(fmt.Errorf("closing primitive inventory: %w", err), os.Remove(temporary))
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return errors.Join(fmt.Errorf("replacing primitive inventory: %w", err), os.Remove(temporary))
	}
	return nil
}

func (u Usage) valid() bool {
	if !record.ValidHarness(u.Harness) || !record.ValidName(u.Name) {
		return false
	}
	if !validKind(u.Kind) {
		return false
	}
	return (u.Invocations == 0 && u.LastUsed.IsZero()) || (u.Invocations > 0 && !u.LastUsed.IsZero())
}

func validKind(kind record.Kind) bool {
	switch kind {
	case record.KindSkill, record.KindSubagent, record.KindMCPTool, record.KindMCPServer, record.KindCommand, record.KindPlugin, record.KindHook:
		return true
	default:
		return false
	}
}
