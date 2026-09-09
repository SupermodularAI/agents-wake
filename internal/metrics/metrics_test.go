package metrics

import (
	"slices"
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
	}, nil)

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
	summary := Aggregate([]record.Record{skill, builtin}, nil)

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
	summary := Aggregate([]record.Record{first, second, second}, nil)
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
	summary := Aggregate([]record.Record{direct, byAgent}, nil)
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
	}, nil)

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
	summary := Aggregate([]record.Record{sessionEndRecord("session-1", instant)}, nil)

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
			if got := Aggregate(test.records, nil).Observed(); got != test.want {
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

// TestAggregateMergesOnePrimitiveUsedInSeveralRepositoriesOntoOneRow inverts
// DG-93's split. The repository is a dimension of an invocation (ADR-0002), not the
// identity of a primitive, so one primitive used in two repositories is one row that
// names both — and its rate is recomputed over the merged population rather than
// averaged from two rendered ratios (ADR-0006, ADR-0042).
func TestAggregateMergesOnePrimitiveUsedInSeveralRepositoriesOntoOneRow(t *testing.T) {
	failed, ok := record.OutcomeError, record.OutcomeOK
	first, second := record.Hash("0123456789abcdef0123456789abcdef"), record.Hash("fedcba9876543210fedcba9876543210")
	failing, passing := testRecord("one", &failed), testRecord("two", &ok)
	failing.Repo, passing.Repo = first, second

	summary := Aggregate([]record.Record{failing, passing}, nil)

	if len(summary.Primitives) != 1 {
		t.Fatalf("primitive rows = %d, want 1; the repository is a dimension of the row, not its grain", len(summary.Primitives))
	}
	row := summary.Primitives[0]
	if !slices.Equal(row.Repos, []record.Hash{first, second}) {
		t.Fatalf("Primitives[0].Repos = %v, want both repositories in ascending order %v", row.Repos, []record.Hash{first, second})
	}
	if row.Invocations != 2 {
		t.Fatalf("Primitives[0].Invocations = %d, want 2 summed across both repositories", row.Invocations)
	}
	// Recomputed over the merged population, never averaged from two rendered rates.
	if row.ErrorRate.Numerator() != 1 || row.ErrorRate.Denominator() != 2 || row.ErrorRate.Excluded() != 0 {
		t.Fatalf("Primitives[0].ErrorRate = %+v, want 1 failure over 2 measured with none excluded", row.ErrorRate)
	}
	// The summary-level figures span every repository and are unchanged by the merge.
	if summary.Invocations != 2 || summary.ErrorRate.Denominator() != 2 {
		t.Fatalf("summary = %d invocations, denominator %d; want 2 and 2", summary.Invocations, summary.ErrorRate.Denominator())
	}
}

// worktreeRecord is testRecord under a second repository hash and a session of its
// own — the shape a linked git worktree produces, since derivation gives a worktree
// its own hash and always will (ADR-0019 §1, §3).
func worktreeRecord(source string, outcome *record.Outcome) record.Record {
	event := testRecord(source, outcome)
	event.Repo = "fedcba9876543210fedcba9876543210"
	event.SessionID = "session-2"
	return event
}

const (
	parentHash   = record.Hash("0123456789abcdef0123456789abcdef")
	worktreeHash = record.Hash("fedcba9876543210fedcba9876543210")
)

// The ticket, in the aggregation layer: one project's activity is one row, however
// many worktrees it was spread across. The record's own Repo is untouched, so
// nothing was rebuilt and grouping by wake.repo still separates them.
func TestAWorktreesInvocationsAreCountedUnderTheRepositoryItBelongsTo(t *testing.T) {
	ok := record.OutcomeOK
	summary := Aggregate([]record.Record{
		testRecord("one", &ok),
		testRecord("two", &ok),
		worktreeRecord("three", &ok),
		worktreeRecord("four", &ok),
	}, RepoRollup{string(worktreeHash): string(parentHash)})

	if len(summary.Primitives) != 1 {
		t.Fatalf("Primitives = %+v, want one row; a worktree's rows are counted under the repository", summary.Primitives)
	}
	row := summary.Primitives[0]
	if len(row.Repos) != 1 || row.Repos[0] != parentHash {
		t.Errorf("Primitives[0].Repos = %v, want only the repository's id %q; the worktree's own id must not stand beside it", row.Repos, parentHash)
	}
	if row.Invocations != 4 {
		t.Errorf("Primitives[0].Invocations = %d, want 4", row.Invocations)
	}
	if row.Sessions != 2 {
		t.Errorf("Primitives[0].Sessions = %d, want 2 distinct sessions across both", row.Sessions)
	}
}

// The reason the roll-up is applied at the counting input and not to two finished
// rows: a rate summed from two rendered ratios has no denominator of its own
// (ADR-0006), and a nil outcome excluded from one of them would be lost. The merged
// row's ratio is recomputed over the merged population, with the unknowns still
// excluded rather than folded into either side (ADR-0005).
func TestARolledUpRowRecomputesItsDenominator(t *testing.T) {
	ok, failed := record.OutcomeOK, record.OutcomeError
	summary := Aggregate([]record.Record{
		testRecord("one", &ok),
		testRecord("two", nil),
		worktreeRecord("three", &failed),
		worktreeRecord("four", nil),
		worktreeRecord("five", &ok),
	}, RepoRollup{string(worktreeHash): string(parentHash)})

	if len(summary.Primitives) != 1 {
		t.Fatalf("Primitives = %+v, want one row", summary.Primitives)
	}
	rate := summary.Primitives[0].ErrorRate
	if rate.Numerator() != 1 {
		t.Errorf("ErrorRate.Numerator() = %d, want 1", rate.Numerator())
	}
	if rate.Denominator() != 3 {
		t.Errorf("ErrorRate.Denominator() = %d, want 3", rate.Denominator())
	}
	if rate.Excluded() != 2 {
		t.Errorf("ErrorRate.Excluded() = %d, want 2; an unknown outcome is never success and never a failure", rate.Excluded())
	}
	if rate.Total() != 5 {
		t.Errorf("ErrorRate.Total() = %d, want 5", rate.Total())
	}
}

// A nil rollup is the ordinary case — no worktree registered on this machine — and
// means every repository stands alone. Under the restored grain that shows up inside
// one row: the two repositories stay two entries in its set rather than being folded
// onto one. This is the negative control for the roll-up test above.
func TestANilRollupLeavesEveryRepositoryStandingAlone(t *testing.T) {
	ok := record.OutcomeOK
	summary := Aggregate([]record.Record{
		testRecord("one", &ok),
		worktreeRecord("two", &ok),
	}, nil)

	if len(summary.Primitives) != 1 {
		t.Fatalf("Primitives = %+v, want one row", summary.Primitives)
	}
	if !slices.Equal(summary.Primitives[0].Repos, []record.Hash{parentHash, worktreeHash}) {
		t.Errorf("Primitives[0].Repos = %v, want both %q and %q standing alone", summary.Primitives[0].Repos, parentHash, worktreeHash)
	}
}

// TestAggregateCarriesTheMCPServerOntoThePrimitiveRow pins the pass-through the
// inventory join needs: an MCP tool's row has to say which server it belongs to, or
// the server can never be credited with its own tools' calls. It is a dimension of
// the row, not a grain of it — two tools of one server stay two rows.
func TestAggregateCarriesTheMCPServerOntoThePrimitiveRow(t *testing.T) {
	ok := record.OutcomeOK
	computer := testRecord("mcp-one", &ok)
	computer.Kind = record.KindMCPTool
	computer.Name = "mcp__claude-in-chrome__computer"
	computer.MCPServer = "claude-in-chrome"
	navigate := testRecord("mcp-two", &ok)
	navigate.Kind = record.KindMCPTool
	navigate.Name = "mcp__claude-in-chrome__navigate"
	navigate.MCPServer = "claude-in-chrome"

	summary := Aggregate([]record.Record{computer, navigate, testRecord("skill-one", &ok)}, nil)

	if len(summary.Primitives) != 3 {
		t.Fatalf("Primitives = %+v, want three rows", summary.Primitives)
	}
	servers := 0
	for _, primitive := range summary.Primitives {
		if primitive.Kind == record.KindMCPTool {
			servers++
			if primitive.MCPServer != "claude-in-chrome" {
				t.Errorf("%q MCPServer = %q, want %q", primitive.Name, primitive.MCPServer, "claude-in-chrome")
			}
			continue
		}
		if primitive.MCPServer != "" {
			t.Errorf("%q MCPServer = %q, want it absent on a non-MCP row", primitive.Name, primitive.MCPServer)
		}
	}
	if servers != 2 {
		t.Fatalf("MCP tool rows = %d, want 2", servers)
	}
}
