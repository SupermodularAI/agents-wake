package opencode

import (
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// past is an instant far enough after the fixture's activity that both thresholds
// below have elapsed; near is one inside them.
var (
	fixtureTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	past        = fixtureTime.Add(2 * time.Hour)
	near        = fixtureTime.Add(time.Second)
)

func TestAPendingPartIsBufferedAndNotEmitted(t *testing.T) {
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{}, adapter.Idleness{})
	scan.Session(session("ses_abc"))
	scan.Part(toolPart("prt_abc", "bash", "running"))

	if scan.Buffered() != 1 {
		t.Fatalf("buffered = %d, want 1", scan.Buffered())
	}
	result := scan.Close()
	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want none: only terminal events are emitted (ADR-0015)", len(result.Records))
	}
	if result.Pending != 1 {
		t.Fatalf("pending = %d, want 1", result.Pending)
	}
}

func TestABufferedPartBecomesInterruptedPastTheThreshold(t *testing.T) {
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{Timeout: time.Hour, Now: past}, adapter.Idleness{})
	scan.Session(session("ses_abc"))
	scan.Part(toolPart("prt_abc", "bash", "running"))
	result := scan.Close()

	if len(result.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(result.Records))
	}
	got := result.Records[0]
	if got.Outcome == nil || *got.Outcome != record.OutcomeInterrupted {
		t.Fatalf("outcome = %v, want interrupted", got.Outcome)
	}
	if got.DurationMS != nil {
		t.Fatalf("duration = %v, want nil: nothing measured an interval", *got.DurationMS)
	}
	// The same id the completed record would have carried, so a result arriving
	// later is deduplicated away rather than upserted (ADR-0004, ADR-0015).
	if want := record.DeriveEventID("opencode", "prt_abc"); got.EventID != want {
		t.Fatalf("event id = %q, want %q", got.EventID, want)
	}
	if result.Interrupted != 1 || result.Pending != 0 {
		t.Fatalf("interrupted = %d, pending = %d, want 1 and 0", result.Interrupted, result.Pending)
	}
}

func TestAnActivePartIsNotInterrupted(t *testing.T) {
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{Timeout: time.Hour, Now: near}, adapter.Idleness{})
	scan.Session(session("ses_abc"))
	scan.Part(toolPart("prt_abc", "bash", "running"))
	result := scan.Close()

	if len(result.Records) != 0 || result.Pending != 1 {
		t.Fatalf("records = %d, pending = %d, want 0 and 1", len(result.Records), result.Pending)
	}
}

func TestAnUnknownStatusIsNeverGivenUpOn(t *testing.T) {
	// An unrecognised status may well be terminal, so guessing "interrupted" would
	// write a permanent wrong record. It stays buffered and is counted as
	// blindness instead (plan §3.3, §12).
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{Timeout: time.Hour, Now: past}, adapter.Idleness{})
	scan.Session(session("ses_abc"))
	scan.Part(toolPart("prt_abc", "bash", "cancelled"))
	result := scan.Close()

	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want none", len(result.Records))
	}
	if result.UnknownOutcomes != 1 || result.Interrupted != 0 {
		t.Fatalf("unknown outcomes = %d, interrupted = %d, want 1 and 0", result.UnknownOutcomes, result.Interrupted)
	}
}

func TestAPartWithNoRegisteredSessionIsRefused(t *testing.T) {
	// Its directory is unknown, so its consent is unknown, and an unknown consent
	// is a refusal rather than an assumption (fail closed).
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{}, adapter.Idleness{})
	scan.Part(toolPart("prt_abc", "bash", "completed"))
	result := scan.Close()

	if len(result.Records) != 0 || result.Refused != 1 {
		t.Fatalf("records = %d, refused = %d, want 0 and 1", len(result.Records), result.Refused)
	}
}

func TestZeroStalenessDisablesTheRule(t *testing.T) {
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{}, adapter.Idleness{})
	scan.Session(session("ses_abc"))
	scan.Part(toolPart("prt_abc", "bash", "running"))
	result := scan.Close()

	if result.Interrupted != 0 || len(result.Records) != 0 {
		t.Fatalf("interrupted = %d, records = %d, want 0 and 0", result.Interrupted, len(result.Records))
	}
}

func TestTwoScansOverTheSameRowsProduceIdenticalRecords(t *testing.T) {
	parts := []ToolPart{
		toolPart("prt_3", "bash", "completed"),
		toolPart("prt_1", "atlassian_search", "error"),
		toolPart("prt_2", "read", "running"),
	}
	run := func() [][]byte {
		scan := NewScan(consents, servers("atlassian"), adapter.Staleness{Timeout: time.Hour, Now: past}, adapter.Idleness{Timeout: time.Hour, Now: past})
		scan.Session(session("ses_abc"))
		for _, part := range parts {
			scan.Part(part)
		}
		lines := [][]byte{}
		for _, derived := range scan.Close().Records {
			line, err := record.Marshal(derived)
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}
			lines = append(lines, line)
		}
		return lines
	}
	first, second := run(), run()
	if len(first) != len(second) {
		t.Fatalf("record counts differ: %d and %d", len(first), len(second))
	}
	for index := range first {
		if string(first[index]) != string(second[index]) {
			t.Fatalf("record %d differs between scans:\n%s\n%s", index, first[index], second[index])
		}
	}
}

func TestHarnessNamesTheSlugEveryRecordCarries(t *testing.T) {
	scan := NewScan(consents, NewServers(nil), adapter.Staleness{}, adapter.Idleness{})
	if scan.Harness() != "opencode" {
		t.Fatalf("Harness() = %q, want opencode", scan.Harness())
	}
}
