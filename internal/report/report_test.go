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
		"Only currently discovered, non-built-in primitives are listed.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Bash") {
		t.Fatalf("report included built-in activity:\n%s", text)
	}
	// The default omits the unused section entirely: it is a deliberate ask
	// (--unused), not a second table every plain `wake report` scrolls past.
	if strings.Contains(text, "UNUSED PRIMITIVES") || strings.Contains(text, "unused-skill") {
		t.Fatalf("wake report with no flags showed unused primitives:\n%s", text)
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
	if err := Render(&output, metrics.Aggregate(nil), available, Options{Usage: true}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(output.String(), "Last observed         not observed") || strings.Contains(output.String(), "0001-01-01") {
		t.Fatalf("zero-activity report = %s", output.String())
	}
}

// A bare `--unused` asks what was never invoked, not how much invocation
// happened — so its OVERVIEW answers that question directly (a count by
// kind) instead of showing the invocation/outcome overview a usage view
// needs.
func TestRenderUnusedOnlyReplacesInvocationOverviewWithUnusedCountsByKind(t *testing.T) {
	ok := record.OutcomeOK
	summary := metrics.Aggregate([]record.Record{reportRecord("used", &ok)})
	available := []inventory.Usage{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "used", Invocations: 1, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "unused-skill-1"},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "unused-skill-2"},
		{Harness: "claude-code", Kind: record.KindMCPTool, Name: "unused-tool"},
	}

	var output bytes.Buffer
	if err := Render(&output, summary, available, Options{Unused: true}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text := output.String()
	for _, unwanted := range []string{"Terminal invocations", "Error rate", "\nOUTCOMES\n"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("unused-only report still shows the invocation overview/outcomes (%q):\n%s", unwanted, text)
		}
	}
	if !strings.Contains(text, "\nOVERVIEW\n") {
		t.Fatalf("unused-only report dropped OVERVIEW instead of repurposing it:\n%s", text)
	}
	if line := lineStartingWith(text, "Skill"); !strings.Contains(line, "2") {
		t.Fatalf("OVERVIEW's Skill count = %q, want 2:\n%s", line, text)
	}
	if line := lineStartingWith(text, "Mcp tool"); !strings.Contains(line, "1") {
		t.Fatalf("OVERVIEW's Mcp tool count = %q, want 1:\n%s", line, text)
	}
	if line := lineStartingWith(text, "Total unused"); !strings.Contains(line, "3") {
		t.Fatalf("OVERVIEW's Total unused = %q, want 3:\n%s", line, text)
	}
}

// When every discovered primitive has activity, the unused overview stays
// silent rather than printing a second copy of what UNUSED PRIMITIVES
// already says.
func TestRenderUnusedOnlyOmitsOverviewWhenNothingIsUnused(t *testing.T) {
	ok := record.OutcomeOK
	summary := metrics.Aggregate([]record.Record{reportRecord("used", &ok)})
	available := []inventory.Usage{{Harness: "claude-code", Kind: record.KindSkill, Name: "used", Invocations: 1, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)}}

	var output bytes.Buffer
	if err := Render(&output, summary, available, Options{Unused: true}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text := output.String()
	if strings.Contains(text, "\nOVERVIEW\n") {
		t.Fatalf("unused-only report showed an empty OVERVIEW:\n%s", text)
	}
	if !strings.Contains(text, "Every discovered primitive has activity.") {
		t.Fatalf("unused-only report lost the empty-unused message:\n%s", text)
	}
}

func lineStartingWith(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return line
		}
	}
	return ""
}

func TestRenderPrettyDrawsColorAndBoxedTablesOnlyWhenAsked(t *testing.T) {
	ok := record.OutcomeOK
	summary := metrics.Aggregate([]record.Record{reportRecord("review", &ok)})
	available := []inventory.Usage{{Harness: "claude-code", Kind: record.KindSkill, Name: "review", Invocations: 1, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)}}

	var plain bytes.Buffer
	if err := Render(&plain, summary, available, Options{}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if strings.ContainsAny(plain.String(), "\x1b") || strings.Contains(plain.String(), "┌") {
		t.Fatalf("plain report carried ANSI or box-drawing output:\n%s", plain.String())
	}

	var pretty bytes.Buffer
	if err := Render(&pretty, summary, available, Options{Pretty: true}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text := pretty.String()
	if !strings.Contains(text, "\x1b[") {
		t.Fatalf("pretty report carried no ANSI styling:\n%s", text)
	}
	for _, want := range []string{"┌", "┬", "┐", "│", "└", "┴", "┘"} {
		if !strings.Contains(text, want) {
			t.Fatalf("pretty report missing box-drawing %q:\n%s", want, text)
		}
	}
	// A pretty report is the same content as a plain one with styling stripped.
	stripped := stripANSI(text)
	for _, want := range []string{"WAKE REPORT", "OVERVIEW", "OUTCOMES", "USED PRIMITIVES", "review"} {
		if !strings.Contains(stripped, want) {
			t.Fatalf("pretty report lost content %q once styling is stripped:\n%s", want, stripped)
		}
	}
}

func stripANSI(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '\x1b' {
			for i < len(text) && text[i] != 'm' {
				i++
			}
			continue
		}
		out.WriteByte(text[i])
	}
	return out.String()
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
