package inventory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/lockfile"
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
	if err := primitives.Refresh(events, Discovery{Primitives: available, ProjectScanned: true}); err != nil {
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
	if refreshErr := primitives.Refresh(events, Discovery{Primitives: available[:1], ProjectScanned: true}); refreshErr != nil {
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
	if err := primitives.Refresh(events, Discovery{Primitives: available, ProjectScanned: true}); err != nil {
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
	if err := New(statePath).Refresh(events, Discovery{Primitives: available, ProjectScanned: true}); err != nil {
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
	if err := primitives.Refresh(events, Discovery{Primitives: discovered, ProjectScanned: true}); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	second := inventoryRecord("second", "project-skill", first.Timestamp.Add(time.Minute))
	if _, err := events.Append([]record.Record{second}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	// The project-local half of discovery was withheld, so the pass never saw
	// project-skill. It must be carried rather than dropped.
	if err := primitives.Refresh(events, Discovery{Primitives: discovered[1:]}); err != nil {
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
	go func() { stale <- primitives.Refresh(source, available) }()
	<-source.entered
	if err := primitives.Refresh(source, available); err != nil {
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
		go func() { done <- primitives.Refresh(events, available) }()
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
	if err := primitives.Refresh(events, discovered); err != nil {
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
	if err := primitives.Refresh(events, discovered); err != nil {
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
