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
