//go:build remote

package remote

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/version"
)

// frozenEncoderImports is the encoder's entire import set, declared here and
// nowhere else. Asserting the encoder against a list it exports itself would be
// vacuous, so the literal lives in the test (ADR-0012: the wire is governed by
// the same allowlist discipline as the disk).
var frozenEncoderImports = []string{
	"encoding/json",
	"errors",
	"github.com/SupermodularAI/agents-wake/internal/record",
	"github.com/SupermodularAI/agents-wake/internal/version",
	"math",
	"strconv",
}

// forbiddenEncoderImports names the capabilities the encoder must not have. The
// set-equality assertion above already excludes them; naming them again makes a
// regression report which capability leaked in rather than only that a set
// differed (AC #2, BC-1, BC-10).
var forbiddenEncoderImports = []string{
	"net", "net/http", "os", "io", "path/filepath", "bufio",
}

func ptr[T any](v T) *T { return &v }

// decodePayload unmarshals into map[string]any rather than the encoder's own
// wire types on purpose: a receiver sees JSON, so the assertions must too. It is
// also what lets a test tell a proto3 JSON string ("1500") from a number.
func decodePayload(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	return decoded
}

func child(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is %T, want an object", key, parent[key])
	}
	return value
}

func objects(t *testing.T, parent map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := parent[key].([]any)
	if !ok {
		t.Fatalf("%q is %T, want an array", key, parent[key])
	}
	result := make([]map[string]any, 0, len(raw))
	for i, element := range raw {
		object, ok := element.(map[string]any)
		if !ok {
			t.Fatalf("%q[%d] is %T, want an object", key, i, element)
		}
		result = append(result, object)
	}
	return result
}

func scopeSpansOf(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	resourceSpans := objects(t, decodePayload(t, payload), "resourceSpans")
	if len(resourceSpans) != 1 {
		t.Fatalf("resourceSpans has %d entries, want 1", len(resourceSpans))
	}
	scopeSpans := objects(t, resourceSpans[0], "scopeSpans")
	if len(scopeSpans) != 1 {
		t.Fatalf("scopeSpans has %d entries, want 1", len(scopeSpans))
	}
	return scopeSpans[0]
}

func spansOf(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	return objects(t, scopeSpansOf(t, payload), "spans")
}

// encodeOne encodes exactly one record and fails unless it survived, so a test
// about a span's contents cannot silently pass against a dropped record.
func encodeOne(t *testing.T, r record.Record) map[string]any {
	t.Helper()
	payload, dropped, err := Encode([]record.Record{r})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if dropped != 0 {
		t.Fatalf("Encode() dropped = %d, want 0", dropped)
	}
	spans := spansOf(t, payload)
	if len(spans) != 1 {
		t.Fatalf("Encode() emitted %d spans, want 1", len(spans))
	}
	return spans[0]
}

// attributesOf flattens an OTLP attribute array into key -> value object.
func attributesOf(t *testing.T, owner map[string]any, key string) map[string]map[string]any {
	t.Helper()
	flattened := make(map[string]map[string]any)
	for _, attribute := range objects(t, owner, key) {
		name, ok := attribute["key"].(string)
		if !ok {
			t.Fatalf("attribute key is %T, want a string", attribute["key"])
		}
		if _, duplicate := flattened[name]; duplicate {
			t.Fatalf("attribute %q emitted twice", name)
		}
		flattened[name] = child(t, attribute, "value")
	}
	return flattened
}

func attributeKeys(attributes map[string]map[string]any) []string {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// validRecord is the minimal record the encoder accepts: every optional field
// left at its zero value or nil.
func validRecord() record.Record {
	return record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID("claude-code", "source-event-1"),
		Timestamp:     time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC),
		Harness:       "claude-code",
		SessionID:     "session-1",
		Repo:          "0123456789abcdef0123456789abcdef",
		Kind:          record.KindSkill,
		Name:          "commit-message",
		Invoker:       record.InvokerModel,
	}
}

// fullRecord populates every optional field, so a key-set assertion against it
// sees the encoder's complete output shape.
func fullRecord() record.Record {
	r := validRecord()
	r.EventID = record.DeriveEventID("claude-code", "source-event-2")
	r.HarnessVersion = "2.0.1"
	r.Package = "atlassian"
	r.PackageVersion = "1.4.0"
	r.Source = ptr(record.SourceMarketplace)
	r.ViaSkill = "commit-message"
	r.ViaAgent = "sdlc-plan"
	r.Model = "claude-opus-5"
	r.Effort = "high"
	r.Outcome = ptr(record.OutcomeOK)
	r.DurationMS = ptr(int64(1500))
	return r
}

// TestFixturesAreValidRecords guards the rest of the suite: the encoder drops a
// record that fails record.Validate, so a fixture that silently stopped being
// valid would turn every downstream assertion into a test of the empty payload.
func TestFixturesAreValidRecords(t *testing.T) {
	for name, r := range map[string]record.Record{"validRecord": validRecord(), "fullRecord": fullRecord()} {
		if err := record.Validate(r); err != nil {
			t.Fatalf("%s() is not a valid record: %v", name, err)
		}
	}
}

func TestEncoderImportsAreFrozen(t *testing.T) {
	// Reading the encoder's own source is test scaffolding, not encoder I/O:
	// the assertion is that otlp.go itself performs none.
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "otlp.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing otlp.go: %v", err)
	}

	imports := make([]string, 0, len(parsed.Imports))
	for _, spec := range parsed.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquoting import path %s: %v", spec.Path.Value, err)
		}
		imports = append(imports, path)
	}
	slices.Sort(imports)

	if !slices.Equal(imports, frozenEncoderImports) {
		t.Fatalf("encoder imports = %v, frozen allowlist = %v", imports, frozenEncoderImports)
	}
	for _, forbidden := range forbiddenEncoderImports {
		if slices.Contains(imports, forbidden) {
			t.Fatalf("encoder imports %q: the encoder must perform no I/O", forbidden)
		}
	}
}

func TestEncodeEmptyInputProducesEmptySpanArray(t *testing.T) {
	payload, dropped, err := Encode(nil)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if dropped != 0 {
		t.Fatalf("Encode() dropped = %d, want 0", dropped)
	}
	if !bytes.Contains(payload, []byte(`"spans":[]`)) {
		t.Fatalf("Encode() payload = %s, want an empty spans array", payload)
	}
	if bytes.Contains(payload, []byte(`"spans":null`)) {
		t.Fatalf("Encode() payload = %s, want [] rather than null", payload)
	}
}

func TestTraceIDGroupsOneSession(t *testing.T) {
	first := validRecord()
	second := validRecord()
	second.EventID = record.DeriveEventID("claude-code", "source-event-9")

	firstSpan, secondSpan := encodeOne(t, first), encodeOne(t, second)
	if firstSpan["traceId"] != secondSpan["traceId"] {
		t.Fatalf("traceId = %v and %v, want one trace per session", firstSpan["traceId"], secondSpan["traceId"])
	}
	if firstSpan["spanId"] == secondSpan["spanId"] {
		t.Fatalf("spanId = %v for two distinct events", firstSpan["spanId"])
	}

	third := second
	third.SessionID = "session-2"
	if thirdSpan := encodeOne(t, third); thirdSpan["traceId"] == secondSpan["traceId"] {
		t.Fatalf("traceId = %v for two distinct sessions", thirdSpan["traceId"])
	}
}

func TestSpanIDIsPureFunctionOfEventID(t *testing.T) {
	// Two records differing in every field the span id must not depend on.
	first := fullRecord()
	second := validRecord()
	second.EventID = first.EventID
	second.Harness = first.Harness
	second.SessionID = first.SessionID
	second.Kind = record.KindSubagent
	second.Name = "other-name"
	second.Repo = "fedcba9876543210fedcba9876543210"
	second.Invoker = record.InvokerUser
	second.Timestamp = first.Timestamp.Add(72 * time.Hour)

	firstSpan, secondSpan := encodeOne(t, first), encodeOne(t, second)
	if firstSpan["spanId"] != secondSpan["spanId"] {
		t.Fatalf("spanId = %v and %v for one event id", firstSpan["spanId"], secondSpan["spanId"])
	}
	if want := string(first.EventID)[:16]; firstSpan["spanId"] != want {
		t.Fatalf("spanId = %v, want %q", firstSpan["spanId"], want)
	}
}

func TestIDsAreCorrectWidth(t *testing.T) {
	span := encodeOne(t, fullRecord())
	for key, width := range map[string]int{"traceId": 32, "spanId": 16} {
		id, ok := span[key].(string)
		if !ok {
			t.Fatalf("%s is %T, want a string", key, span[key])
		}
		if len(id) != width {
			t.Fatalf("%s = %q has %d chars, want %d", key, id, len(id), width)
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("%s = %q is not lowercase hex", key, id)
			}
		}
	}
}

func TestUnknownDurationRendersZeroLengthSpan(t *testing.T) {
	cases := map[string]struct {
		duration *int64
		wantNano int64
	}{
		"unknown":  {duration: nil, wantNano: 0},
		"explicit": {duration: ptr(int64(1500)), wantNano: 1_500_000_000},
		"zero":     {duration: ptr(int64(0)), wantNano: 0},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			r := fullRecord()
			r.DurationMS = testCase.duration
			span := encodeOne(t, r)

			start := parseNano(t, span, "startTimeUnixNano")
			end := parseNano(t, span, "endTimeUnixNano")
			if end-start != testCase.wantNano {
				t.Fatalf("end - start = %d ns, want %d", end-start, testCase.wantNano)
			}
			if want := r.Timestamp.UTC().UnixNano(); start != want {
				t.Fatalf("startTimeUnixNano = %d, want %d", start, want)
			}
		})
	}
}

func TestTimestampsAreJSONStrings(t *testing.T) {
	span := encodeOne(t, fullRecord())
	for _, key := range []string{"startTimeUnixNano", "endTimeUnixNano"} {
		if _, ok := span[key].(string); !ok {
			// A float64 here means the encoder emitted a JSON number, which
			// proto3 JSON forbids for 64-bit ints and which silently loses
			// precision past 2^53.
			t.Fatalf("%s decoded as %T, want string (proto3 JSON)", key, span[key])
		}
	}
}

func TestEncodeDropsUnrepresentableTimestamps(t *testing.T) {
	preEpoch := fullRecord()
	preEpoch.Timestamp = time.Unix(-1, 0).UTC()

	overflow := fullRecord()
	overflow.DurationMS = ptr(int64(math.MaxInt64))

	for name, r := range map[string]record.Record{"pre-epoch": preEpoch, "duration overflow": overflow} {
		t.Run(name, func(t *testing.T) {
			payload, dropped, err := Encode([]record.Record{r})
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			if dropped != 1 {
				t.Fatalf("Encode() dropped = %d, want 1", dropped)
			}
			if spans := spansOf(t, payload); len(spans) != 0 {
				t.Fatalf("Encode() emitted %d spans, want 0", len(spans))
			}
		})
	}
}

func parseNano(t *testing.T, span map[string]any, key string) int64 {
	t.Helper()
	raw, ok := span[key].(string)
	if !ok {
		t.Fatalf("%s is %T, want a string", key, span[key])
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("%s = %q is not a decimal int64: %v", key, raw, err)
	}
	return parsed
}

func TestSpanNameIsKindColonName(t *testing.T) {
	span := encodeOne(t, validRecord())
	if span["name"] != "skill:commit-message" {
		t.Fatalf("name = %v, want %q", span["name"], "skill:commit-message")
	}
	if span["kind"] != float64(kindInternal) {
		t.Fatalf("kind = %v, want %d", span["kind"], kindInternal)
	}
}

// TestStatusCodeNeverGuessesSuccess covers all nine outcome states, asserting
// through the encoded payload rather than through statusCode alone: the value
// that matters is the one that leaves the machine.
//
// The nil row and both denied_* rows carry an explicit "not OK" assertion on top
// of the exact-value one. That is the invariant ADR-0005 actually protects — a
// refactor that started reporting an unreported outcome as success would be the
// bug that quietly inflates every health number.
func TestStatusCodeNeverGuessesSuccess(t *testing.T) {
	cases := []struct {
		name      string
		outcome   *record.Outcome
		want      int
		mustNotOK bool
	}{
		{name: "unreported", outcome: nil, want: codeUnset, mustNotOK: true},
		{name: "ok", outcome: ptr(record.OutcomeOK), want: codeOK},
		{name: "error", outcome: ptr(record.OutcomeError), want: codeError},
		{name: "timeout", outcome: ptr(record.OutcomeTimeout), want: codeError},
		{name: "interrupted", outcome: ptr(record.OutcomeInterrupted), want: codeError},
		{name: "not_found", outcome: ptr(record.OutcomeNotFound), want: codeError},
		{name: "bad_args", outcome: ptr(record.OutcomeBadArgs), want: codeError},
		{name: "denied_policy", outcome: ptr(record.OutcomeDeniedPolicy), want: codeUnset, mustNotOK: true},
		{name: "denied_user", outcome: ptr(record.OutcomeDeniedUser), want: codeUnset, mustNotOK: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			r := fullRecord()
			r.Outcome = testCase.outcome
			got := child(t, encodeOne(t, r), "status")["code"]
			if got != float64(testCase.want) {
				t.Fatalf("status.code = %v, want %d", got, testCase.want)
			}
			if testCase.mustNotOK && got == float64(codeOK) {
				t.Fatalf("status.code = OK for outcome %q, which is not success", testCase.name)
			}
			if code := statusCode(testCase.outcome); code != testCase.want {
				t.Fatalf("statusCode() = %d, want %d", code, testCase.want)
			}
		})
	}
}

// frozenSpanAttributeKeys is the complete set of attribute keys a span may
// carry. Like frozenEncoderImports it is declared here and imported from
// nowhere: a test that compared the encoder's output against a list the encoder
// exports would prove only that the encoder agrees with itself.
//
// The assertion against it is equality, not containment. Containment would pass
// for a payload that had grown a new key, and a new key is exactly the way a
// field carrying prompt text, tool arguments, or tool output would arrive
// (ADR-0007, ADR-0012).
var frozenSpanAttributeKeys = []string{
	"gen_ai.request.model",
	"langfuse.observation.type",
	"langfuse.session.id",
	"wake.duration_ms",
	"wake.effort",
	"wake.harness",
	"wake.harness_version",
	"wake.invoker",
	"wake.kind",
	"wake.model",
	"wake.name",
	"wake.outcome",
	"wake.package",
	"wake.package_version",
	"wake.repo",
	"wake.schema_version",
	"wake.session_id",
	"wake.source",
	"wake.via_agent",
	"wake.via_skill",
}

// frozenAlwaysPresentKeys is the subset every span carries regardless of which
// optional fields the record populated.
var frozenAlwaysPresentKeys = []string{
	"langfuse.observation.type",
	"langfuse.session.id",
	"wake.harness",
	"wake.invoker",
	"wake.kind",
	"wake.name",
	"wake.repo",
	"wake.schema_version",
	"wake.session_id",
}

// frozenResourceAttributeKeys is the resource-level equivalent.
var frozenResourceAttributeKeys = []string{"service.name", "service.version"}

func TestFullRecordEmitsFrozenKeySet(t *testing.T) {
	got := attributeKeys(attributesOf(t, encodeOne(t, fullRecord()), "attributes"))
	if !slices.Equal(got, frozenSpanAttributeKeys) {
		t.Fatalf("span attribute keys = %v, frozen set = %v", got, frozenSpanAttributeKeys)
	}
}

func TestMinimalRecordEmitsOnlyAlwaysPresentKeys(t *testing.T) {
	got := attributeKeys(attributesOf(t, encodeOne(t, validRecord()), "attributes"))
	if !slices.Equal(got, frozenAlwaysPresentKeys) {
		t.Fatalf("span attribute keys = %v, always-present set = %v", got, frozenAlwaysPresentKeys)
	}
}

func TestObservationTypeMapping(t *testing.T) {
	cases := map[record.Kind]string{
		record.KindMCPTool:      "tool",
		record.KindBuiltinTool:  "tool",
		record.KindSubagent:     "agent",
		record.KindSkill:        "span",
		record.KindMCPServer:    "span",
		record.KindCommand:      "span",
		record.KindPlugin:       "span",
		record.KindHook:         "span",
		record.KindSessionStart: "span",
		record.KindSessionEnd:   "span",
	}
	for kind, want := range cases {
		t.Run(string(kind), func(t *testing.T) {
			r := fullRecord()
			r.Kind = kind
			attributes := attributesOf(t, encodeOne(t, r), "attributes")
			if got := attributes["langfuse.observation.type"]["stringValue"]; got != want {
				t.Fatalf("langfuse.observation.type = %v, want %q", got, want)
			}
			if got := attributes["wake.kind"]["stringValue"]; got != string(kind) {
				t.Fatalf("wake.kind = %v, want %q", got, kind)
			}
		})
	}
}

// TestOptionalAttributesAreOmittedNotBlank pins the difference between "the
// harness did not report this" and "the harness reported an empty value". An
// empty stringValue is indistinguishable from a real blank at the receiver, so
// an unreported optional is omitted entirely.
func TestOptionalAttributesAreOmittedNotBlank(t *testing.T) {
	cases := []struct {
		field string
		clear func(*record.Record)
		keys  []string
	}{
		{"HarnessVersion", func(r *record.Record) { r.HarnessVersion = "" }, []string{"wake.harness_version"}},
		{"Package", func(r *record.Record) { r.Package = "" }, []string{"wake.package"}},
		{"PackageVersion", func(r *record.Record) { r.PackageVersion = "" }, []string{"wake.package_version"}},
		{"Source", func(r *record.Record) { r.Source = nil }, []string{"wake.source"}},
		{"ViaSkill", func(r *record.Record) { r.ViaSkill = "" }, []string{"wake.via_skill"}},
		{"ViaAgent", func(r *record.Record) { r.ViaAgent = "" }, []string{"wake.via_agent"}},
		{"Model", func(r *record.Record) { r.Model = "" }, []string{"wake.model", "gen_ai.request.model"}},
		{"Effort", func(r *record.Record) { r.Effort = "" }, []string{"wake.effort"}},
		{"Outcome", func(r *record.Record) { r.Outcome = nil }, []string{"wake.outcome"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.field, func(t *testing.T) {
			r := fullRecord()
			testCase.clear(&r)
			attributes := attributesOf(t, encodeOne(t, r), "attributes")
			for _, key := range testCase.keys {
				if _, present := attributes[key]; present {
					t.Fatalf("%q emitted for an unset %s", key, testCase.field)
				}
			}
			for key, value := range attributes {
				if len(value) == 0 {
					t.Fatalf("%q carries an empty value object", key)
				}
				if text, ok := value["stringValue"]; ok && text == "" {
					t.Fatalf("%q carries an empty stringValue", key)
				}
			}
		})
	}
}

// TestUnknownDurationOmitsItsAttribute is the discriminating pair: a zero-length
// span alone cannot tell "unknown duration" from "measured at zero". The
// attribute is what separates them, so it is present for an explicit zero and
// absent when the harness reported nothing.
func TestUnknownDurationOmitsItsAttribute(t *testing.T) {
	unknown := fullRecord()
	unknown.DurationMS = nil
	unknownSpan := encodeOne(t, unknown)
	if unknownSpan["endTimeUnixNano"] != unknownSpan["startTimeUnixNano"] {
		t.Fatalf("unknown duration rendered a non-zero-length span")
	}
	if _, present := attributesOf(t, unknownSpan, "attributes")["wake.duration_ms"]; present {
		t.Fatal("wake.duration_ms emitted for an unreported duration")
	}

	measured := fullRecord()
	measured.DurationMS = ptr(int64(0))
	measuredSpan := encodeOne(t, measured)
	if measuredSpan["endTimeUnixNano"] != measuredSpan["startTimeUnixNano"] {
		t.Fatalf("a measured zero duration rendered a non-zero-length span")
	}
	if got := attributesOf(t, measuredSpan, "attributes")["wake.duration_ms"]["intValue"]; got != "0" {
		t.Fatalf("wake.duration_ms = %v, want %q", got, "0")
	}
}

func TestIntegerAttributesAreJSONStrings(t *testing.T) {
	attributes := attributesOf(t, encodeOne(t, fullRecord()), "attributes")
	for key, want := range map[string]string{"wake.schema_version": "1", "wake.duration_ms": "1500"} {
		got, ok := attributes[key]["intValue"].(string)
		if !ok {
			// A float64 here means a JSON number reached the wire, which
			// proto3 JSON forbids for 64-bit integers.
			t.Fatalf("%s intValue decoded as %T, want string (proto3 JSON)", key, attributes[key]["intValue"])
		}
		if got != want {
			t.Fatalf("%s intValue = %q, want %q", key, got, want)
		}
	}
}

func TestSchemaVersionOnEverySpan(t *testing.T) {
	denied := fullRecord()
	denied.EventID = record.DeriveEventID("claude-code", "source-event-3")
	denied.Outcome = ptr(record.OutcomeDeniedUser)
	denied.DurationMS = nil

	payload, dropped, err := Encode([]record.Record{validRecord(), fullRecord(), denied})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if dropped != 0 {
		t.Fatalf("Encode() dropped = %d, want 0", dropped)
	}
	spans := spansOf(t, payload)
	if len(spans) != 3 {
		t.Fatalf("Encode() emitted %d spans, want 3", len(spans))
	}
	for i, span := range spans {
		if got := attributesOf(t, span, "attributes")["wake.schema_version"]["intValue"]; got != "1" {
			t.Fatalf("span %d wake.schema_version = %v, want %q", i, got, "1")
		}
	}
}

// TestOutcomeStringSurvivesDenial keeps denial distinguishable from silence. Both
// render status UNSET, so the attribute is the only thing a receiver can use to
// tell "the user said no" from "the harness said nothing".
func TestOutcomeStringSurvivesDenial(t *testing.T) {
	r := fullRecord()
	r.Outcome = ptr(record.OutcomeDeniedUser)
	span := encodeOne(t, r)

	if got := child(t, span, "status")["code"]; got != float64(codeUnset) {
		t.Fatalf("status.code = %v, want %d", got, codeUnset)
	}
	if got := attributesOf(t, span, "attributes")["wake.outcome"]["stringValue"]; got != "denied_user" {
		t.Fatalf("wake.outcome = %v, want %q", got, "denied_user")
	}
}

func TestResourceAttributesAreFrozen(t *testing.T) {
	payload, _, err := Encode([]record.Record{fullRecord()})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	resourceSpans := objects(t, decodePayload(t, payload), "resourceSpans")
	attributes := attributesOf(t, child(t, resourceSpans[0], "resource"), "attributes")

	if got := attributeKeys(attributes); !slices.Equal(got, frozenResourceAttributeKeys) {
		t.Fatalf("resource attribute keys = %v, frozen set = %v", got, frozenResourceAttributeKeys)
	}
	if got := attributes["service.name"]["stringValue"]; got != serviceName {
		t.Fatalf("service.name = %v, want %q", got, serviceName)
	}
	if got := attributes["service.version"]["stringValue"]; got != version.Version {
		t.Fatalf("service.version = %v, want %q", got, version.Version)
	}
}

func TestEnvelopeShape(t *testing.T) {
	invalid := validRecord()
	invalid.SchemaVersion = 99
	records := []record.Record{validRecord(), invalid, fullRecord()}

	payload, dropped, err := Encode(records)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if dropped != 1 {
		t.Fatalf("Encode() dropped = %d, want 1", dropped)
	}

	// scopeSpansOf already asserts exactly one resourceSpans and one scopeSpans:
	// trace grouping is expressed by traceId per span, never by nesting, so a
	// batch never fans out into more than one envelope entry.
	scopeSpans := scopeSpansOf(t, payload)
	if got := child(t, scopeSpans, "scope")["name"]; got != scopeName {
		t.Fatalf("scope name = %v, want %q", got, scopeName)
	}
	if got := child(t, scopeSpans, "scope")["version"]; got != version.Version {
		t.Fatalf("scope version = %v, want %q", got, version.Version)
	}
	if got := objects(t, scopeSpans, "spans"); len(got) != len(records)-dropped {
		t.Fatalf("Encode() emitted %d spans, want %d", len(got), len(records)-dropped)
	}
}

// TestAttributeCapacityMatchesFrozenSet keeps the slice capacity from drifting
// away from the key set it is sized for.
func TestAttributeCapacityMatchesFrozenSet(t *testing.T) {
	if len(frozenSpanAttributeKeys) != maxSpanAttributes {
		t.Fatalf("maxSpanAttributes = %d, frozen key set has %d entries", maxSpanAttributes, len(frozenSpanAttributeKeys))
	}
}

func TestEncodeDropsInvalidRecord(t *testing.T) {
	invalid := validRecord()
	invalid.SchemaVersion = 99

	payload, dropped, err := Encode([]record.Record{invalid})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if dropped != 1 {
		t.Fatalf("Encode() dropped = %d, want 1", dropped)
	}
	if got := spansOf(t, payload); len(got) != 0 {
		t.Fatalf("Encode() emitted %d spans, want 0", len(got))
	}
}
