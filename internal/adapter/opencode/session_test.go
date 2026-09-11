package opencode

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// finished drives a scan whose only content is one session, past the idle
// threshold.
func finished(idle adapter.Idleness) Result {
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{}, idle)
	scan.Session(session("ses_abc"))
	return scan.Close()
}

func TestAFinishedSessionDerivesOneSessionEnd(t *testing.T) {
	result := finished(adapter.Idleness{Timeout: time.Hour, Now: past})
	if len(result.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(result.Records))
	}
	got := result.Records[0]
	if got.Kind != record.KindSessionEnd || got.Name != "session" || got.Invoker != record.InvokerAuto {
		t.Fatalf("kind/name/invoker = %q/%q/%q, want session_end/session/auto", got.Kind, got.Name, got.Invoker)
	}
	if got.Outcome != nil || got.DurationMS != nil {
		t.Fatalf("outcome = %v, duration = %v, want both nil", got.Outcome, got.DurationMS)
	}
	if want := record.DeriveEventID("opencode", record.Identifier("ses_abc\x1esession_end")); got.EventID != want {
		t.Fatalf("event id = %q, want %q", got.EventID, want)
	}
}

func TestASessionEndCarriesTheFiveTokenTotals(t *testing.T) {
	got := finished(adapter.Idleness{Timeout: time.Hour, Now: past}).Records[0]
	totals := map[string]*int64{
		"input": got.InputTokens, "output": got.OutputTokens, "thinking": got.ThinkingTokens,
		"cache read": got.CacheReadTokens, "cache creation": got.CacheCreationTokens,
	}
	for name, total := range totals {
		if total == nil {
			t.Errorf("%s tokens are nil, want the harness's own total", name)
		}
	}
	// opencode pre-totals neither call count, and nil means "the harness reported
	// nothing", which is never 0.
	if got.ToolCalls != nil || got.BuiltinToolCalls != nil {
		t.Errorf("tool calls = %v / %v, want both nil", got.ToolCalls, got.BuiltinToolCalls)
	}
}

func TestNoTokenTotalIsEstimated(t *testing.T) {
	// Distinctive primes in, exact equality out: these come from opencode's own
	// integers and never from bytes ÷ 4, so no renderer can blend an estimate into
	// them (plan §2.6).
	got := finished(adapter.Idleness{Timeout: time.Hour, Now: past}).Records[0]
	for _, pair := range []struct {
		name string
		got  *int64
		want int64
	}{
		{"input", got.InputTokens, 11},
		{"output", got.OutputTokens, 13},
		{"thinking", got.ThinkingTokens, 17},
		{"cache read", got.CacheReadTokens, 19},
		{"cache creation", got.CacheCreationTokens, 23},
	} {
		if pair.got == nil || *pair.got != pair.want {
			t.Errorf("%s tokens = %v, want %d exactly", pair.name, pair.got, pair.want)
		}
	}
}

func TestASessionEndHasNoCostField(t *testing.T) {
	// session.cost is deferred, and the deferral is structural: no field holds it,
	// so no key can appear on the wire or on disk.
	got := finished(adapter.Idleness{Timeout: time.Hour, Now: past}).Records[0]
	line, err := record.Marshal(got)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	keys := map[string]json.RawMessage{}
	if err := json.Unmarshal(line, &keys); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if _, present := keys["cost"]; present {
		t.Fatal("the session grain carries a cost key")
	}
}

func TestZeroIdlenessDerivesNoSessionEnd(t *testing.T) {
	if result := finished(adapter.Idleness{}); len(result.Records) != 0 {
		t.Fatalf("records = %d, want none: a session_end on a guessed threshold is permanent", len(result.Records))
	}
}

func TestAnActiveSessionIsNotFinished(t *testing.T) {
	// Strictly greater than the threshold, matching the first adapter's rule: a
	// session silent for exactly the threshold is still open.
	exactly := fixtureTime.Add(time.Hour)
	if result := finished(adapter.Idleness{Timeout: time.Hour, Now: exactly}); len(result.Records) != 0 {
		t.Fatalf("records = %d, want none at exactly the threshold", len(result.Records))
	}
}

func TestAnUnconsentedSessionDerivesNoSessionEnd(t *testing.T) {
	scan := NewScan(declines, NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
	scan.Session(session("ses_abc"))
	result := scan.Close()
	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want none", len(result.Records))
	}
}

func TestSessionEndsAreDerivedInAStableOrder(t *testing.T) {
	run := func() []record.Hash {
		scan := NewScan(consents, NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
		for _, id := range []string{"ses_c", "ses_a", "ses_b"} {
			scan.Session(session(id))
		}
		ids := []record.Hash{}
		for _, derived := range scan.Close().Records {
			ids = append(ids, derived.EventID)
		}
		return ids
	}
	first, second := run(), run()
	if len(first) != 3 {
		t.Fatalf("records = %d, want 3", len(first))
	}
	for index := range first {
		if first[index] != second[index] {
			t.Fatalf("session_end order is not stable across scans")
		}
	}
}

// withoutLastActivity is a session row the harness recorded no time_updated for.
func withoutLastActivity(id string) Session {
	registered := session(id)
	registered.UpdatedMS, registered.HasUpdated = 0, false
	return registered
}

// A session's last-activity instant is the session grain's whole timestamp and the
// idleness comparison's whole input. Coalescing an absent one to the epoch makes
// every such session instantly, permanently "finished" and stamps its session_end
// at 1970 — a record nothing measured, deduplicated forever by ADR-0004. It is
// refused and counted instead.
func TestASessionWithNoLastActivityInstantDerivesNoSessionEnd(t *testing.T) {
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
	scan.Session(withoutLastActivity("ses_abc"))
	result := scan.Close()

	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want 0: no session_end carries an instant nothing recorded", len(result.Records))
	}
	if result.Refused != 1 {
		t.Fatalf("refused = %d, want 1: the loss has to be counted, not silent", result.Refused)
	}
}

// The same absence must not make the staleness rule give up on the session's
// parts: "no instant" is not "silent for long enough", and interrupted is a
// permanent verdict.
func TestAPartOfASessionWithNoLastActivityInstantStaysPending(t *testing.T) {
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{Timeout: time.Hour, Now: past}, adapter.Idleness{})
	scan.Session(withoutLastActivity("ses_abc"))
	scan.Part(toolPart("prt_abc", "bash", "running"))
	result := scan.Close()

	if result.Interrupted != 0 || len(result.Records) != 0 {
		t.Fatalf("interrupted = %d, records = %d, want 0 and 0", result.Interrupted, len(result.Records))
	}
	if result.Pending != 1 {
		t.Fatalf("pending = %d, want 1", result.Pending)
	}
}
