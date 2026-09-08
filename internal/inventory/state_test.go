package inventory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/lockfile"
	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

func TestRefreshPersistsDiscoveredPrimitivesAndCurrentUsage(t *testing.T) {
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	first := inventoryRecord("first", "used", time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC))
	if _, err := events.Append([]record.Record{first}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	primitives := New(statePath)
	available := []Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "used"}, {Harness: "claude-code", Kind: record.KindSkill, Name: "unused"}}
	if err := primitives.Refresh(events, Discovery{Primitives: available, ProjectScanned: true}, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(items) != 2 || items[0].Name != "used" || items[0].Invocations != 1 || !items[0].LastUsed.Equal(first.Timestamp) || items[1].Name != "unused" || items[1].Invocations != 0 || !items[1].LastUsed.IsZero() {
		t.Fatalf("inventory = %+v", items)
	}

	second := inventoryRecord("second", "used", first.Timestamp.Add(time.Minute))
	if _, appendErr := events.Append([]record.Record{second}); appendErr != nil {
		t.Fatalf("Append() error = %v", appendErr)
	}
	if refreshErr := primitives.Refresh(events, Discovery{Primitives: available[:1], ProjectScanned: true}, nil); refreshErr != nil {
		t.Fatalf("second Refresh() error = %v", refreshErr)
	}
	items, err = primitives.Read()
	if err != nil {
		t.Fatalf("Read() after refresh error = %v", err)
	}
	if len(items) != 1 || items[0].Invocations != 2 || !items[0].LastUsed.Equal(second.Timestamp) {
		t.Fatalf("updated inventory = %+v", items)
	}
}

func TestRefreshCarriesFailuresAndUnknownOutcomesIntoUsage(t *testing.T) {
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	failed := record.OutcomeError
	ok := record.OutcomeOK
	records := []record.Record{
		outcomeRecord("first", "flaky", &failed, time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)),
		outcomeRecord("second", "flaky", &ok, time.Date(2026, time.August, 13, 12, 1, 0, 0, time.UTC)),
		outcomeRecord("third", "flaky", nil, time.Date(2026, time.August, 13, 12, 2, 0, 0, time.UTC)),
	}
	if _, err := events.Append(records); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	primitives := New(statePath)
	available := []Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "flaky"}}
	if err := primitives.Refresh(events, Discovery{Primitives: available, ProjectScanned: true}, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(items) != 1 || items[0].Invocations != 3 || items[0].Failures != 1 || items[0].Unknown != 1 {
		t.Fatalf("usage = %+v, want 3 invocations, 1 failure, 1 unknown", items)
	}
}

func TestRefreshDropsPrimitivesWithUnsafeNames(t *testing.T) {
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	available := []Primitive{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "safe-skill"},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "usr/local/bin"},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "contains space"},
	}
	if err := New(statePath).Refresh(events, Discovery{Primitives: available, ProjectScanned: true}, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var snapshot struct{ Primitives []Usage }
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(snapshot.Primitives) != 1 || snapshot.Primitives[0].Name != "safe-skill" {
		t.Fatalf("primitives.json = %+v", snapshot.Primitives)
	}
	if strings.Contains(string(raw), "usr/local") {
		t.Fatalf("primitives.json retains a path: %s", raw)
	}
}

func TestRefreshCarriesForwardWhatAnUnscannedPassCouldNotSee(t *testing.T) {
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	first := inventoryRecord("first", "project-skill", time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC))
	if _, err := events.Append([]record.Record{first}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	primitives := New(statePath)
	discovered := []Primitive{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "project-skill"},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "global-skill"},
	}
	if err := primitives.Refresh(events, Discovery{Primitives: discovered, ProjectScanned: true}, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	second := inventoryRecord("second", "project-skill", first.Timestamp.Add(time.Minute))
	if _, err := events.Append([]record.Record{second}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	// The project-local half of discovery was withheld, so the pass never saw
	// project-skill. It must be carried rather than dropped.
	if err := primitives.Refresh(events, Discovery{Primitives: discovered[1:]}, nil); err != nil {
		t.Fatalf("partial Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("inventory = %+v, want both primitives", items)
	}
	if items[0].Name != "project-skill" || items[0].Invocations != 2 || !items[0].LastUsed.Equal(second.Timestamp) {
		t.Fatalf("carried primitive lost its counters: %+v", items[0])
	}
	if items[1].Name != "global-skill" {
		t.Fatalf("inventory = %+v", items)
	}
}

func TestReadRejectsInconsistentFailureCounts(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	// 2 invocations cannot hold 1 unknown and 2 failures: only 1 invocation is
	// left "known" to have failed.
	content := `{"version":2,"refreshed_at":"2026-08-13T12:00:00Z","primitives":[{"harness":"claude-code","kind":"skill","name":"flaky","repo":"0123456789abcdef0123456789abcdef","invocations":2,"failures":2,"unknown":1,"last_used":"2026-08-13T12:00:00Z"}]}`
	if err := os.WriteFile(statePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := New(statePath).Read(); err == nil {
		t.Fatal("Read() accepted a primitive with more failures than known invocations")
	}
}

func TestReadRejectsAPathShapedPrimitiveName(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	content := `{"version":2,"refreshed_at":"2026-08-13T12:00:00Z","primitives":[{"harness":"claude-code","kind":"skill","name":"usr/local/bin"}]}`
	if err := os.WriteFile(statePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := New(statePath).Read(); err == nil {
		t.Fatal("Read() accepted a path-shaped primitive name")
	}
}

// interleaveWindow is how long the stale refresh waits for the newer one. It is
// generous on purpose: on the passing path it is spent in full, and shortening it
// is what would make this test flaky. Raise it if CI needs more, never lower it.
const interleaveWindow = 200 * time.Millisecond

// growingSource returns more of the spool on each read and lets the first read be
// held open, so the test can force the exact interleaving that made an older
// snapshot overwrite a newer one: refresh A reads, refresh B reads and publishes,
// then A publishes what it read before B ran.
type growingSource struct {
	mu       sync.Mutex
	reads    int
	entries  []store.Entry
	entered  chan struct{} // closed inside the first read
	released chan struct{} // closed by the test once the second refresh has finished
}

func (g *growingSource) Entries(uint64) ([]store.Entry, error) {
	g.mu.Lock()
	g.reads++
	read := g.reads
	g.mu.Unlock()
	if read == 1 {
		close(g.entered)
		// Serialised, this wait times out because the second refresh cannot start
		// until this one has published and released the lock. Unserialised, it
		// returns at once and this refresh republishes what it read first.
		select {
		case <-g.released:
		case <-time.After(interleaveWindow):
		}
		return g.entries[:1], nil
	}
	return g.entries, nil
}

func TestRefreshCannotPublishAStaleSnapshotAfterANewerOne(t *testing.T) {
	first := inventoryRecord("first", "skill-a", time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC))
	second := inventoryRecord("second", "skill-a", first.Timestamp.Add(time.Minute))
	source := &growingSource{
		entries:  []store.Entry{{Position: 1, Record: first}, {Position: 2, Record: second}},
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	available := Discovery{
		Primitives:     []Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "skill-a"}},
		ProjectScanned: true,
	}

	stale := make(chan error, 1)
	go func() { stale <- primitives.Refresh(source, available, nil) }()
	<-source.entered
	if err := primitives.Refresh(source, available, nil); err != nil {
		t.Fatalf("second Refresh() error = %v", err)
	}
	close(source.released)
	if err := <-stale; err != nil {
		t.Fatalf("first Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	// The published snapshot must be the one derived from the later read: two
	// invocations, and the later timestamp.
	if len(items) != 1 || items[0].Invocations != 2 || !items[0].LastUsed.Equal(second.Timestamp) {
		t.Fatalf("a stale snapshot was published over a newer one: %+v", items)
	}
}

func TestRefreshWaitsForTheStateLock(t *testing.T) {
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	seed := inventoryRecord("first", "skill-a", time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC))
	if _, err := events.Append([]record.Record{seed}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	available := Discovery{
		Primitives:     []Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "skill-a"}},
		ProjectScanned: true,
	}

	done := make(chan error, 1)
	// finished records that the channel was already drained, so an unserialised
	// Refresh fails this test rather than blocking it forever on a second receive.
	finished := false
	// t.Errorf rather than t.Fatalf inside the closure, so the lock is released
	// however these assertions go.
	if err := lockfile.WithLock(primitives.lockPath, func() error {
		go func() { done <- primitives.Refresh(events, available, nil) }()
		select {
		case err := <-done:
			finished = true
			t.Errorf("Refresh() finished while the state lock was held: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if _, statErr := os.Stat(primitives.path); !os.IsNotExist(statErr) {
			t.Errorf("Refresh() published a snapshot while the state lock was held: %v", statErr)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithLock() error = %v", err)
	}
	if finished {
		return
	}
	if err := <-done; err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil || len(items) != 1 {
		t.Fatalf("Read() = %+v, %v", items, err)
	}
}

func inventoryRecord(id, name string, timestamp time.Time) record.Record {
	return record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID("claude-code", record.Identifier(id)),
		Timestamp:     timestamp,
		Harness:       "claude-code",
		SessionID:     "session-1",
		Repo:          "0123456789abcdef0123456789abcdef",
		Kind:          record.KindSkill,
		Name:          record.Identifier(name),
		Invoker:       record.InvokerModel,
	}
}

func outcomeRecord(id, name string, outcome *record.Outcome, timestamp time.Time) record.Record {
	r := inventoryRecord(id, name, timestamp)
	r.Outcome = outcome
	return r
}

func repoRecord(id, name string, repo record.Hash, timestamp time.Time) record.Record {
	r := inventoryRecord(id, name, timestamp)
	r.Repo = repo
	return r
}

// TestRefreshSplitsUsageByRepository is DG-93's grain change at the layer both
// renderers read. metrics.Aggregate splitting per repository is not enough on its
// own: derive joins discovery — which has no repository (ADR-0002) — against the
// aggregate, and a join on a repo-less key would collapse the split straight back.
func TestRefreshSplitsUsageByRepository(t *testing.T) {
	first, second := record.Hash("0123456789abcdef0123456789abcdef"), record.Hash("fedcba9876543210fedcba9876543210")
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		repoRecord("here", "used", first, at),
		repoRecord("there", "used", second, at.Add(time.Minute)),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	discovered := Discovery{
		Primitives:     []Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "used"}},
		ProjectScanned: true,
	}
	if err := primitives.Refresh(events, discovered, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("inventory = %+v, want one row per repository", items)
	}
	repos := map[record.Hash]Usage{}
	for _, usage := range items {
		if usage.Name != "used" {
			t.Fatalf("unexpected row %+v", usage)
		}
		repos[usage.Repo] = usage
	}
	for _, repo := range []record.Hash{first, second} {
		usage, present := repos[repo]
		if !present {
			t.Fatalf("no row for repository %q: %+v", repo, items)
		}
		if usage.Invocations != 1 {
			t.Fatalf("row %q invocations = %d, want 1", repo, usage.Invocations)
		}
	}
}

func TestRefreshLeavesAnUnusedPrimitiveWithoutARepository(t *testing.T) {
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	discovered := Discovery{
		Primitives:     []Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "unused"}},
		ProjectScanned: true,
	}
	if err := primitives.Refresh(events, discovered, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(items) != 1 || items[0].Repo != "" || items[0].Invocations != 0 {
		t.Fatalf("inventory = %+v, want one repo-less row with no invocations", items)
	}
}

func TestReadRefusesAUsedPrimitiveWithNoRepository(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	content := `{"version":2,"refreshed_at":"2026-08-13T12:00:00Z","primitives":[{"harness":"claude-code","kind":"skill","name":"used","invocations":1,"last_used":"2026-08-13T12:00:00Z"}]}`
	if err := os.WriteFile(statePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := New(statePath).Read(); err == nil {
		t.Fatal("Read() accepted an invoked primitive with no repository")
	}
}

func TestReadRefusesAnUnusedPrimitiveCarryingARepository(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	content := `{"version":2,"refreshed_at":"2026-08-13T12:00:00Z","primitives":[{"harness":"claude-code","kind":"skill","name":"unused","repo":"0123456789abcdef0123456789abcdef","invocations":0}]}`
	if err := os.WriteFile(statePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := New(statePath).Read(); err == nil {
		t.Fatal("Read() accepted an uninvoked primitive carrying a repository")
	}
}

func TestReadRefusesARepositoryThatIsNotAnId(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	content := `{"version":2,"refreshed_at":"2026-08-13T12:00:00Z","primitives":[{"harness":"claude-code","kind":"skill","name":"used","repo":"/Users/someone/code","invocations":1,"last_used":"2026-08-13T12:00:00Z"}]}`
	if err := os.WriteFile(statePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := New(statePath).Read(); err == nil {
		t.Fatal("Read() accepted a path-shaped repository")
	}
}

// TestReadTreatsAPreviousVersionSnapshotAsAnEmptyInventory pins the upgrade path:
// the snapshot's row grain changed, so a file this build did not write says nothing
// it can read — but it is derived, regenerable state, so `wake report` degrades to
// an empty inventory rather than failing on an existing install's first run.
func TestReadTreatsAPreviousVersionSnapshotAsAnEmptyInventory(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	content := `{"version":1,"refreshed_at":"2026-08-13T12:00:00Z","primitives":[{"harness":"claude-code","kind":"skill","name":"used","invocations":1,"last_used":"2026-08-13T12:00:00Z"}]}`
	if err := os.WriteFile(statePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	items, err := New(statePath).Read()
	if err != nil {
		t.Fatalf("Read() error = %v, want a previous-version snapshot to degrade", err)
	}
	if items != nil {
		t.Fatalf("Read() = %+v, want no inventory", items)
	}
}

// DG-106's join: the two discovered spellings of one plugin skill collapse to one
// row under the namespaced name, and usage recorded under either spelling
// accumulates on it. The kind is asserted unchanged — no fold ever moves a
// primitive between kinds (ADR-0005).
func TestRefreshFoldsBothSpellingsOntoOneRowUnderTheNamespacedName(t *testing.T) {
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		inventoryRecord("one", "superpowers:brainstorming", at),
		inventoryRecord("two", "superpowers:brainstorming", at.Add(time.Minute)),
		inventoryRecord("three", "brainstorming", at.Add(2*time.Minute)),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, foldedDiscovery(true), nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("inventory = %+v, want one row", items)
	}
	if items[0].Name != "superpowers:brainstorming" || items[0].Kind != record.KindSkill {
		t.Fatalf("row = %+v, want the namespaced name under kind skill", items[0])
	}
	if items[0].Invocations != 3 || !items[0].LastUsed.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("row = %+v, want 3 invocations last used at %v", items[0], at.Add(2*time.Minute))
	}
}

// Summing across the fold has to preserve what Usage.valid() checks: unknown
// outcomes stay excluded from the failure denominator rather than counting as ok
// (ADR-0005, ADR-0006).
func TestRefreshFoldedRowKeepsFailureAndUnknownInvariants(t *testing.T) {
	failed, ok := record.OutcomeError, record.OutcomeOK
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		outcomeRecord("one", "brainstorming", &failed, at),
		outcomeRecord("two", "superpowers:brainstorming", &ok, at.Add(time.Minute)),
		outcomeRecord("three", "superpowers:brainstorming", nil, at.Add(2*time.Minute)),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, foldedDiscovery(true), nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(items) != 1 || items[0].Invocations != 3 || items[0].Failures != 1 || items[0].Unknown != 1 {
		t.Fatalf("inventory = %+v, want one row with 3 invocations, 1 failure, 1 unknown", items)
	}
}

// A carried name is not provenance: it comes from a previous snapshot, not from a
// source, so it can never create a fold — it is only ever folded by one the
// current pass proved.
func TestRefreshFoldsACarriedForwardBareRowOntoTheCanonicalName(t *testing.T) {
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	bare := Discovery{
		Primitives:     []Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "brainstorming"}},
		ProjectScanned: true,
	}
	if err := primitives.Refresh(events, bare, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	partial := Discovery{
		Primitives:     []Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "superpowers:brainstorming"}},
		ProjectScanned: false,
		canonical:      map[identity]record.Identifier{{harness: "claude-code", kind: record.KindSkill, name: "brainstorming"}: "superpowers:brainstorming"},
	}
	if err := primitives.Refresh(events, partial, nil); err != nil {
		t.Fatalf("second Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(items) != 1 || items[0].Name != "superpowers:brainstorming" {
		t.Fatalf("inventory = %+v, want one row named superpowers:brainstorming", items)
	}
}

// With nothing proved, nothing folds: the pre-existing pair of rows survives
// untouched rather than a merged counter being fabricated. This is also the
// regression guard for every other test in this file, none of which supplies a
// canonical map.
func TestRefreshLeavesAnUnprovenPairAsTwoRows(t *testing.T) {
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		inventoryRecord("one", "superpowers:brainstorming", at),
		inventoryRecord("two", "brainstorming", at.Add(time.Minute)),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, foldedDiscovery(false), nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("inventory = %+v, want two rows", items)
	}
	for _, usage := range items {
		if usage.Invocations != 1 {
			t.Fatalf("row = %+v, want one invocation each", usage)
		}
	}
}

// foldedDiscovery is both spellings of one plugin skill, with the fold between
// them either proved or absent.
func foldedDiscovery(proved bool) Discovery {
	discovery := Discovery{
		Primitives: []Primitive{
			{Harness: "claude-code", Kind: record.KindSkill, Name: "brainstorming"},
			{Harness: "claude-code", Kind: record.KindSkill, Name: "superpowers:brainstorming"},
		},
		ProjectScanned: true,
	}
	if proved {
		discovery.canonical = map[identity]record.Identifier{
			{harness: "claude-code", kind: record.KindSkill, name: "brainstorming"}: "superpowers:brainstorming",
		}
	}
	return discovery
}

// TestUsageErrorRateCarriesTheStoredCountsAsAPopulation pins the inverse of the
// flattening derive does: the four counts a snapshot stores go back out as the
// Ratio they came from, with the unrated calls excluded from the denominator
// rather than counted as successes (ADR-0005, ADR-0006). It lives here because
// this is where a renderer used to be told to rebuild the rate itself (DG-103).
func TestUsageErrorRateCarriesTheStoredCountsAsAPopulation(t *testing.T) {
	usage := Usage{Harness: "claude-code", Kind: record.KindSkill, Name: "flaky", Repo: "0123456789abcdef0123456789abcdef", Invocations: 4, Failures: 1, Unknown: 1, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)}
	if !usage.valid() {
		t.Fatalf("fixture is not a snapshot row Read would accept: %+v", usage)
	}
	ratio := usage.ErrorRate()
	for _, test := range []struct {
		name string
		got  uint64
		want uint64
	}{
		{name: "numerator", got: ratio.Numerator(), want: 1},
		{name: "denominator", got: ratio.Denominator(), want: 3},
		{name: "excluded", got: ratio.Excluded(), want: 1},
		{name: "total", got: ratio.Total(), want: 4},
	} {
		if test.got != test.want {
			t.Errorf("ErrorRate().%s = %d, want %d", test.name, test.got, test.want)
		}
	}
	percent, ok := ratio.Percent()
	if !ok {
		t.Fatalf("ErrorRate().Percent() reported no rate for a rated population")
	}
	if got := fmt.Sprintf("%.1f", percent); got != "33.3" {
		t.Errorf("ErrorRate().Percent() = %s, want 33.3", got)
	}
}

// The other half of DG-93's grain change, one ticket later: rows split per
// repository, but a linked git worktree is not a repository of its own to a reader
// of the report. TestRefreshSplitsUsageByRepository above pins that two unrelated
// repositories still get two rows; this pins that two spellings of one project get
// one. The snapshot is what `wake report` and the dashboard render, so it has to
// arrive already counted under the repository (ADR-0011).
func TestASnapshotCountsAWorktreesRowsUnderItsRepository(t *testing.T) {
	parent, worktree := record.Hash("0123456789abcdef0123456789abcdef"), record.Hash("fedcba9876543210fedcba9876543210")
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		repoRecord("here", "used", parent, at),
		repoRecord("there", "used", worktree, at.Add(time.Minute)),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	discovered := Discovery{
		Primitives:     []Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "used"}},
		ProjectScanned: true,
	}
	rollup := metrics.RepoRollup{string(worktree): string(parent)}
	if err := primitives.Refresh(events, discovered, rollup); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("inventory = %+v, want one row; a worktree's rows are counted under the repository", items)
	}
	if items[0].Repo != parent {
		t.Errorf("row repository = %q, want %q", items[0].Repo, parent)
	}
	if items[0].Invocations != 2 {
		t.Errorf("row invocations = %d, want 2", items[0].Invocations)
	}
}

// mcpToolRecord is one MCP tool invocation carrying its observed server segment —
// exactly what the Claude Code reader writes for an "mcp__<server>__<tool>" call.
func mcpToolRecord(id, toolName, server string, repo record.Hash, timestamp time.Time) record.Record {
	r := inventoryRecord(id, toolName, timestamp)
	r.Kind = record.KindMCPTool
	r.MCPServer = record.Identifier(server)
	r.Repo = repo
	return r
}

func mcpServerDiscovery(names ...string) Discovery {
	discovery := Discovery{ProjectScanned: true}
	for _, name := range names {
		discovery.Primitives = append(discovery.Primitives, Primitive{Harness: "claude-code", Kind: record.KindMCPServer, Name: record.Identifier(name)})
	}
	return discovery
}

func usageNamed(t *testing.T, items []Usage, name record.Identifier) Usage {
	t.Helper()
	for _, item := range items {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("inventory = %+v, want a row named %q", items, name)
	return Usage{}
}

// TestRefreshRollsMCPToolCallsOntoAnExactlyNamedServer is DG-99's headline bug: a
// configured server whose tools are used heavily still reported zero invocations,
// so `--unused` recommended removing it. Its tools' calls now land on its row.
func TestRefreshRollsMCPToolCallsOntoAnExactlyNamedServer(t *testing.T) {
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		mcpToolRecord("one", "mcp__claude-in-chrome__computer", "claude-in-chrome", repo, at),
		mcpToolRecord("two", "mcp__claude-in-chrome__navigate", "claude-in-chrome", repo, at.Add(time.Minute)),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, mcpServerDiscovery("claude-in-chrome"), nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	server := usageNamed(t, items, "claude-in-chrome")
	if server.Kind != record.KindMCPServer {
		t.Fatalf("server row = %+v, want kind mcp_server", server)
	}
	if server.Invocations != 2 {
		t.Fatalf("server row = %+v, want 2 invocations — a used server must never read zero", server)
	}
	if server.Unmatched {
		t.Errorf("server row = %+v, want Unmatched false for an exactly named server", server)
	}
	if server.Repo != repo || !server.LastUsed.Equal(at.Add(time.Minute)) {
		t.Errorf("server row = %+v, want repo %q last used %v", server, repo, at.Add(time.Minute))
	}
}

// The plugin triple is the case the normalisation exists for: the config key
// carries colons, the tool name carries underscores, and the record must keep the
// spelling it observed while the row is published under the configured key.
func TestRefreshRollsMCPToolCallsOntoASanitisedPluginTriple(t *testing.T) {
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	seeded := mcpToolRecord("one", "mcp__plugin_context7_context7__query-docs", "plugin_context7_context7", repo, at)
	if seeded.MCPServer != "plugin_context7_context7" {
		t.Fatalf("record stored a normalised guess: %q", seeded.MCPServer)
	}
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{seeded}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, mcpServerDiscovery("plugin:context7:context7"), nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	server := usageNamed(t, items, "plugin:context7:context7")
	if server.Invocations != 1 || server.Unmatched {
		t.Fatalf("server row = %+v, want 1 invocation and Unmatched false", server)
	}
}

// A server the index cannot name still gets a row carrying its calls, flagged
// unmatched. "collects nothing" is not "collects zero" (plan §12): reporting a used
// server as zero is the failure mode this ticket exists to end.
func TestRefreshReportsAServerWithNoDiscoveredMatchAsUnmatched(t *testing.T) {
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		mcpToolRecord("one", "mcp__linear-server__list_issues", "linear-server", repo, at),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, mcpServerDiscovery("linear"), nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	observed := usageNamed(t, items, "linear-server")
	if observed.Kind != record.KindMCPServer || observed.Invocations != 1 || !observed.Unmatched {
		t.Fatalf("observed row = %+v, want an unmatched mcp_server with 1 invocation", observed)
	}
	configured := usageNamed(t, items, "linear")
	if configured.Invocations != 0 || configured.Unmatched {
		t.Fatalf("configured row = %+v, want 0 invocations and Unmatched false", configured)
	}
}

// The roll-up is the same arithmetic the tool rows use, so the invariants
// Usage.valid() asserts hold on a server row too: unknown outcomes stay out of the
// failure denominator rather than counting as ok (ADR-0005, ADR-0006).
func TestRefreshServerRowKeepsFailureAndUnknownInvariants(t *testing.T) {
	failed, ok := record.OutcomeError, record.OutcomeOK
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	first := mcpToolRecord("one", "mcp__claude-in-chrome__computer", "claude-in-chrome", repo, at)
	first.Outcome = &failed
	second := mcpToolRecord("two", "mcp__claude-in-chrome__navigate", "claude-in-chrome", repo, at.Add(time.Minute))
	second.Outcome = &ok
	third := mcpToolRecord("three", "mcp__claude-in-chrome__navigate", "claude-in-chrome", repo, at.Add(2*time.Minute))

	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{first, second, third}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, mcpServerDiscovery("claude-in-chrome"), nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	server := usageNamed(t, items, "claude-in-chrome")
	if server.Invocations != 3 || server.Failures != 1 || server.Unknown != 1 {
		t.Fatalf("server row = %+v, want 3 invocations, 1 failure, 1 unknown", server)
	}
	rate := server.ErrorRate()
	if rate.Numerator() != 1 || rate.Denominator() != 2 || rate.Excluded() != 1 {
		t.Fatalf("ErrorRate() = %+v", rate)
	}
}

// A repository is a property of the invocation (ADR-0002), and the roll-up must not
// launder that away: one server used in two repositories is two server rows.
func TestRefreshSplitsAServerRowByRepository(t *testing.T) {
	here, there := record.Hash("0123456789abcdef0123456789abcdef"), record.Hash("fedcba9876543210fedcba9876543210")
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		mcpToolRecord("one", "mcp__claude-in-chrome__computer", "claude-in-chrome", here, at),
		mcpToolRecord("two", "mcp__claude-in-chrome__computer", "claude-in-chrome", there, at.Add(time.Minute)),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, mcpServerDiscovery("claude-in-chrome"), nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	repos := map[record.Hash]uint64{}
	for _, item := range items {
		if item.Kind == record.KindMCPServer {
			repos[item.Repo] += item.Invocations
		}
	}
	if len(repos) != 2 || repos[here] != 1 || repos[there] != 1 {
		t.Fatalf("server rows by repo = %+v, want one invocation in each of two repositories", repos)
	}
}

// Unmatched only ever describes an observed MCP server. A snapshot claiming
// otherwise is refused rather than repaired (fail closed, plan §3.4).
func TestReadRefusesAnUnmatchedFlagOnANonServerRow(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	content := `{"version":2,"refreshed_at":"2026-08-13T12:00:00Z","primitives":[{"harness":"claude-code","kind":"skill","name":"review","repo":"0123456789abcdef0123456789abcdef","invocations":2,"unmatched":true,"last_used":"2026-08-13T12:00:00Z"}]}`
	if err := os.WriteFile(statePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := New(statePath).Read(); err == nil {
		t.Fatal("Read() accepted an unmatched flag on a skill row")
	}
}

// The server roll-up is its own cross-kind path, built beside DG-106's fold and
// never through it: canonicalIdentity never touches kind, and this must not make it
// start.
func TestRefreshDoesNotFoldAServerThroughCanonical(t *testing.T) {
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		mcpToolRecord("one", "mcp__brainstorming__go", "brainstorming", repo, at),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	discovery := foldedDiscovery(true)
	discovery.Primitives = append(discovery.Primitives, Primitive{Harness: "claude-code", Kind: record.KindMCPServer, Name: "brainstorming"})
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, discovery, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	for _, item := range items {
		if item.Kind == record.KindMCPServer && item.Name != "brainstorming" {
			t.Fatalf("server row was folded onto %q", item.Name)
		}
	}
	server := usageNamed(t, items, "brainstorming")
	if server.Kind != record.KindMCPServer || server.Invocations != 1 {
		t.Fatalf("server row = %+v, want an mcp_server with 1 invocation", server)
	}
}

// An unmatched row is an observation, not a discovery. A pass whose project-local
// discovery was withheld carries the previous snapshot's names forward, and if it
// carried this one the segment would look discovered on the next pass: the flag
// would clear and the report would assert a match no config key supports (ADR-0039
// §4). The calls are re-derived from the spool either way, so the row comes back —
// still flagged.
func TestRefreshKeepsAnUnmatchedServerFlaggedAcrossAnUnscannedPass(t *testing.T) {
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{
		mcpToolRecord("one", "mcp__linear-server__list_issues", "linear-server", repo, at),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, mcpServerDiscovery("linear"), nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	withheld := mcpServerDiscovery("linear")
	withheld.ProjectScanned = false
	if err := primitives.Refresh(events, withheld, nil); err != nil {
		t.Fatalf("unscanned Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	observed := usageNamed(t, items, "linear-server")
	if observed.Kind != record.KindMCPServer || observed.Invocations != 1 || !observed.Unmatched {
		t.Fatalf("observed row = %+v, want an unmatched mcp_server with 1 invocation", observed)
	}
}

// Discovery and the roll-up must agree on what identifies a server: the roll-up
// row is per harness (ADR-0002's grain), so a server name one harness discovered
// says nothing about a segment observed under another. Keyed on the name alone,
// the segment below resolves as "discovered", is never flagged unnamed, and is
// never published — the calls vanish silently, which is the very failure DG-99
// exists to fix.
func TestRefreshDoesNotMatchAServerDiscoveredUnderAnotherHarness(t *testing.T) {
	repo := record.Hash("0123456789abcdef0123456789abcdef")
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	elsewhere := mcpToolRecord("one", "mcp__linear__list_issues", "linear", repo, at)
	elsewhere.Harness = "codex"
	elsewhere.EventID = record.DeriveEventID("codex", "one")

	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := events.Append([]record.Record{elsewhere}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(events, mcpServerDiscovery("linear"), nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	items, err := primitives.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	var observed, configured *Usage
	for i, item := range items {
		if item.Kind != record.KindMCPServer {
			continue
		}
		switch item.Harness {
		case "codex":
			observed = &items[i]
		case "claude-code":
			configured = &items[i]
		}
	}
	if observed == nil || observed.Invocations != 1 || !observed.Unmatched {
		t.Fatalf("inventory = %+v, want an unmatched codex mcp_server row with 1 invocation", items)
	}
	if configured == nil || configured.Invocations != 0 || configured.Unmatched {
		t.Fatalf("inventory = %+v, want the discovered claude-code row untouched at 0 invocations", items)
	}
}
