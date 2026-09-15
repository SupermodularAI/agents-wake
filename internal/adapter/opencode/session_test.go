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
	scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{}, idle)
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
	scan := NewScan(declines, testNames(), NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
	scan.Session(session("ses_abc"))
	result := scan.Close()
	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want none", len(result.Records))
	}
}

func TestSessionEndsAreDerivedInAStableOrder(t *testing.T) {
	run := func() []record.Hash {
		scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
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
	scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
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
	scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{Timeout: time.Hour, Now: past}, adapter.Idleness{})
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

// closedChild drives a scan over one child session past the idle threshold, which
// is the only boundary either grain is emitted at.
func closedChild(from Session) Result {
	scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
	scan.Session(from)
	return scan.Close()
}

// subagentOf picks the subagent invocation out of a walk's records, or fails.
func subagentOf(t *testing.T, result Result) record.Record {
	t.Helper()
	for _, derived := range result.Records {
		if derived.Kind == record.KindSubagent {
			return derived
		}
	}
	t.Fatalf("no subagent invocation among %d records", len(result.Records))
	return record.Record{}
}

func TestAChildSessionDerivesASubagentInvocation(t *testing.T) {
	result := closedChild(childSession("ses_child", "ses_parent", "explore"))
	if len(result.Records) != 2 {
		t.Fatalf("records = %d, want 2: the session grain and the invocation", len(result.Records))
	}
	got := subagentOf(t, result)
	if got.Name != "explore" {
		t.Errorf("name = %q, want explore: the harness's own declaration (ADR-0041)", got.Name)
	}
	if got.Invoker != record.InvokerModel {
		t.Errorf("invoker = %q, want model: a subagent run is entered by the model", got.Invoker)
	}
	// ADR-0036 §2: ok is never derived for a subagent, from either side, and
	// absence stays nil. opencode reports no duration for the run either.
	if got.Outcome != nil || got.DurationMS != nil || got.Model != "" {
		t.Errorf("outcome = %v, duration = %v, model = %q, want nil, nil and empty", got.Outcome, got.DurationMS, got.Model)
	}
	if got.HarnessVersion != "1.18.30" {
		t.Errorf("harness version = %q, want 1.18.30", got.HarnessVersion)
	}
	if want := record.DeriveEventID("opencode", record.Identifier("ses_child\x1dsubagent")); got.EventID != want {
		t.Errorf("event id = %q, want %q: derived from the child session's own id", got.EventID, want)
	}
}

// ADR-0027 fixes trace_id at harness ‖ session_id, so a subagent invocation
// carrying the child's own session id lands in the child's own trace, whose root
// span is that child's session_end. The parent's id is never substituted.
func TestASubagentInvocationCarriesTheChildsOwnSessionID(t *testing.T) {
	got := subagentOf(t, closedChild(childSession("ses_child", "ses_parent", "explore")))
	if got.SessionID != "ses_child" {
		t.Fatalf("session id = %q, want ses_child", got.SessionID)
	}
}

// The run happened when the session was opened, not when it last went quiet:
// time_updated is the closure signal, and stamping the invocation with it would
// report the idle threshold rather than the run.
func TestASubagentInvocationIsStampedAtTheSessionsCreation(t *testing.T) {
	from := childSession("ses_child", "ses_parent", "explore")
	got := subagentOf(t, closedChild(from))
	want := record.NormalizedTimestamp(time.UnixMilli(from.CreatedMS).UTC())
	if got.Timestamp != want {
		t.Fatalf("timestamp = %v, want %v", got.Timestamp, want)
	}
}

// Two grains off one row is expected symmetry, not duplication — and the
// kind-discriminating component is what keeps the two ids disjoint (ADR-0034 §1).
func TestASubagentInvocationNeverCollidesWithItsSessionEnd(t *testing.T) {
	result := closedChild(childSession("ses_child", "ses_parent", "explore"))
	if len(result.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(result.Records))
	}
	if result.Records[0].EventID == result.Records[1].EventID {
		t.Fatal("the session grain and the invocation share an event id")
	}
}

// Measured on a real store: 36 top-level sessions declare an agent with no parent,
// so gating on the agent would fabricate 36 subagent invocations out of ordinary
// sessions. parent_id is the gate; agent is only the name.
func TestATopLevelSessionWithAnAgentDerivesNoSubagent(t *testing.T) {
	from := session("ses_abc")
	from.Agent = "explore"
	result := closedChild(from)

	if len(result.Records) != 1 {
		t.Fatalf("records = %d, want 1: the session grain alone", len(result.Records))
	}
	if result.Records[0].Kind != record.KindSessionEnd {
		t.Fatalf("kind = %q, want session_end", result.Records[0].Kind)
	}
	if result.Refused != 0 {
		t.Fatalf("refused = %d, want 0: a session with no parent is not a subagent run at all", result.Refused)
	}
}

// ADR-0036 §2: refused and counted, never named by inference.
func TestAChildSessionWithNoAgentIsRefusedAndCounted(t *testing.T) {
	result := closedChild(childSession("ses_child", "ses_parent", ""))

	if len(result.Records) != 1 || result.Records[0].Kind != record.KindSessionEnd {
		t.Fatalf("records = %d, want the session grain alone", len(result.Records))
	}
	if result.Refused != 1 {
		t.Fatalf("refused = %d, want 1", result.Refused)
	}
}

// The creation instant is the invocation's whole timestamp, so an absent one is
// refused rather than coalesced into a 1970 record that passes every validator.
func TestAChildSessionWithNoCreationInstantIsRefused(t *testing.T) {
	from := childSession("ses_child", "ses_parent", "explore")
	from.CreatedMS, from.HasCreated = 0, false
	result := closedChild(from)

	for _, derived := range result.Records {
		if derived.Kind == record.KindSubagent {
			t.Fatal("a subagent invocation was derived with no creation instant")
		}
		if derived.Timestamp.Year() == 1970 {
			t.Fatal("a record was stamped at the epoch")
		}
	}
	if result.Refused != 1 {
		t.Fatalf("refused = %d, want 1", result.Refused)
	}
}

// A directory outside consent derives nothing at all, and that is an honest zero
// rather than lost collection — the same answer sessionEnd gives for the row.
func TestAnUnconsentedChildSessionIsAnHonestZero(t *testing.T) {
	scan := NewScan(declines, testNames(), NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
	scan.Session(childSession("ses_child", "ses_parent", "explore"))
	result := scan.Close()

	if len(result.Records) != 0 || result.Refused != 0 {
		t.Fatalf("records = %d, refused = %d, want 0 and 0", len(result.Records), result.Refused)
	}
}

// No second threshold is introduced: the invocation resolves at the idle boundary
// the session grain already uses, and an open child session is not a terminal
// event (ADR-0015, ADR-0023 §3).
func TestASubagentInvocationIsNotEmittedWhileTheSessionIsOpen(t *testing.T) {
	scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: near})
	scan.Session(childSession("ses_child", "ses_parent", "explore"))
	result := scan.Close()

	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want none while the session is still open", len(result.Records))
	}
}
