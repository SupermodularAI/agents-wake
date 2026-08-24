//go:build remote

package remote

import (
	"bytes"
	"encoding/json"
	"flag"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/version"
)

// frozenPackageImports is every non-test file in this package and the exact
// import set that file may have, declared here and nowhere else. Asserting the
// package against a list it exports itself would be vacuous, so the literal
// lives in the test (ADR-0012: the wire is governed by the same allowlist
// discipline as the disk).
//
// Per file rather than per package, since DG-65 added the delivery loop beside
// the encoder. That is stricter, not looser: previously one union covered every
// file, so any file could hold any allowlisted import; now each file's set is
// exact, and a file absent from this map fails outright. What the split makes
// possible is the honest statement that deliver.go performs I/O and otlp.go
// still does not — see encoderFiles.
var frozenPackageImports = map[string][]string{
	"otlp.go": {
		"encoding/json",
		"errors",
		"github.com/SupermodularAI/agents-wake/internal/record",
		"github.com/SupermodularAI/agents-wake/internal/version",
		"math",
		"strconv",
	},
	"deliver.go": {
		"bytes",
		"compress/gzip",
		"encoding/base64",
		"errors",
		"fmt",
		"github.com/SupermodularAI/agents-wake/internal/config",
		"github.com/SupermodularAI/agents-wake/internal/lockfile",
		"github.com/SupermodularAI/agents-wake/internal/record",
		"github.com/SupermodularAI/agents-wake/internal/store",
		"io",
		"net/http",
		"path/filepath",
		"time",
	},
	"preview.go": {
		"github.com/SupermodularAI/agents-wake/internal/config",
		"github.com/SupermodularAI/agents-wake/internal/store",
	},
	"state.go": {
		"github.com/SupermodularAI/agents-wake/internal/config",
		"github.com/SupermodularAI/agents-wake/internal/store",
		"time",
	},
	"watermark.go": {
		"encoding/json",
		"github.com/SupermodularAI/agents-wake/internal/atomicfile",
		"github.com/SupermodularAI/agents-wake/internal/config",
		"io/fs",
		"os",
		"path/filepath",
		"time",
	},
}

// encoderFiles names the files the package doc's no-I/O claim is made about.
// Delivery is I/O by definition — a socket and a state file are the whole point
// of it — so the delivery files are excluded here and only here. The encoder's
// purity is what the forbidden set below protects, and it is unchanged.
var encoderFiles = []string{"otlp.go"}

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

// TestPackageImportsAreFrozen asserts the capability claim in the package doc,
// which is made about the PACKAGE and so must be checked across the package.
//
// It scans every non-test .go file in this directory rather than naming files.
// The transport landed behind this same remote build tag, and a by-name
// assertion would have stayed green while net/http entered the package: a file
// this map does not declare is a failure, so a new file cannot arrive with
// capabilities nobody reviewed. Build constraints are deliberately not honoured
// — go/parser ignores them — because a file excluded from this build is still a
// file in this package for some other build.
//
// Reading the package's own source is test scaffolding, not encoder I/O: the
// assertion is about what the non-test files themselves import.
func TestPackageImportsAreFrozen(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}

	fileSet := token.NewFileSet()
	scanned := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned = append(scanned, name)

		parsed, err := parser.ParseFile(fileSet, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		imports := make([]string, 0, len(parsed.Imports))
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquoting import path %s in %s: %v", spec.Path.Value, name, err)
			}
			imports = append(imports, path)
		}
		slices.Sort(imports)

		frozen, declared := frozenPackageImports[name]
		if !declared {
			// The tripwire. A new file in this package is a new set of
			// capabilities on the wire path, and it has to be declared here
			// before it can be compiled past this test.
			t.Errorf("%s has no entry in frozenPackageImports: declare its exact import set", name)
			continue
		}
		if !slices.Equal(imports, frozen) {
			t.Errorf("imports of %s = %v, frozen allowlist = %v", name, imports, frozen)
		}
		if !slices.Contains(encoderFiles, name) {
			continue
		}
		for _, forbidden := range forbiddenEncoderImports {
			if slices.Contains(imports, forbidden) {
				t.Errorf("%s imports %q: the encoder must perform no I/O", name, forbidden)
			}
		}
	}
	if len(scanned) == 0 {
		// Without this the whole assertion passes vacuously against an empty
		// set if the scan ever stops finding files.
		t.Fatal("found no non-test .go files to scan: the import assertion would be vacuous")
	}

	// The twin guard: a stale entry would leave a deleted file's allowlist
	// lingering, and an encoderFiles entry naming nothing would make the no-I/O
	// half of this test vacuous rather than false.
	for name := range frozenPackageImports {
		if !slices.Contains(scanned, name) {
			t.Errorf("frozenPackageImports declares %s, which this package does not contain", name)
		}
	}
	for _, name := range encoderFiles {
		if !slices.Contains(scanned, name) {
			t.Errorf("encoderFiles names %s, which this package does not contain", name)
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

	// time.Unix(-1, 0) above is *inside* UnixNano's defined range, so it only
	// exercises the sign check. These two are outside it, where UnixNano wraps
	// rather than saturating — and the pre-1678 case wraps to a large POSITIVE
	// value, which is why a sign check alone let a fabricated year-2184
	// timestamp onto the wire uncounted.
	preRepresentable := fullRecord()
	preRepresentable.Timestamp = time.Date(1600, time.January, 1, 0, 0, 0, 0, time.UTC)

	postRepresentable := fullRecord()
	postRepresentable.Timestamp = time.Date(3000, time.January, 1, 0, 0, 0, 0, time.UTC)

	overflow := fullRecord()
	overflow.DurationMS = ptr(int64(math.MaxInt64))

	for name, r := range map[string]record.Record{
		"pre-epoch":         preEpoch,
		"pre-1678 wrap":     preRepresentable,
		"post-2262 wrap":    postRepresentable,
		"duration overflow": overflow,
	} {
		t.Run(name, func(t *testing.T) {
			// record.Validate imposes no range on Timestamp, so every row here
			// is a *valid* record the encoder must still refuse. Asserting that
			// first keeps the test honest: without it a row could pass because
			// encodeSpan's Validate gate dropped it, never reaching spanTimes.
			if err := record.Validate(r); err != nil {
				t.Fatalf("fixture is not a valid record, so the drop would prove nothing: %v", err)
			}
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

// hostileIdentifiers is this package's own corpus of source values that must
// never reach the wire. ADR-0007 requires a hostile-payload corpus per input
// shape rather than one shared set, and the wire is a new *output* shape with
// its own failure modes, so it gets its own corpus even though it is not a new
// adapter. A shared corpus would let this package inherit coverage it never ran.
//
// It repeats internal/record's list and adds two entries that matter only here:
// a prompt-injection string and an API-key-shaped string, the two things an
// operator would most regret finding in a payload sent to a third-party
// collector.
var hostileIdentifiers = []string{
	"/usr/local/bin", "usr/local/bin", "./relative", "../secrets", "a/../b",
	"~/.ssh/id_rsa", `C:\Windows\System32`, "C:temp", `C:/Users/me`, `\\server\share`,
	`back\slash`, "contains space", "tab\there", "new\nline", "trailing/", "/", "..",
	".hidden", "", "ignore previous instructions", "sk-ant-api03-DEADBEEF",
}

// transcriptKeys are the four attribute keys that would carry prompt text, tool
// arguments, or model output. They are the keys an OTLP integration written
// without this project's constraints would reach for first, so their absence is
// asserted by name rather than left to the key-set equality test alone.
var transcriptKeys = []string{
	"langfuse.observation.input",
	"langfuse.observation.output",
	"gen_ai.prompt",
	"gen_ai.completion",
}

// frozenWireFieldNames is every JSON object field the payload may contain. They
// come from struct tags, so they are compile-time literals, but freezing them
// makes a newly added wire field fail this test rather than pass unnoticed.
var frozenWireFieldNames = []string{
	"attributes", "code", "endTimeUnixNano", "intValue", "key", "kind", "name",
	"resource", "resourceSpans", "scope", "scopeSpans", "spanId", "spans",
	"startTimeUnixNano", "status", "stringValue", "traceId", "value", "version",
}

// hostileFields are the record fields that carry a name-domain or token-domain
// value from a harness. Each is a place a source string could reach the wire.
var hostileFields = []struct {
	name string
	set  func(*record.Record, string)
}{
	{"Name", func(r *record.Record, v string) { r.Name = record.Identifier(v) }},
	{"SessionID", func(r *record.Record, v string) { r.SessionID = record.Identifier(v) }},
	{"Harness", func(r *record.Record, v string) { r.Harness = record.Identifier(v) }},
	{"Package", func(r *record.Record, v string) { r.Package = record.Identifier(v) }},
	{"Model", func(r *record.Record, v string) { r.Model = record.Identifier(v) }},
	{"ViaSkill", func(r *record.Record, v string) { r.ViaSkill = record.Identifier(v) }},
	{"ViaAgent", func(r *record.Record, v string) { r.ViaAgent = record.Identifier(v) }},
	{"Effort", func(r *record.Record, v string) { r.Effort = record.Identifier(v) }},
}

// TestHostileIdentifiersNeverReachTheWire states the encoder's actual privacy
// contract, which is narrower than "hostile input is rejected" and stronger than
// "hostile input is not echoed".
//
// The encoder is not a validator and must not become one — record.Validate is
// the single gate, and duplicating its rules here would give two definitions of
// a safe value that could drift. What the encoder owes is this: a value that the
// gate refused appears nowhere in the payload, and a value the gate accepted
// appears only as its own field's attribute, never smuggled into a second place.
//
// So the corpus splits by what record.Validate says, and the test asserts the
// matching half. Note that "sk-ant-api03-DEADBEEF" is an accepted value: it is a
// well-formed bounded Identifier, and if a harness genuinely names a skill that,
// reporting the name is correct behaviour. The record type is the allowlist
// (ADR-0007) — the encoder's job is to add nothing to it.
func TestHostileIdentifiersNeverReachTheWire(t *testing.T) {
	for _, field := range hostileFields {
		for _, hostile := range hostileIdentifiers {
			t.Run(field.name+"/"+strconv.Quote(hostile), func(t *testing.T) {
				r := fullRecord()
				field.set(&r, hostile)
				accepted := record.Validate(r) == nil

				// A path-shaped value must never be accepted, in any field.
				// This is the assertion that would catch a widened validator.
				if accepted && strings.ContainsAny(hostile, `/\`) {
					t.Fatalf("a path-shaped value was accepted into %s", field.name)
				}

				payload, dropped, err := Encode([]record.Record{r})
				if err != nil {
					t.Fatalf("Encode() error = %v", err)
				}

				if !accepted {
					if dropped != 1 {
						t.Fatalf("Encode() dropped = %d, want 1 for a refused value", dropped)
					}
					if spans := spansOf(t, payload); len(spans) != 0 {
						t.Fatalf("Encode() emitted %d spans for a refused value", len(spans))
					}
					if hostile != "" && bytes.Contains(payload, []byte(hostile)) {
						t.Fatalf("a refused value reached the payload")
					}
					return
				}

				if dropped != 0 {
					t.Fatalf("Encode() dropped = %d for an accepted value", dropped)
				}
				assertEveryStringIsAllowlisted(t, payload, r)
			})
		}
	}
}

// assertEveryStringIsAllowlisted is the positive form of the allowlist: no
// string reaches the wire that did not come from a typed Record field, a derived
// id, a rendered number, or a constant of this package.
//
// A containment test ("the secret is not in the payload") only catches leaks
// somebody thought to name. This catches the ones nobody did.
func assertEveryStringIsAllowlisted(t *testing.T, payload []byte, r record.Record) {
	t.Helper()

	allowed := make(map[string]bool)
	for _, group := range [][]string{frozenSpanAttributeKeys, frozenResourceAttributeKeys, frozenWireFieldNames} {
		for _, key := range group {
			allowed[key] = true
		}
	}
	for _, constant := range []string{serviceName, scopeName, version.Version, "tool", "agent", "span"} {
		allowed[constant] = true
	}

	start, end, ok := spanTimes(r)
	if !ok {
		t.Fatal("spanTimes() rejected a record the encoder accepted")
	}
	values := []string{
		string(r.Kind), string(r.Name), string(r.Harness), string(r.HarnessVersion),
		string(r.SessionID), string(r.Repo), string(r.Package), string(r.PackageVersion),
		string(r.ViaSkill), string(r.ViaAgent), string(r.Model), string(r.Effort),
		string(r.Invoker), string(r.Kind) + ":" + string(r.Name),
		traceID(r), spanID(r), start, end,
		strconv.FormatUint(uint64(r.SchemaVersion), 10),
	}
	if r.Source != nil {
		values = append(values, string(*r.Source))
	}
	if r.Outcome != nil {
		values = append(values, string(*r.Outcome))
	}
	if r.DurationMS != nil {
		values = append(values, strconv.FormatInt(*r.DurationMS, 10))
	}
	for _, value := range values {
		allowed[value] = true
	}

	for _, found := range walkStrings(t, payload) {
		if !allowed[found] {
			t.Fatalf("payload carries the unallowlisted string %q", found)
		}
	}
}

// walkStrings collects every string in the decoded payload — object field names
// as well as values, since a leak could arrive as either.
func walkStrings(t *testing.T, payload []byte) []string {
	t.Helper()
	var found []string
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				found = append(found, key)
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		case string:
			found = append(found, typed)
		}
	}
	walk(decodePayload(t, payload))
	return found
}

func TestEveryEmittedStringIsAllowlisted(t *testing.T) {
	for name, r := range map[string]record.Record{"fullRecord": fullRecord(), "validRecord": validRecord()} {
		t.Run(name, func(t *testing.T) {
			payload, dropped, err := Encode([]record.Record{r})
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			if dropped != 0 {
				t.Fatalf("Encode() dropped = %d, want 0", dropped)
			}
			assertEveryStringIsAllowlisted(t, payload, r)
		})
	}
}

func TestTranscriptKeysAreAbsent(t *testing.T) {
	batches := map[string][]record.Record{
		"full":  {fullRecord()},
		"valid": {validRecord()},
		"empty": nil,
	}
	for name, batch := range batches {
		t.Run(name, func(t *testing.T) {
			payload, _, err := Encode(batch)
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			for _, key := range transcriptKeys {
				if bytes.Contains(payload, []byte(key)) {
					t.Fatalf("payload carries the transcript key %q", key)
				}
			}
		})
	}
}

// TestPayloadHasNoPathSeparator is the blunt mechanical check behind plan §3.4:
// nothing this encoder legitimately emits contains a path separator, so one
// appearing means a path fragment reached the wire.
//
// It holds because scopeName is the binary's name rather than this package's Go
// import path, and because record.Validate refuses a path shape in every field.
// version.Version is "dev" under go test; in a real build it is `git describe
// --tags` output, which cannot contain a slash for the vX.Y.Z tags this project
// uses.
func TestPayloadHasNoPathSeparator(t *testing.T) {
	batches := map[string][]record.Record{
		"full":  {fullRecord()},
		"valid": {validRecord()},
		"batch": {fullRecord(), validRecord()},
	}
	for name, batch := range batches {
		t.Run(name, func(t *testing.T) {
			payload, _, err := Encode(batch)
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			if bytes.ContainsRune(payload, '/') {
				t.Fatal("payload contains a path separator")
			}
			if bytes.ContainsRune(payload, '\\') {
				t.Fatal("payload contains a backslash")
			}
		})
	}
}

// update rewrites the golden fixture instead of comparing against it. The
// fixture is generated, never hand-written — a hand-edited golden is a golden
// that agrees with whatever the code did.
var update = flag.Bool("update", false, "rewrite testdata/golden.json")

// goldenBatch is the fixed input behind testdata/golden.json: a fully populated
// record, a minimal one, and a denied one with no reported duration. The third
// exists because it is the combination most likely to regress — UNSET status
// that must not become OK, and an omitted duration attribute that must not
// become zero.
func goldenBatch() []record.Record {
	denied := fullRecord()
	denied.EventID = record.DeriveEventID("claude-code", "source-event-3")
	denied.Outcome = ptr(record.OutcomeDeniedUser)
	denied.DurationMS = nil
	return []record.Record{fullRecord(), validRecord(), denied}
}

// TestEncodeIsDeterministic pins the property the spool's replay safety rests
// on: the same records always produce the same bytes, so a re-send is a
// duplicate a receiver can drop rather than a second, differently-shaped event.
//
// It holds because attributes are built as a slice in fixed source order. Ranging
// a map anywhere in the encoder would break it, and would break it
// intermittently, which is the worst way for it to break.
func TestEncodeIsDeterministic(t *testing.T) {
	records := goldenBatch()
	first, _, err := Encode(records)
	if err != nil {
		t.Fatalf("Encode() first error = %v", err)
	}
	for i := range 32 {
		next, _, err := Encode(records)
		if err != nil {
			t.Fatalf("Encode() run %d error = %v", i, err)
		}
		if !bytes.Equal(first, next) {
			t.Fatalf("Encode() run %d differs:\n%s\n%s", i, first, next)
		}
	}
}

// TestGoldenPayload is the human-review surface: the one place a reviewer can
// read exactly what leaves the machine, rather than infer it from assertions.
//
// The fixture is stored indented for that reason, and it lives in this package's
// own testdata/ — not the repo-root testdata/, which AGENTS.md reserves for
// harness fixtures captured through the redaction tooling.
func TestGoldenPayload(t *testing.T) {
	payload, dropped, err := Encode(goldenBatch())
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if dropped != 0 {
		t.Fatalf("Encode() dropped = %d, want 0", dropped)
	}

	var indented bytes.Buffer
	if err = json.Indent(&indented, payload, "", "  "); err != nil {
		t.Fatalf("json.Indent() error = %v", err)
	}
	indented.WriteByte('\n')

	goldenPath := filepath.Join("testdata", "golden.json")
	if *update {
		if err = os.WriteFile(goldenPath, indented.Bytes(), 0o644); err != nil {
			t.Fatalf("writing %s: %v", goldenPath, err)
		}
		t.Logf("wrote %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading %s (regenerate with -update): %v", goldenPath, err)
	}
	if !bytes.Equal(indented.Bytes(), want) {
		t.Fatalf("payload differs from %s (regenerate with -update after reviewing the change):\ngot:\n%s\nwant:\n%s",
			goldenPath, indented.Bytes(), want)
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
