package inventory

import (
	"path/filepath"
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
