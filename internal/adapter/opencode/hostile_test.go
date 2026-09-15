package opencode

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// payloads is this adapter's own hostile corpus. It is per adapter and not shared,
// because each adapter is a new input shape and the privacy guarantee has to hold
// for every one (ADR-0007).
//
// Each payload is driven through every string field of both row types, and the
// assertion is always the same three things: nothing panics, every emitted record
// passes record.Validate, and no emitted field carries the payload.
var payloads = []struct {
	name  string
	value string
	// bounded marks a payload that is a legal value in the name, token or version
	// domain. Those three are not free text: a tool really named "mcp__evil__tool"
	// is stored under the spelling the harness used, exactly as the first adapter
	// stores a Claude Code tool of that name (record.BoundedIdentifier is the gate
	// in both). What must never happen is that such a value reaches a field whose
	// domain does not admit it, or changes what the record means — which is what
	// the assertions below separate.
	bounded bool
}{
	{name: "a 4MB string", value: strings.Repeat("A", 4<<20)},
	{name: "a traversal", value: "../../etc/passwd"},
	{name: "an absolute path", value: "/home/dev/secret/project"},
	{name: "NUL and the separator bytes", value: "\x00\x01\x1f\x1e"},
	{name: "a Claude Code tool id", value: "mcp__evil__tool", bounded: true},
	{name: "a forged scope digest", value: "scope-0123456789ab:name", bounded: true},
	{name: "a secret-shaped name", value: "sk-" + strings.Repeat("A", 48), bounded: true},
	{name: "a JSON reference", value: `{"$ref":"file:///etc/shadow"}`},
	{name: "a script tag", value: "<script>alert(1)</script>"},
	{name: "a format string", value: "%s%n"},
	{name: "invalid UTF-8", value: "\xed\xa0\x80"},
	{name: "a newline", value: "first\nsecond"},
}

// hostileSession and hostilePart put one payload into every string field of a row.
func hostileSession(payload string) Session {
	return Session{
		ID:               payload,
		Directory:        payload,
		Version:          payload,
		TokensInput:      math.MaxInt64,
		TokensOutput:     math.MaxInt64,
		TokensReasoning:  math.MaxInt64,
		TokensCacheRead:  math.MaxInt64,
		TokensCacheWrite: math.MaxInt64,
		UpdatedMS:        time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixMilli(),
		HasUpdated:       true,
		// The parentage and agent declaration too, so every assertion below covers
		// the subagent grain and not only the session grain.
		ParentID:   payload,
		Agent:      payload,
		CreatedMS:  time.Date(2026, 3, 1, 11, 59, 0, 0, time.UTC).UnixMilli(),
		HasCreated: true,
	}
}

func hostilePart(payload string) ToolPart {
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	return ToolPart{
		ID:         payload,
		SessionID:  payload,
		Tool:       payload,
		SkillName:  payload,
		Status:     payload,
		StartMS:    start,
		EndMS:      start + 1,
		HasStart:   true,
		HasEnd:     true,
		UpdatedMS:  start,
		HasUpdated: true,
	}
}

func TestHostilePayloadsNeverReachARecord(t *testing.T) {
	for _, payload := range payloads {
		t.Run(payload.name, func(t *testing.T) {
			scan := NewScan(consents, testNames(), servers("atlassian", payload.value), adapter.Staleness{Timeout: time.Minute, Now: past}, adapter.Idleness{Timeout: time.Minute, Now: past})
			from := hostileSession(payload.value)
			scan.Session(from)
			scan.Part(hostilePart(payload.value))
			// And once more with the payload only in the tool name, so a session
			// that is otherwise clean cannot smuggle one through the name.
			clean := session("ses_clean")
			scan.Session(clean)
			part := toolPart("prt_clean", payload.value, "completed")
			part.SessionID = clean.ID
			scan.Part(part)
			// And once more through the skill name, which is the one free-text key
			// this reader is licensed to read in order to derive a name (ADR-0007).
			skill := toolPart("prt_skill", "skill", "completed")
			skill.SkillName = payload.value
			skill.SessionID = clean.ID
			scan.Part(skill)

			for _, derived := range scan.Close().Records {
				if err := record.Validate(derived); err != nil {
					t.Fatalf("emitted a record that does not validate: %v", err)
				}
				assertNoPayload(t, derived, payload.value, payload.bounded)
			}
		})
	}
}

// admitsObservedText names the three fields that hold a value the harness itself
// chose, inside a bounded domain: the primitive's name, the session's id and the
// harness's version. Every other string field is Wake's own vocabulary or a
// digest, and nothing a store contains may reach one.
var admitsObservedText = map[string]bool{"Name": true, "SessionID": true, "HarnessVersion": true, "MCPServer": true}

// assertNoPayload reflects over every string-kinded field of a record and refuses
// any that carries the payload. Reflection rather than a field list, so a field
// added later is covered without anyone remembering to add it here.
//
// A bounded payload is allowed to appear in the three fields that hold observed,
// domain-checked text and nowhere else — so the assertion stays exactly as strong
// where it matters: no payload of any kind reaches an id, a hash, a kind, an
// invoker or an outcome.
func assertNoPayload(t *testing.T, derived record.Record, payload string, bounded bool) {
	t.Helper()
	value := reflect.ValueOf(derived)
	for index := range value.NumField() {
		field := value.Field(index)
		if field.Kind() != reflect.String {
			continue
		}
		name := value.Type().Field(index).Name
		if bounded && admitsObservedText[name] {
			continue
		}
		if text := field.String(); text != "" && strings.Contains(text, payload) {
			t.Errorf("field %s carries the payload", name)
		}
	}
}

func TestNoRecordCarriesAFreeTextField(t *testing.T) {
	// Every string-kinded field of every record this corpus can produce is either
	// empty or passes the record package's own validator for its domain. The record
	// type is the allowlist, and this is that claim asserted rather than assumed.
	for _, payload := range payloads {
		scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{Timeout: time.Minute, Now: past}, adapter.Idleness{Timeout: time.Minute, Now: past})
		scan.Session(hostileSession(payload.value))
		scan.Part(hostilePart(payload.value))
		for _, derived := range scan.Close().Records {
			if !record.ValidHarness(derived.Harness) {
				t.Errorf("%s: harness %q is outside its domain", payload.name, derived.Harness)
			}
			if !record.ValidName(derived.Name) {
				t.Errorf("%s: name %q is outside its domain", payload.name, derived.Name)
			}
			if !record.ValidRepo(derived.Repo) {
				t.Errorf("%s: repo %q is outside its domain", payload.name, derived.Repo)
			}
		}
	}
}

func TestAnEnormousTokenTotalDoesNotOverflow(t *testing.T) {
	from := session("ses_abc")
	from.TokensInput = math.MaxInt64
	from.TokensOutput = math.MaxInt64
	scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
	scan.Session(from)
	result := scan.Close()
	if len(result.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(result.Records))
	}
	if got := result.Records[0].InputTokens; got == nil || *got != math.MaxInt64 {
		t.Fatalf("input tokens = %v, want MaxInt64 unchanged", got)
	}
}

func TestANegativeTokenTotalIsDroppedRatherThanWritten(t *testing.T) {
	// A count below zero measures nothing. Fail closed: the record is dropped and
	// counted, never clamped into a number that would read as real.
	from := session("ses_abc")
	from.TokensInput = -1
	scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
	scan.Session(from)
	result := scan.Close()
	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want none", len(result.Records))
	}
	if result.Refused != 1 {
		t.Fatalf("refused = %d, want 1", result.Refused)
	}
}

func TestAnUnrepresentableInstantIsRefused(t *testing.T) {
	// A corrupt or hostile instant can land outside the year range JSON encodes.
	// Refusing here keeps the drop next to the counter that can explain it, rather
	// than in the store's marshaller one layer away.
	for _, instant := range []int64{math.MinInt64, math.MaxInt64} {
		part := toolPart("prt_abc", "bash", "completed")
		part.StartMS = instant
		part.HasEnd = false
		scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{}, adapter.Idleness{})
		scan.Session(session("ses_abc"))
		scan.Part(part)
		result := scan.Close()
		if len(result.Records) != 0 || result.Refused != 1 {
			t.Errorf("instant %d: records = %d, refused = %d, want 0 and 1", instant, len(result.Records), result.Refused)
		}
	}
}

func TestNoCounterCarriesAPayload(t *testing.T) {
	// This package returns no error at all — every refusal is a count — so there is
	// no message that could quote a payload. The assertion is that the counters
	// stay counters: a Result holds nothing but ints and records.
	value := reflect.TypeOf(Result{})
	for index := range value.NumField() {
		field := value.Field(index)
		if field.Name == "Records" {
			continue
		}
		if field.Type.Kind() != reflect.Int {
			t.Errorf("Result.%s is %s, and a diagnostic that is not a count could carry store content", field.Name, field.Type)
		}
	}
}

// TestAForgedScopeDigestIsRefusedAsASkillName asserts what the corpus run alone
// cannot. "scope-0123456789ab:name" is a bounded payload, so assertNoPayload skips
// the Name field for it — but the whole point of ADR-0020 is that a source value
// already wearing the keyed digest's shape is refused verbatim, or a transcript
// could merge a crafted name onto a real scope's metrics.
func TestAForgedScopeDigestIsRefusedAsASkillName(t *testing.T) {
	part := toolPart("prt_abc", "skill", "completed")
	part.SkillName = "scope-0123456789ab:name"
	result := walk(consents, NewServers(nil), part)

	if len(result.Records) != 0 {
		t.Fatalf("records = %d, want 0: a forged digest is refused verbatim", len(result.Records))
	}
	if result.Refused != 1 {
		t.Fatalf("refused = %d, want 1", result.Refused)
	}
}

// The same rule on the other derived name. The session grain is still written —
// it carries no observed name at all — and only the invocation is refused.
func TestAForgedScopeDigestIsRefusedAsASubagentName(t *testing.T) {
	from := childSession("ses_child", "ses_parent", "scope-0123456789ab:name")
	scan := NewScan(consents, testNames(), NewServers(nil), adapter.Staleness{}, adapter.Idleness{Timeout: time.Hour, Now: past})
	scan.Session(from)
	result := scan.Close()

	if len(result.Records) != 1 || result.Records[0].Kind != record.KindSessionEnd {
		t.Fatalf("records = %d, want the session grain alone", len(result.Records))
	}
	if result.Refused != 1 {
		t.Fatalf("refused = %d, want 1", result.Refused)
	}
}
