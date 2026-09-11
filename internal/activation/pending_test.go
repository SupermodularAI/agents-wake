package activation

import (
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// The merge is the scan's own min-fold carried across two writers: the earlier
// anchor wins, the earlier declaration wins, and the order the two carries
// arrive in changes nothing — the property that makes a hook-fired scan and a
// `wake ingest` racing the carry converge on what one scan over the union
// would have produced.
func TestMergePendingRunIsOrderIndependent(t *testing.T) {
	earlier := claudecode.PendingSubagentRun{
		AgentID:    "agent-1",
		SessionID:  "session-1",
		Repo:       "repo-1",
		UUID:       "uuid-a",
		Timestamp:  time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC),
		Entrypoint: "cli",
		Name:       "sdlc-run",
		DeclUUID:   "uuid-d",
		DeclTime:   time.Date(2026, 9, 7, 10, 0, 5, 0, time.UTC),
	}
	later := earlier
	later.UUID = "uuid-b"
	later.Timestamp = earlier.Timestamp.Add(time.Hour)
	later.DeclUUID = "uuid-e"
	later.DeclTime = earlier.DeclTime.Add(time.Hour)

	forward := mergePendingRun(earlier, later)
	reverse := mergePendingRun(later, earlier)
	if forward != reverse {
		t.Errorf("merge is order-dependent: forward = %+v, reverse = %+v", forward, reverse)
	}
	if forward.UUID != "uuid-a" || !forward.Timestamp.Equal(earlier.Timestamp) {
		t.Errorf("anchor = %q at %v, want the earlier fold", forward.UUID, forward.Timestamp)
	}
	if forward.DeclUUID != "uuid-d" || !forward.DeclTime.Equal(earlier.DeclTime) {
		t.Errorf("declaration = %q at %v, want the earlier fold", forward.DeclUUID, forward.DeclTime)
	}
}

// A run one carry declared and one did not merges into the declared half: the
// declaration is a fold over evidence, and dropping it would re-open the
// refusal the scan has not reached yet.
func TestMergePendingRunKeepsTheDeclaration(t *testing.T) {
	undeclared := claudecode.PendingSubagentRun{
		AgentID:   "agent-1",
		SessionID: "session-1",
		Repo:      "repo-1",
		UUID:      "uuid-a",
		Timestamp: time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC),
	}
	declared := undeclared
	declared.Name = "sdlc-run"
	declared.DeclUUID = "uuid-d"
	declared.DeclTime = undeclared.Timestamp.Add(time.Second)

	merged := mergePendingRun(undeclared, declared)
	if merged.Name != "sdlc-run" {
		t.Errorf("merged name = %q, want the declared half kept", merged.Name)
	}
}

// mergePending dedups children on the record's own event id: two scans that
// derived the same record carry it once, and the carry cannot grow by
// re-publishing what one source event yielded.
func TestMergePendingDedupsChildrenByEventID(t *testing.T) {
	event := record.Record{EventID: "event-1", SessionID: "session-1", Kind: record.KindBuiltinTool, Name: "Bash"}
	a := pendingState{Version: pendingVersion, Children: []claudecode.PendingChild{{Event: event, AgentID: "agent-1"}}}
	b := pendingState{Version: pendingVersion, Children: []claudecode.PendingChild{{Event: event, AgentID: "agent-1"}}}

	merged := mergePending(a, b)
	if len(merged.Children) != 1 {
		t.Errorf("merged children = %d, want 1 — one source event is one record however many scans carried it", len(merged.Children))
	}
}
