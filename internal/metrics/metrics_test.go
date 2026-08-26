package metrics

import (
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

func TestAggregateExcludesUnknownOutcomes(t *testing.T) {
	ok := record.OutcomeOK
	errOutcome := record.OutcomeError
	summary := Aggregate([]record.Record{
		testRecord("one", &ok),
		testRecord("two", nil),
		testRecord("three", &errOutcome),
	})

	if summary.Invocations != 3 || summary.Sessions != 1 {
		t.Fatalf("summary counts = %+v", summary)
	}
	if summary.ErrorRate.Numerator() != 1 || summary.ErrorRate.Denominator() != 2 || summary.ErrorRate.Excluded() != 1 || summary.ErrorRate.Total() != 3 {
		t.Fatalf("error rate = %+v", summary.ErrorRate)
	}
	percent, defined := summary.ErrorRate.Percent()
	if !defined || percent != 50 {
		t.Fatalf("Percent() = %v, %t; want 50, true", percent, defined)
	}
}

func TestAggregateExcludesBuiltinToolActivity(t *testing.T) {
	ok := record.OutcomeOK
	skill := testRecord("skill-call", &ok)
	builtin := testRecord("bash-call", &ok)
	builtin.Kind = record.KindBuiltinTool
	builtin.Name = "Bash"
	builtin.SessionID = "session-builtin-only"
	summary := Aggregate([]record.Record{skill, builtin})

	if summary.Invocations != 1 || summary.Sessions != 1 {
		t.Fatalf("summary counts = %+v, want only the skill call counted", summary)
	}
	if summary.Outcomes[record.OutcomeOK] != 1 {
		t.Fatalf("Outcomes[ok] = %d, want 1 (builtin tool ok must not count)", summary.Outcomes[record.OutcomeOK])
	}
	if len(summary.Primitives) != 1 || summary.Primitives[0].Kind == record.KindBuiltinTool {
		t.Fatalf("Primitives = %+v, want only the skill", summary.Primitives)
	}
}

func TestAggregateOrdersPrimitiveUsage(t *testing.T) {
	ok := record.OutcomeOK
	first := testRecord("one", &ok)
	second := testRecord("two", &ok)
	second.Name = "more-used"
	summary := Aggregate([]record.Record{first, second, second})
	if len(summary.Primitives) != 2 || summary.Primitives[0].Name != "more-used" {
		t.Fatalf("Primitives = %+v", summary.Primitives)
	}
}

func TestAggregateKeepsInvocationProvenanceSeparate(t *testing.T) {
	ok := record.OutcomeOK
	direct := testRecord("direct", &ok)
	direct.Invoker = record.InvokerUser
	byAgent := testRecord("agent", &ok)
	byAgent.ViaAgent = "sdlc-implement"
	summary := Aggregate([]record.Record{direct, byAgent})
	if len(summary.Primitives) != 2 {
		t.Fatalf("Primitives = %+v", summary.Primitives)
	}
	if summary.Primitives[0].Invoker != record.InvokerModel || summary.Primitives[0].ViaAgent != "sdlc-implement" || summary.Primitives[1].Invoker != record.InvokerUser || summary.Primitives[1].ViaAgent != "" {
		t.Fatalf("Primitives = %+v", summary.Primitives)
	}
}

func TestRatioDoesNotHaveARateWithoutDenominator(t *testing.T) {
	ratio := NewRatio(0, 0, 1, 1)
	if _, ok := ratio.Percent(); ok {
		t.Fatal("Percent() reported a rate with no denominator")
	}
}

func testRecord(source string, outcome *record.Outcome) record.Record {
	return record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID("claude-code", record.Identifier(source)),
		Timestamp:     time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC),
		Harness:       "claude-code",
		SessionID:     "session-1",
		Repo:          "0123456789abcdef0123456789abcdef",
		Kind:          record.KindSkill,
		Name:          "review",
		Invoker:       record.InvokerModel,
		Outcome:       outcome,
	}
}

// TestAggregateCountsASessionEndAsASessionNotAnInvocation pins what a session-grain
// record is evidence of: that a session existed, and nothing else. Counting it as an
// invocation would put a primitive named "session" in every report and add a row to
// every rate's denominator that nobody invoked (ADR-0002, ADR-0006).
func TestAggregateCountsASessionEndAsASessionNotAnInvocation(t *testing.T) {
	ok := record.OutcomeOK
	summary := Aggregate([]record.Record{
		testRecord("one", &ok),
		sessionEndRecord("session-1", time.Date(2026, time.August, 13, 12, 5, 0, 0, time.UTC)),
	})

	if summary.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1", summary.Invocations)
	}
	if summary.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1", summary.Sessions)
	}
	if len(summary.Primitives) != 1 {
		t.Fatalf("Primitives = %+v, want only the skill", summary.Primitives)
	}
	for _, primitive := range summary.Primitives {
		if primitive.Name == "session" {
			t.Fatalf("Primitives holds a phantom %q row: %+v", primitive.Name, primitive)
		}
	}
	// A session reports no outcome, and that absence is not an unknown-outcome
	// exclusion either: the rate is over invocations, and this was not one.
	if summary.ErrorRate.Total() != 1 || summary.ErrorRate.Excluded() != 0 {
		t.Errorf("ErrorRate = %+v, want the one invocation and nothing excluded", summary.ErrorRate)
	}
	// It does move the last-observed instant: the session was observed, later than
	// the invocation inside it.
	if want := time.Date(2026, time.August, 13, 12, 5, 0, 0, time.UTC); !summary.LastObserved.Equal(want) {
		t.Errorf("LastObserved = %v, want %v", summary.LastObserved, want)
	}
}

// TestAggregateCountsASessionWithNoInvocations is the plan §2.7 baseline made
// observable end to end: a session that invoked no primitive is a row in the session
// population, and it is exactly the row that makes every rate above it meaningful.
func TestAggregateCountsASessionWithNoInvocations(t *testing.T) {
	instant := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	summary := Aggregate([]record.Record{sessionEndRecord("session-1", instant)})

	if summary.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1", summary.Sessions)
	}
	if summary.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0", summary.Invocations)
	}
	if len(summary.Primitives) != 0 {
		t.Errorf("Primitives = %+v, want none", summary.Primitives)
	}
	if !summary.LastObserved.Equal(instant) {
		t.Errorf("LastObserved = %v, want %v", summary.LastObserved, instant)
	}
}

// TestSummaryObserved pins the question every renderer's empty state actually asks:
// did the store hold anything terminal at all? Asking it as "Invocations == 0" was
// true only while an invocation was the sole thing a record could be; the session
// grain made it a renderer bug, twice over, so the answer lives here beside the
// counts instead of being re-derived per renderer.
func TestSummaryObserved(t *testing.T) {
	ok := record.OutcomeOK
	instant := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		records []record.Record
		want    bool
	}{
		{name: "nothing", records: nil},
		{name: "an invocation", records: []record.Record{testRecord("one", &ok)}, want: true},
		{name: "a session with no primitive use", records: []record.Record{sessionEndRecord("session-1", instant)}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Aggregate(test.records).Observed(); got != test.want {
				t.Errorf("Observed() = %t, want %t", got, test.want)
			}
		})
	}
}

func sessionEndRecord(sessionID record.Identifier, at time.Time) record.Record {
	var zero int64
	return record.Record{
		SchemaVersion:    record.SchemaVersion,
		EventID:          record.DeriveEventID("claude-code", sessionID+"\x1esession_end"),
		Timestamp:        at,
		Harness:          "claude-code",
		SessionID:        sessionID,
		Repo:             "0123456789abcdef0123456789abcdef",
		Kind:             record.KindSessionEnd,
		Name:             "session",
		Invoker:          record.InvokerAuto,
		ToolCalls:        &zero,
		BuiltinToolCalls: &zero,
	}
}
