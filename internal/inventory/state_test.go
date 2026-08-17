package inventory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err := primitives.Refresh(events, available); err != nil {
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
	if refreshErr := primitives.Refresh(events, available[:1]); refreshErr != nil {
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

func TestRefreshDropsPrimitivesWithUnsafeNames(t *testing.T) {
	events := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	available := []Primitive{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "safe-skill"},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "usr/local/bin"},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "contains space"},
	}
	if err := New(statePath).Refresh(events, available); err != nil {
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

func TestReadRejectsAPathShapedPrimitiveName(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "primitives.json")
	content := `{"version":1,"refreshed_at":"2026-08-13T12:00:00Z","primitives":[{"harness":"claude-code","kind":"skill","name":"usr/local/bin"}]}`
	if err := os.WriteFile(statePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := New(statePath).Read(); err == nil {
		t.Fatal("Read() accepted a path-shaped primitive name")
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
