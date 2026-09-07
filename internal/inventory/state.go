package inventory

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
	"github.com/SupermodularAI/agents-wake/internal/lockfile"
	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

const (
	// primitiveFileVersion is 2 because the snapshot's row grain changed: a row is
	// now one primitive in one repository, not one primitive. A v1 file is a
	// different shape, not a corrupt one, and Read answers accordingly.
	primitiveFileVersion = 2
	fileMode             = fs.FileMode(0o600)
)

// Usage is a locally available primitive and its activity derived from the
// event spool. Its fields are identifiers, enums, timestamps, and counters only.
//
// Failures and Unknown are raw counts, not a Ratio, for one reason only:
// metrics.Ratio's fields are private, so it cannot round-trip through JSON. A
// renderer that wants a rate calls ErrorRate and never assembles one from these
// counts — this comment used to invite exactly that, and two renderers took the
// invitation and drifted apart (DG-103).
//
// Repo is the salted repository id, never a readable label: it is an identifier
// like every other field here (ADR-0007, ADR-0019 §3). It is empty exactly when
// there are no invocations, because a repository is a property of an invocation
// (ADR-0002) and a primitive nothing invoked has none to name.
type Usage struct {
	Harness     record.Identifier `json:"harness"`
	Kind        record.Kind       `json:"kind"`
	Name        record.Identifier `json:"name"`
	Repo        record.Hash       `json:"repo,omitempty"`
	Invocations uint64            `json:"invocations"`
	Failures    uint64            `json:"failures,omitempty"`
	Unknown     uint64            `json:"unknown,omitempty"`
	LastUsed    time.Time         `json:"last_used,omitempty"`
	// Unmatched marks a row derived from invocations of an MCP server that no
	// discovered config key accounts for: the calls happened, and the server they
	// belong to could not be named from the inventory. It is reported rather than
	// silently rendered as zero, which is the same discipline ADR-0005 applies to an
	// unreported outcome and plan §12 applies to doctor — "collects nothing" is not
	// "collects zero" (plan §3.3, §12).
	//
	// A bool, not a label: the snapshot's fields are identifiers, enums, timestamps
	// and counters, and a bool is the tightest enumeration there is (ADR-0007).
	Unmatched bool `json:"unmatched,omitempty"`
}

// KindLabel is this row's kind as a reader sees it, including the unmatched
// marker. It lives here for ErrorRate's reason: two renderers each deriving a
// display value from raw fields is how the ERRORS cell drifted (DG-103).
func (u Usage) KindLabel() string {
	label := strings.ReplaceAll(string(u.Kind), "_", " ")
	if u.Unmatched {
		label += " (unmatched)"
	}
	return label
}

// ErrorRate is this row's failure rate carrying the population it was measured
// over. It is the exact inverse of the flattening derive performs, and it exists
// so that no renderer reconstructs a rate from raw counts: that is the
// alternative ADR-0006 rejected, and the one that let the ERRORS cell print a
// percentage with no visible denominator (DG-103).
//
// Unknown is excluded from the denominator and reported beside it rather than
// folded into either side: an unreported outcome is not a success (ADR-0005).
//
// It panics for counts valid would refuse, deliberately. NewRatio treats
// impossible counts as a bug in this layer rather than as data (ADR-0006), and
// any snapshot that reached a renderer has already passed valid.
func (u Usage) ErrorRate() metrics.Ratio {
	return metrics.NewRatio(u.Failures, u.Invocations-u.Unknown, u.Unknown, u.Invocations)
}

// EventSource is the read side of the event spool a refresh derives usage from.
// It is an interface so this package depends on the spool's contents and not on
// its storage format; *store.Store implements it.
type EventSource interface {
	Entries(after uint64) ([]store.Entry, error)
}

// Store owns the derived primitive inventory at one local path and the lock that
// serializes a refresh of it.
type Store struct {
	path     string
	lockPath string
}

// New creates a primitive inventory store. It creates nothing until Refresh.
func New(path string) *Store { return &Store{path: path, lockPath: path + ".lock"} }

// Refresh records all currently discovered primitives and their current usage.
// Replacing the snapshot removes primitives no longer found by the harness — but
// only the ones this pass was in a position to look for; see available.
//
// The lock spans the source read, the carry-forward decision and the publication,
// not just the write: publication is already atomic, but a refresh republishes the
// whole snapshot, so one that decided what to publish from an earlier read would
// erase a newer one. Atomicity and isolation are different properties, and this
// needs both.
func (s *Store) Refresh(source EventSource, discovered Discovery) error {
	return lockfile.WithLock(s.lockPath, func() error {
		entries, err := source.Entries(0)
		if err != nil {
			return err
		}
		records := make([]record.Record, 0, len(entries))
		for _, entry := range entries {
			records = append(records, entry.Record)
		}
		return s.write(derive(metrics.Aggregate(records), s.available(discovered), discovered.canonical))
	})
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
//
// A carried entry brings no provenance of its own — it is a name from a previous
// snapshot, not a source — so it can never create a fold; it is only ever folded by
// one the current pass proved.
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
// empty inventory so existing installs gain this state file on their next refresh,
// and so is a snapshot written by another version of this file format: both mean
// there is nothing here this build can read, and neither is a reason to fail a
// command over derived state the next Refresh republishes.
func (s *Store) Read() ([]Usage, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading primitive inventory: %w", err)
	}
	var snapshot primitiveFile
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.RefreshedAt.IsZero() {
		return nil, errors.New("invalid primitive inventory")
	}
	// A snapshot this build does not write is not a corrupt one: the row grain
	// changed, so a file from another version says nothing this build can read. It
	// is derived, regenerable local state — the next Refresh republishes it — so it
	// answers like a missing snapshot rather than failing `wake report`.
	if snapshot.Version != primitiveFileVersion {
		return nil, nil
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

// derive joins the aggregate against what discovery may write. canonical is the
// fold discovery proved between two spellings of one primitive; it is applied to
// both sides of the join, so the folded row exists and the events recorded under
// either spelling land on it.
func derive(summary metrics.Summary, available []Primitive, canonical map[identity]record.Identifier) []Usage {
	observed := make(map[usageKey]Usage)
	// repos is which repositories observed each identity, in first-seen order. The
	// join below is on the identity — discovery has no repository to join on
	// (ADR-0002) — while the rows it produces are per repository.
	repos := make(map[identity][]record.Hash)
	// accumulate sums one aggregate row onto one identity's row for one repository.
	// It is shared by the primitive's own row and by the MCP server roll-up below,
	// so a server's counts are the same arithmetic its tools' are and the ratio
	// invariants Usage.valid asserts hold on both.
	accumulate := func(id identity, primitive metrics.PrimitiveUsage) {
		key := usageKey{identity: id, repo: primitive.Repo}
		usage, seen := observed[key]
		if !seen {
			repos[id] = append(repos[id], primitive.Repo)
		}
		usage.Harness, usage.Kind, usage.Name, usage.Repo = id.harness, id.kind, id.name, primitive.Repo
		usage.Invocations += primitive.Invocations
		usage.Failures += primitive.ErrorRate.Numerator()
		usage.Unknown += primitive.ErrorRate.Excluded()
		if primitive.LastUsed.After(usage.LastUsed) {
			usage.LastUsed = primitive.LastUsed
		}
		observed[key] = usage
	}

	index, discovered := serverIndex(available)
	// unnamed is which server rows this pass built from invocations of a server no
	// discovered config key accounts for, so they can still be published below.
	// Every other kind is published from the available side alone; an MCP server is
	// the one case where an observed row with no discovered counterpart is the
	// answer rather than noise, because the calls provably happened.
	unnamed := map[usageKey]struct{}{}

	for _, primitive := range summary.Primitives {
		if primitive.Kind == record.KindBuiltinTool {
			continue
		}
		accumulate(canonicalIdentity(canonical, identity{harness: primitive.Harness, kind: primitive.Kind, name: primitive.Name}), primitive)
		if primitive.Kind != record.KindMCPTool || primitive.MCPServer == "" {
			continue
		}
		// Deliberately not through canonicalIdentity: that fold never touches kind,
		// because a fold whose target belongs to another kind was refused at
		// discovery (DG-106). This roll-up is cross-kind by construction — mcp_tool
		// events onto an mcp_server row — so it is its own path and leaves DG-106's
		// guarantee exactly as it was.
		server := identity{harness: primitive.Harness, kind: record.KindMCPServer, name: serverName(index, primitive.MCPServer)}
		accumulate(server, primitive)
		if _, found := discovered[server.name]; !found {
			unnamed[usageKey{identity: server, repo: primitive.Repo}] = struct{}{}
		}
	}

	items := make(map[usageKey]Usage, len(available))
	for _, primitive := range available {
		if primitive.Kind == record.KindBuiltinTool {
			continue
		}
		id := canonicalIdentity(canonical, identity{harness: primitive.Harness, kind: primitive.Kind, name: primitive.Name})
		seen := repos[id]
		if len(seen) == 0 {
			// Nothing invoked it, so it has no repository to name and belongs to the
			// inventory grain alone.
			usage := Usage{Harness: id.harness, Kind: id.kind, Name: id.name}
			// Fail closed: a name the record contract would refuse must not reach the
			// snapshot either, whatever a caller handed us (ADR-0007, plan §3.4).
			if !usage.valid() {
				continue
			}
			items[usageKey{identity: id}] = usage
			continue
		}
		for _, repo := range seen {
			key := usageKey{identity: id, repo: repo}
			usage := observed[key]
			usage.Harness, usage.Kind, usage.Name, usage.Repo = id.harness, id.kind, id.name, repo
			if !usage.valid() {
				continue
			}
			items[key] = usage
		}
	}
	// The observed server rows the available side could not publish, because no
	// discovered config key names them. Published last and flagged, so `--unused` is
	// not the only place a heavily used server shows up (plan §3.3, §12).
	for key := range unnamed {
		if _, published := items[key]; published {
			continue
		}
		usage := observed[key]
		usage.Unmatched = true
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
		return cmp.Or(cmp.Compare(string(left.Harness), string(right.Harness)), cmp.Compare(string(left.Kind), string(right.Kind)), cmp.Compare(string(left.Name), string(right.Name)), cmp.Compare(string(left.Repo), string(right.Repo)))
	})
	return result
}

// identity is a primitive as discovery knows it: the inventory grain, which
// carries no repository (ADR-0002). derive joins on this.
type identity struct {
	harness record.Identifier
	kind    record.Kind
	name    record.Identifier
}

// canonicalIdentity applies the fold discovery proved for one primitive: the two
// discovered spellings of one skill collapse onto one row, and usage recorded under
// either spelling accumulates there.
//
// Both sides of the join go through it. Folding discovery alone would leave an event
// recorded under the bare spelling with no row to land on and drop it from the
// snapshot — the opposite of what the fold is for. The kind is never touched: a fold
// whose target belongs to another kind was refused at discovery, so nothing here has
// to pick one (ADR-0005).
func canonicalIdentity(canonical map[identity]record.Identifier, id identity) identity {
	if name, folded := canonical[id]; folded {
		id.name = name
	}
	return id
}

// usageKey is one snapshot row: an identity in one repository, or — for a
// primitive with no invocations — in none.
type usageKey struct {
	identity
	repo record.Hash
}

func (s *Store) write(primitives []Usage) error {
	snapshot := primitiveFile{Version: primitiveFileVersion, RefreshedAt: time.Now().UTC(), Primitives: primitives}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding primitive inventory: %w", err)
	}
	raw = append(raw, '\n')
	if err := atomicfile.Publish(s.path, raw, fileMode); err != nil {
		return fmt.Errorf("publishing primitive inventory: %w", err)
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
	// Unmatched only ever describes an observed MCP server: a row of another kind,
	// or one with no invocations, cannot be unmatched, and a snapshot claiming
	// otherwise is refused rather than repaired (fail closed, plan §3.4).
	if u.Unmatched && (u.Kind != record.KindMCPServer || u.Invocations == 0) {
		return false
	}
	if u.Unknown > u.Invocations || u.Failures > u.Invocations-u.Unknown {
		return false
	}
	// Repo belongs to the invocation grain (ADR-0002): a row with invocations
	// carries the salted id they were recorded under, and a row with none has no
	// repository to name. Validated to the record contract's own rule so the
	// snapshot cannot hold a repository field a record could not (ADR-0007).
	if (u.Repo == "") != (u.Invocations == 0) {
		return false
	}
	if u.Repo != "" && !record.ValidRepo(u.Repo) {
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
