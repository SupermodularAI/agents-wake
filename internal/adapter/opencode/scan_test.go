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

// TestAPartWithNoStartInstantIsRefused pins the rule that separates an instant the
// harness recorded from one nothing recorded. A part whose state.time.start is
// absent has no time of its own, and a substituted epoch would be a measurement
// nothing measured — indistinguishable, once written, from a call that really
// happened on 1 January 1970. It is refused and counted instead, which is what
// makes the loss visible to doctor rather than silent (plan §3.3, §12), and it is
// the answer the first adapter already gives an entry with no timestamp
// (internal/adapter/claudecode/reader.go, transcriptEntry.valid).
func TestAPartWithNoStartInstantIsRefused(t *testing.T) {
	part := toolPart("prt_abc", "bash", "completed")
	part.StartMS, part.HasStart = 0, false

	result := walk(consents, NewServers(nil), part)

	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want 0: a part with no start instant has no time to stamp", len(result.Records))
	}
	if result.Refused != 1 {
		t.Fatalf("refused = %d, want 1: the loss has to be counted, not silent", result.Refused)
	}
}

// A part with no start instant is refused whether or not its status is terminal:
// buffering it would only defer the same fabrication to Close.
func TestAPendingPartWithNoStartInstantIsRefusedRatherThanBuffered(t *testing.T) {
	part := toolPart("prt_abc", "bash", "running")
	part.StartMS, part.HasStart = 0, false

	scan := NewScan(consents, NewServers(nil), adapter.Staleness{Timeout: time.Hour, Now: past}, adapter.Idleness{})
	scan.Session(session("ses_abc"))
	scan.Part(part)
	result := scan.Close()

	if len(result.Records) != 0 || result.Refused != 1 {
		t.Fatalf("records = %d, refused = %d, want 0 and 1", len(result.Records), result.Refused)
	}
	if scan.Buffered() != 0 || result.Pending != 0 || result.Interrupted != 0 {
		t.Fatalf("buffered = %d, pending = %d, interrupted = %d, want 0, 0 and 0",
			scan.Buffered(), result.Pending, result.Interrupted)
	}
}
