package claudecode

import (
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// A carried run restores through the same min-folds a fresh entry takes, so a
// restore and a re-read merge into what one scan over the union would have
// produced: the earlier anchor wins, the earlier declaration wins, and the
// session, repo and span follow the anchor that won.
func TestRestorePendingFoldsWithFreshEntries(t *testing.T) {
	scan := NewScan(nil, record.Namer{}, Installed{}, Staleness{}, Idleness{})
	carried := PendingSubagentRun{
		AgentID:    "agent-1",
		SessionID:  "session-1",
		Repo:       "repo-1",
		UUID:       "uuid-a",
		Timestamp:  time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC),
		Entrypoint: "cli",
		Version:    "2.1.0",
		Name:       "sdlc-run",
		Model:      "sonnet",
		DeclUUID:   "uuid-d",
		DeclTime:   time.Date(2026, 9, 7, 10, 0, 5, 0, time.UTC),
	}
	scan.RestorePending([]PendingSubagentRun{carried}, nil)

	runs, _ := scan.Pending()
	if len(runs) != 1 {
		t.Fatalf("Pending() = %d runs, want the restored one", len(runs))
	}
	if runs[0] != carried {
		t.Errorf("restored run = %+v, want the carried value round-tripped", runs[0])
	}
}

// A child carried across scans re-enters the deferred buffer and reports back
// through Pending after Close, with the restored source ordinal kept off every
// counter — credit and refuse ignore a negative source, so a restored child is
// never credited to a source of the scan that emits it. The session it belongs
// to was never observed here, so Close cannot conclude it ended and the child
// stays carried.
func TestRestorePendingChildRoundTrips(t *testing.T) {
	scan := NewScan(nil, record.Namer{}, Installed{}, Staleness{}, Idleness{})
	child := PendingChild{
		Event:   record.Record{EventID: "event-1", SessionID: "session-1", Kind: record.KindBuiltinTool, Name: "Bash"},
		AgentID: "agent-1",
	}
	scan.RestorePending(nil, []PendingChild{child})
	scan.Close()

	_, children := scan.Pending()
	if len(children) != 1 || children[0].Event.EventID != "event-1" || children[0].AgentID != "agent-1" {
		t.Errorf("Pending() children = %+v, want the carried child", children)
	}
}

// An unanchored run carries nothing: no session, no repo and no span a later
// scan could resolve, so Pending reports no carry for it — the same clean zero
// observeSubagentRun reports for an entry it cannot use.
func TestPendingReportsNoCarryForAnUnanchoredRun(t *testing.T) {
	run := &subagentRun{}
	if _, ok := run.pending("agent-1"); ok {
		t.Error("an unanchored run reported a carry; there is nothing a later scan could resolve from it")
	}
}
