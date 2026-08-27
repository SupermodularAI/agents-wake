package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/repolabel"
)

func TestRenderShowsPrimitiveActivityAndLastObserved(t *testing.T) {
	ok := record.OutcomeOK
	failed := record.OutcomeError
	builtin := reportRecord("builtin", &ok)
	builtin.Kind = record.KindBuiltinTool
	builtin.Name = "Bash"
	summary := metrics.Aggregate([]record.Record{reportRecord("review", &ok), reportRecord("retry", nil), reportRecord("failed", &failed), builtin})

	var output bytes.Buffer
	available := []inventory.Usage{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "review", Repo: "0123456789abcdef0123456789abcdef", Invocations: 3, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "unused-skill"},
	}
	if err := Render(&output, summary, available, Options{}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text := output.String()
	for _, want := range []string{
		"WAKE REPORT",
		"Last observed: 2026-08-13T12:00:00Z",
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

func TestRenderShowsPerPrimitiveErrorsAsCountAndPercentage(t *testing.T) {
	summary := metrics.Aggregate(nil)
	available := []inventory.Usage{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "flaky", Repo: "0123456789abcdef0123456789abcdef", Invocations: 4, Failures: 1, Unknown: 1, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "solid", Repo: "0123456789abcdef0123456789abcdef", Invocations: 2, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)},
	}

	var output bytes.Buffer
	if err := Render(&output, summary, available, Options{}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text := output.String()
	// flaky: 1 failure out of 3 known (4 invocations, 1 excluded as unknown).
	if !strings.Contains(text, "flaky") || !strings.Contains(text, "33.3%") {
		t.Fatalf("report missing flaky primitive's error rate:\n%s", text)
	}
	if line := lineStartingWith(text, "solid"); !strings.Contains(line, "0") || strings.Contains(line, "%") {
		t.Fatalf("failure-free primitive should report a bare 0, got %q:\n%s", line, text)
	}
}

func TestRenderRespectsPrimitiveSectionFilters(t *testing.T) {
	available := []inventory.Usage{{Harness: "claude-code", Kind: record.KindSkill, Name: "used", Repo: "0123456789abcdef0123456789abcdef", Invocations: 1, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)}, {Harness: "claude-code", Kind: record.KindSkill, Name: "unused"}}
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

// TestRenderDoesNotClaimAnEmptyStoreForASessionWithNoPrimitiveUse is the plan §2.7
// baseline at the renderer. A session that invoked no primitive contributes nothing
// to Invocations by design, so a gate keyed on Invocations alone calls the store
// empty and prints "No terminal events yet." over a terminal session_end that was
// read, written, and delivered — while `wake` root reports the same session as
// observed. The session population is the one this report exists to expose.
func TestRenderDoesNotClaimAnEmptyStoreForASessionWithNoPrimitiveUse(t *testing.T) {
	instant := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	if err := Render(&output, metrics.Aggregate([]record.Record{sessionEndRecord("session-1", instant)}), nil, Options{}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text := output.String()
	if strings.Contains(text, "No terminal events yet") {
		t.Fatalf("report called a store holding a session_end empty:\n%s", text)
	}
	if !strings.Contains(text, "Last observed: 2026-08-13T12:00:00Z") {
		t.Fatalf("report did not show the observed session:\n%s", text)
	}
}

func TestRenderLabelsAbsentActivityWithoutAZeroTimestamp(t *testing.T) {
	var output bytes.Buffer
	available := []inventory.Usage{{Harness: "claude-code", Kind: record.KindSkill, Name: "unused"}}
	if err := Render(&output, metrics.Aggregate(nil), available, Options{Usage: true}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(output.String(), "Last observed: not observed") || strings.Contains(output.String(), "0001-01-01") {
		t.Fatalf("zero-activity report = %s", output.String())
	}
}

// A bare `--unused` asks what was never invoked, so it gets its own OVERVIEW —
// a count of unused primitives by kind — rather than the usage view's tables.
func TestRenderUnusedOnlyReplacesInvocationOverviewWithUnusedCountsByKind(t *testing.T) {
	ok := record.OutcomeOK
	summary := metrics.Aggregate([]record.Record{reportRecord("used", &ok)})
	available := []inventory.Usage{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "used", Repo: "0123456789abcdef0123456789abcdef", Invocations: 1, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)},
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
	available := []inventory.Usage{{Harness: "claude-code", Kind: record.KindSkill, Name: "used", Repo: "0123456789abcdef0123456789abcdef", Invocations: 1, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)}}

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

func linesStartingWith(text, prefix string) []string {
	var found []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			found = append(found, line)
		}
	}
	return found
}

// TestRenderShowsOneRowPerRepositoryWithItsLabel is DG-93 at the terminal: the
// snapshot's grain is per repository, so one primitive used in two of them is two
// rows, and each row names which repository it is — by label where one is recorded,
// by a readable form of the id otherwise. Never a blank cell (plan §4.5).
func TestRenderShowsOneRowPerRepositoryWithItsLabel(t *testing.T) {
	const labelled, unlabelled = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	available := []inventory.Usage{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "review", Repo: labelled, Invocations: 2, LastUsed: at},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "review", Repo: unlabelled, Invocations: 1, LastUsed: at},
	}

	var output bytes.Buffer
	options := Options{Usage: true, Labels: repolabel.Labels{labelled: "agents-wake"}}
	if err := Render(&output, metrics.Aggregate(nil), available, options); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text := output.String()
	for _, want := range []string{"REPO", "agents-wake", "repo-fedcba987654"} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
	rows := linesStartingWith(text, "review")
	if len(rows) != 2 {
		t.Fatalf("rows for `review` = %d, want one per repository:\n%s", len(rows), text)
	}
	// By field position on the plain rendering: PRIMITIVE, TYPE, HARNESS, REPO.
	for _, row := range rows {
		fields := strings.Fields(row)
		if len(fields) < 4 || fields[3] == "" {
			t.Fatalf("row %q has no repository cell", row)
		}
	}
}

// TestRenderShowsNoRepositoryColumnForUnusedPrimitives pins where the column does
// not belong. An unused primitive has zero invocations, and a repository is a
// property of an invocation (ADR-0002) — the column there would be the empty one
// plan §4.5 forbids.
func TestRenderShowsNoRepositoryColumnForUnusedPrimitives(t *testing.T) {
	available := []inventory.Usage{{Harness: "claude-code", Kind: record.KindSkill, Name: "unused"}}

	var output bytes.Buffer
	if err := Render(&output, metrics.Aggregate(nil), available, Options{Unused: true}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	// The table's own header row, not the whole report: "WAKE REPORT" contains
	// "REPO", and the closing disclaimer names repository paths.
	header := lineStartingWith(output.String(), "PRIMITIVE")
	if header == "" {
		t.Fatalf("unused-only report has no primitive table:\n%s", output.String())
	}
	if strings.Contains(header, "REPO") {
		t.Fatalf("unused table header carries a repository column: %q", header)
	}
}

// TestRenderMakesNoClaimThatRepositoryLabelsAreNeverShown keeps the closing
// disclaimer true now that a label can appear in a cell. The claim about paths is
// unaffected and stays absolute (ADR-0033 §4).
func TestRenderMakesNoClaimThatRepositoryLabelsAreNeverShown(t *testing.T) {
	available := []inventory.Usage{{Harness: "claude-code", Kind: record.KindSkill, Name: "unused"}}

	var output bytes.Buffer
	if err := Render(&output, metrics.Aggregate(nil), available, Options{}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text := output.String()
	if strings.Contains(text, "repository labels") {
		t.Fatalf("report still claims repository labels are never shown:\n%s", text)
	}
	if !strings.Contains(text, "paths") {
		t.Fatalf("report dropped its claim about paths:\n%s", text)
	}
}

func TestRenderPrettyDrawsColorAndBoxedTablesOnlyWhenAsked(t *testing.T) {
	ok := record.OutcomeOK
	summary := metrics.Aggregate([]record.Record{reportRecord("review", &ok)})
	available := []inventory.Usage{{Harness: "claude-code", Kind: record.KindSkill, Name: "review", Repo: "0123456789abcdef0123456789abcdef", Invocations: 1, LastUsed: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)}}

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
	for _, want := range []string{"WAKE REPORT", "Last observed:", "USED PRIMITIVES", "review"} {
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

// sessionEndRecord is a session that invoked nothing: no outcome, no duration, and
// zero counted calls (ADR-0002's session grain).
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
