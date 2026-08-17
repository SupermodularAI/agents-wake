package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

func TestRenderShowsCurrentMetricsAndPrimitiveActivity(t *testing.T) {
	ok := record.OutcomeOK
	failed := record.OutcomeError
	builtin := reportRecord("builtin", &ok)
	builtin.Kind = record.KindBuiltinTool
	builtin.Name = "Bash"
	summary := metrics.Aggregate([]record.Record{reportRecord("review", &ok), reportRecord("retry", nil), reportRecord("failed", &failed), builtin})

	var output bytes.Buffer
	available := []inventory.Usage{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "review", Invocations: 3, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "unused-skill"},
	}
	if err := Render(&output, summary, available, Options{}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text := output.String()
	for _, want := range []string{
		"WAKE REPORT",
		"Terminal invocations  4",
		"Error rate",
		"33.3% (1/3 known; 1 unknown)",
		"Health coverage",
		"3 known; 1 unknown excluded",
		"unknown  1 (excluded from health rates)",
		"review",
		"USED PRIMITIVES",
		"UNUSED PRIMITIVES",
		"unused-skill",
		"Only currently discovered, non-built-in primitives are listed.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Bash") {
		t.Fatalf("report included built-in activity:\n%s", text)
	}
}

func TestRenderRespectsPrimitiveSectionFilters(t *testing.T) {
	available := []inventory.Usage{{Harness: "claude-code", Kind: record.KindSkill, Name: "used", Invocations: 1, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)}, {Harness: "claude-code", Kind: record.KindSkill, Name: "unused"}}
	for _, test := range []struct {
		name    string
		options Options
		want    string
		omit    string
	}{
		{name: "usage", options: Options{Usage: true}, want: "\nUSED PRIMITIVES\n", omit: "\nUNUSED PRIMITIVES\n"},
		{name: "unused", options: Options{Unused: true}, want: "\nUNUSED PRIMITIVES\n", omit: "\nUSED PRIMITIVES\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := Render(&output, metrics.Aggregate([]record.Record{reportRecord("used", nil)}), available, test.options); err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if !strings.Contains(output.String(), test.want) || strings.Contains(output.String(), test.omit) {
				t.Fatalf("section filter output = %s", output.String())
			}
		})
	}
}

func TestRenderShowsHelpfulEmptyState(t *testing.T) {
	var output bytes.Buffer
	if err := Render(&output, metrics.Aggregate(nil), nil, Options{}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(output.String(), "No terminal events yet. Run `wake ingest`") {
		t.Fatalf("empty report = %q", output.String())
	}
}

func TestRenderLabelsAbsentActivityWithoutAZeroTimestamp(t *testing.T) {
	var output bytes.Buffer
	available := []inventory.Usage{{Harness: "claude-code", Kind: record.KindSkill, Name: "unused"}}
	if err := Render(&output, metrics.Aggregate(nil), available, Options{Unused: true}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(output.String(), "Last observed         not observed") || strings.Contains(output.String(), "0001-01-01") {
		t.Fatalf("zero-activity report = %s", output.String())
	}
}

func reportRecord(id string, outcome *record.Outcome) record.Record {
	return record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID("claude-code", record.Identifier(id)),
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
