package opencode

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// consentedRepo is a repository id in the domain record.ValidRepo admits: the
// truncated keyed digest internal/config derives, 32 hex characters.
const consentedRepo = record.Hash("0123456789abcdef0123456789abcdef")

// consents is a resolver that admits every directory. Consent itself is the
// caller's to answer (ADR-0025); the reader only ever sees the answer.
func consents(string, time.Time) (record.Hash, bool) { return consentedRepo, true }

// declines is its refusal: a directory belonging to no consented repository.
func declines(string, time.Time) (record.Hash, bool) { return "", false }

func session(id string) Session {
	return Session{
		ID:               id,
		Directory:        "/home/dev/project",
		Version:          "1.18.30",
		TokensInput:      11,
		TokensOutput:     13,
		TokensReasoning:  17,
		TokensCacheRead:  19,
		TokensCacheWrite: 23,
		UpdatedMS:        time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixMilli(),
	}
}

func toolPart(id, tool, status string) ToolPart {
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	return ToolPart{
		ID:        id,
		SessionID: "ses_abc",
		Tool:      tool,
		Status:    status,
		StartMS:   start,
		EndMS:     start + 250,
		HasStart:  true,
		HasEnd:    true,
		UpdatedMS: start,
	}
}

// walk drives one scan over one session and the parts given, with consent
// granted, nothing configured as an MCP server, and both thresholds disabled.
func walk(resolve adapter.Resolver, servers Servers, parts ...ToolPart) Result {
	scan := NewScan(resolve, servers, adapter.Staleness{}, adapter.Idleness{})
	scan.Session(session("ses_abc"))
	for _, part := range parts {
		scan.Part(part)
	}
	return scan.Close()
}

func TestACompletedToolPartDerivesOneRecord(t *testing.T) {
	result := walk(consents, NewServers(nil), toolPart("prt_abc", "bash", "completed"))
	if len(result.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(result.Records))
	}
	got := result.Records[0]

	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	duration := int64(250)
	outcome := record.OutcomeOK
	want := record.Record{
		SchemaVersion:  record.SchemaVersion,
		EventID:        record.DeriveEventID("opencode", "prt_abc"),
		Timestamp:      record.NormalizedTimestamp(start),
		Harness:        "opencode",
		HarnessVersion: "1.18.30",
		SessionID:      "ses_abc",
		Repo:           consentedRepo,
		Kind:           record.KindBuiltinTool,
		Name:           "bash",
		Invoker:        record.InvokerModel,
		Outcome:        &outcome,
		DurationMS:     &duration,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record = %+v\nwant %+v", got, want)
	}
}

func TestEventIDDerivesFromPartID(t *testing.T) {
	result := walk(consents, NewServers(nil), toolPart("prt_abc", "bash", "completed"))
	if want := record.DeriveEventID("opencode", record.Identifier("prt_abc")); result.Records[0].EventID != want {
		t.Fatalf("event id = %q, want %q", result.Records[0].EventID, want)
	}
}

func TestDistinctPartsNeverShareAnEventID(t *testing.T) {
	// The measured collision shape: three invocations in three sessions whose
	// provider call ids all read "bash:1" and whose part ids differ. Deriving from
	// callID would fold three invocations into one record and no number would ever
	// say so (ADR-0004).
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{}, adapter.Idleness{})
	for _, id := range []string{"ses_1", "ses_2", "ses_3"} {
		registered := session(id)
		scan.Session(registered)
	}
	for index, id := range []string{"prt_1", "prt_2", "prt_3"} {
		part := toolPart(id, "bash", "completed")
		part.SessionID = fmt.Sprintf("ses_%d", index+1)
		scan.Part(part)
	}
	result := scan.Close()
	if len(result.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(result.Records))
	}
	ids := map[record.Hash]struct{}{}
	for _, derived := range result.Records {
		ids[derived.EventID] = struct{}{}
	}
	if len(ids) != 3 {
		t.Fatalf("distinct event ids = %d over 3 records", len(ids))
	}

	t.Run("over a generated corpus", func(t *testing.T) {
		corpus := NewScan(consents, NewServers(nil), adapter.Staleness{}, adapter.Idleness{})
		corpus.Session(session("ses_abc"))
		for index := range 5000 {
			corpus.Part(toolPart(fmt.Sprintf("prt_%04d", index), "bash", "completed"))
		}
		derived := corpus.Close()
		unique := map[record.Hash]struct{}{}
		for _, row := range derived.Records {
			unique[row.EventID] = struct{}{}
		}
		if len(unique) != 5000 {
			t.Fatalf("distinct event ids = %d over 5000 parts", len(unique))
		}
	})
}

func TestRescanningDerivesIdenticalRecords(t *testing.T) {
	parts := []ToolPart{
		toolPart("prt_1", "bash", "completed"),
		toolPart("prt_2", "atlassian_search", "error"),
		toolPart("prt_3", "read", "running"),
	}
	first := walk(consents, servers("atlassian"), parts...)
	second := walk(consents, servers("atlassian"), parts...)

	if len(first.Records) != len(second.Records) {
		t.Fatalf("record counts differ: %d and %d", len(first.Records), len(second.Records))
	}
	for index := range first.Records {
		left, err := record.Marshal(first.Records[index])
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		right, err := record.Marshal(second.Records[index])
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		if string(left) != string(right) {
			t.Fatalf("record %d differs between scans:\n%s\n%s", index, left, right)
		}
	}
}

func TestAnMCPToolCarriesItsServer(t *testing.T) {
	result := walk(consents, servers("atlassian", "notion"), toolPart("prt_abc", "atlassian_search", "completed"))
	got := result.Records[0]
	if got.Kind != record.KindMCPTool || got.MCPServer != "atlassian" || got.Name != "atlassian_search" {
		t.Fatalf("record kind/server/name = %q/%q/%q, want mcp_tool/atlassian/atlassian_search", got.Kind, got.MCPServer, got.Name)
	}
}

func TestABuiltinShapedLikeAPrefixIsABuiltinTool(t *testing.T) {
	result := walk(consents, servers("atlassian", "notion"), toolPart("prt_abc", "apply_patch", "completed"))
	got := result.Records[0]
	if got.Kind != record.KindBuiltinTool || got.MCPServer != "" {
		t.Fatalf("record kind/server = %q/%q, want builtin_tool and no server", got.Kind, got.MCPServer)
	}
}

func TestDurationIsTheExactInterval(t *testing.T) {
	// Two instants the harness itself recorded, so the interval is exact rather
	// than derived from a pair of surrounding events.
	part := toolPart("prt_abc", "bash", "completed")
	part.EndMS = part.StartMS + 1234
	result := walk(consents, NewServers(nil), part)
	if got := result.Records[0].DurationMS; got == nil || *got != 1234 {
		t.Fatalf("duration = %v, want 1234", got)
	}
}

func TestAPartWithNoEndHasNoDuration(t *testing.T) {
	part := toolPart("prt_abc", "bash", "completed")
	part.HasEnd = false
	part.EndMS = 0
	result := walk(consents, NewServers(nil), part)
	if got := result.Records[0].DurationMS; got != nil {
		t.Fatalf("duration = %v, want nil where the harness reported no end", *got)
	}
}

func TestAnEndBeforeItsStartLeavesDurationNil(t *testing.T) {
	// A clamped 0 would be a measurement, and nothing measured this (ADR-0027).
	part := toolPart("prt_abc", "bash", "completed")
	part.EndMS = part.StartMS - 5
	result := walk(consents, NewServers(nil), part)
	if got := result.Records[0].DurationMS; got != nil {
		t.Fatalf("duration = %v, want nil", *got)
	}
	if result.OutOfOrderPairs != 1 {
		t.Fatalf("out-of-order pairs = %d, want 1", result.OutOfOrderPairs)
	}
}

func TestAnUnnameableToolIsRefusedAndDropped(t *testing.T) {
	result := walk(consents, NewServers(nil), toolPart("prt_abc", "../etc/passwd", "completed"))
	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want none", len(result.Records))
	}
	if result.Refused != 1 {
		t.Fatalf("refused = %d, want 1", result.Refused)
	}
}

func TestASessionIDOutsideTheTokenDomainIsRefused(t *testing.T) {
	part := toolPart("prt_abc", "bash", "completed")
	part.SessionID = "ses/abc"
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{}, adapter.Idleness{})
	registered := session("ses/abc")
	scan.Session(registered)
	scan.Part(part)
	result := scan.Close()
	if len(result.Records) != 0 || result.Refused != 1 {
		t.Fatalf("records = %d, refused = %d, want 0 and 1", len(result.Records), result.Refused)
	}
}

func TestAnUnconsentedDirectoryDerivesNothing(t *testing.T) {
	result := walk(declines, NewServers(nil), toolPart("prt_abc", "bash", "completed"))
	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want none", len(result.Records))
	}
	if result.Refused != 0 {
		t.Fatalf("refused = %d, want 0: an unconsented directory is an honest zero, not lost collection", result.Refused)
	}
	if result.SkippedSources != 1 {
		t.Fatalf("skipped sources = %d, want 1", result.SkippedSources)
	}
}

func TestEveryDerivedRecordValidates(t *testing.T) {
	parts := []ToolPart{
		toolPart("prt_1", "bash", "completed"),
		toolPart("prt_2", "atlassian_search", "error"),
		toolPart("prt_3", "notion_fetch", "completed"),
	}
	scan := NewScan(consents, servers("atlassian", "notion"), adapter.Staleness{Timeout: time.Minute, Now: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)}, adapter.Idleness{Timeout: time.Minute, Now: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)})
	scan.Session(session("ses_abc"))
	for _, part := range parts {
		scan.Part(part)
	}
	scan.Part(toolPart("prt_4", "read", "running"))
	result := scan.Close()
	if len(result.Records) == 0 {
		t.Fatal("no records derived")
	}
	for _, derived := range result.Records {
		if err := record.Validate(derived); err != nil {
			t.Errorf("record %+v failed validation: %v", derived, err)
		}
	}
}
