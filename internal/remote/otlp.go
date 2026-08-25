// Package remote projects stored records onto the OTLP/HTTP JSON wire format
// and delivers them.
//
// The payload is a projection computed at flush time, never the spool's bytes.
// ADR-0017 once required the wire to be byte-identical to the spool; ADR-0027
// deliberately spends that property, because OTLP is a span shape and the spool
// is a record shape, and no single serialisation is both. What replaces it is
// stricter, not looser: the emitted attribute key set is a frozen literal
// asserted by test, so the privacy allowlist governs the wire exactly as it
// governs the disk (ADR-0007, ADR-0030, plan §9). That holds in every build:
// there is no configuration and no build in which a record reaches the wire
// through any other encoder.
//
// The encoder is pure. This file and its helpers perform no I/O — no network,
// no filesystem, no clock — and make no model call (ADR-0008). Encode takes
// records and returns bytes.
//
// Sending them is deliver.go's business, and watermark.go and state.go are the
// state it keeps: those three are this package's I/O surface, and deliver.go's
// POST is the only outbound connection the binary makes in its own process
// (ADR-0026, ADR-0030). The import assertion in the test is therefore per file
// rather than per package — each file declares its exact import set, and the
// no-I/O forbidden set is asserted against the encoder's files, which is what
// the purity claim above is actually about.
package remote

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/version"
)

// Wire types for the OTLP/HTTP JSON (proto3 JSON) span export request. They are
// unexported: the payload's shape is this package's private business, and the
// only supported way to observe it is the bytes Encode returns.
type (
	payload struct {
		ResourceSpans []resourceSpans `json:"resourceSpans"`
	}
	resourceSpans struct {
		Resource   resource     `json:"resource"`
		ScopeSpans []scopeSpans `json:"scopeSpans"`
	}
	resource struct {
		Attrs []keyValue `json:"attributes"`
	}
	scopeSpans struct {
		Scope scope  `json:"scope"`
		Spans []span `json:"spans"`
	}
	scope struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	keyValue struct {
		Key   string `json:"key"`
		Value value  `json:"value"`
	}
	// value carries IntValue as a string because proto3 JSON encodes 64-bit
	// integers as strings. A lenient receiver would accept a number; a strict
	// one rejects the whole batch.
	value struct {
		StringValue string `json:"stringValue,omitempty"`
		IntValue    string `json:"intValue,omitempty"`
	}
	// status has no omitempty on Code: UNSET is written explicitly rather than
	// left to be inferred from an absent field (ADR-0005 — unknown is never
	// success, and it must say so on the wire).
	status struct {
		Code int `json:"code"`
	}
	span struct {
		TraceID   string     `json:"traceId"`
		SpanID    string     `json:"spanId"`
		Name      string     `json:"name"`
		Kind      int        `json:"kind"`
		StartTime string     `json:"startTimeUnixNano"`
		EndTime   string     `json:"endTimeUnixNano"`
		Attrs     []keyValue `json:"attributes"`
		Status    status     `json:"status"`
	}
)

const (
	serviceName = "wake"
	// scopeName is the binary's name, deliberately not this package's Go import
	// path. The conventional OTLP scope name is the instrumenting library's
	// import path, but that string contains slashes, and a path separator on the
	// wire is precisely what the privacy assertions look for (plan §3.4).
	scopeName    = "wake"
	kindInternal = 1

	codeUnset = 0
	codeOK    = 1
	codeError = 2

	nanosPerMilli  = int64(1_000_000)
	nanosPerSecond = int64(1_000_000_000)

	// maxUnixSecond bounds spanTimes' input. time.Time.UnixNano() is documented
	// as undefined outside roughly 1678-2262, and it wraps there rather than
	// saturating, so it cannot be used to detect its own overflow. Unix() is
	// exact at every time a time.Time can hold, so the whole seconds are what
	// gets range-checked. The -1 gives the sub-second remainder room: at exactly
	// MaxInt64/nanosPerSecond the nanoseconds within that final second could
	// still carry the product past MaxInt64.
	maxUnixSecond = math.MaxInt64/nanosPerSecond - 1

	zeroTraceID = "00000000000000000000000000000000"
	zeroSpanID  = "0000000000000000"

	// maxSpanAttributes is the size of the frozen span attribute key set. It
	// only sizes an allocation, but keeping it beside the constants makes a
	// change to the key set visible in the same review.
	maxSpanAttributes = 20
)

// errEncode is deliberately valueless. A diagnostic that quoted the record it
// failed on would be the one place derived data could leak back into free text
// (plan §4.2), the same reason record.errUnsafeIdentifier names no value.
var errEncode = errors.New("encoding otlp payload")

// Encode returns the OTLP/HTTP JSON payload for records and the number of
// records omitted from it.
//
// It fails closed: every record is re-validated here, on the way out, and one
// that cannot be represented is dropped rather than emitted in a degraded form
// (ADR-0030). Built-in tools are also omitted: Wake retains them locally, but
// they are implementation noise rather than useful remote observations. Omission
// is never silent — the count is returned so a caller can report it rather than
// treating it as an empty delivery (plan §12).
func Encode(records []record.Record) ([]byte, int, error) {
	spans := make([]span, 0, len(records))
	dropped := 0
	for _, r := range records {
		encoded, ok := encodeSpan(r)
		if !ok {
			dropped++
			continue
		}
		spans = append(spans, encoded)
	}

	encoded, err := json.Marshal(payload{ResourceSpans: []resourceSpans{{
		Resource:   resource{Attrs: resourceAttributes()},
		ScopeSpans: []scopeSpans{{Scope: scope{Name: scopeName, Version: version.Version}, Spans: spans}},
	}}})
	if err != nil {
		return nil, dropped, errEncode
	}
	return encoded, dropped, nil
}

// encodeSpan projects one record onto a span, reporting false when the record
// must be omitted. Built-in tools are intentionally excluded from remote
// telemetry; every other omission is a representability failure, never a
// judgement about the record's content.
//
// parentSpanId is deliberately absent in v1. ViaSkill and ViaAgent name a parent
// primitive but not its event_id, and the encoder cannot invent the link without
// a lookup this pure package must not perform. Emitting a fabricated parent id
// would be worse than emitting none, so the span tree stays flat until a record
// carries a real parent id — an ADR conversation, not an implementation detail.
func encodeSpan(r record.Record) (span, bool) {
	// Validate on the way out as well as on the way in: the wire is subject to
	// the same contract as the disk (ADR-0030), and this is the last point at
	// which a malformed record can still be stopped.
	if err := record.Validate(r); err != nil {
		return span{}, false
	}
	// Built-in tools are retained for local reporting, but exporting every Bash,
	// Read, and Write call obscures the user-configured primitives this signal is
	// intended to show in Langfuse.
	if r.Kind == record.KindBuiltinTool {
		return span{}, false
	}
	start, end, ok := spanTimes(r)
	if !ok {
		return span{}, false
	}
	trace, id := traceID(r), spanID(r)
	// OTLP rejects an all-zero trace or span id. Reaching one means a SHA-256
	// prefix came out all zeroes — a ~2^-64 event — and the record is dropped
	// and counted rather than emitted under a fabricated id.
	if trace == zeroTraceID || id == zeroSpanID {
		return span{}, false
	}
	return span{
		TraceID:   trace,
		SpanID:    id,
		Name:      string(r.Kind) + ":" + string(r.Name),
		Kind:      kindInternal,
		StartTime: start,
		EndTime:   end,
		Attrs:     spanAttributes(r),
		Status:    status{Code: statusCode(r.Outcome)},
	}, true
}

// spanAttributes projects the record's remaining fields onto the frozen
// attribute key set, in a fixed source order. The order is part of the contract:
// it is what makes Encode byte-deterministic, and it is why attributes are built
// as a slice and never as a map.
//
// This function is the wire's allowlist. Every key it can emit is a literal
// here, every value comes from a typed Record field or a constant, and there is
// no branch that copies free text — because Record has no free text to copy
// (ADR-0007). A test asserts this key set by equality against an independent
// literal, so adding a key here without adding it there fails the build.
func spanAttributes(r record.Record) []keyValue {
	attrs := make([]keyValue, 0, maxSpanAttributes)

	// wake.schema_version is on every span, not only on the resource, and it is
	// not dead weight. The receiver's store is append-only from our side and can
	// never be rebuilt; delivery is at-least-once and the record schema will
	// evolve, so the store accumulates a mix of record shapes. This attribute is
	// the only thing that lets a later query tell those shapes apart. Deleting
	// it as "unused" silently corrupts every receiver's history (ADR-0027).
	attrs = appendInt(attrs, "wake.schema_version", int64(r.SchemaVersion))

	attrs = appendString(attrs, "wake.kind", string(r.Kind))
	// wake.name passes the already-derived Identifier through verbatim. Where
	// that identifier carries a scope digest, the digest was computed upstream
	// under a key this package neither holds nor wants: nothing here derives,
	// re-derives, reverses, or strips one (ADR-0020, ADR-0022).
	attrs = appendString(attrs, "wake.name", string(r.Name))
	attrs = appendString(attrs, "wake.harness", string(r.Harness))
	attrs = appendString(attrs, "wake.harness_version", string(r.HarnessVersion))
	attrs = appendString(attrs, "wake.session_id", string(r.SessionID))
	// langfuse.session.id duplicates wake.session_id under the key Langfuse
	// groups traces by. It is the same already-bounded token, not a second value.
	attrs = appendString(attrs, "langfuse.session.id", string(r.SessionID))
	// wake.repo is the hashed id and only the hashed id. The repository label
	// and path never leave the local store; this package could not read them if
	// it wanted to, because it has no filesystem access at all. The local
	// hash-to-label map is internal/config's business and is named nowhere else
	// in the module, by test (plan §3.4, §9).
	attrs = appendString(attrs, "wake.repo", string(r.Repo))
	attrs = appendString(attrs, "wake.package", string(r.Package))
	attrs = appendString(attrs, "wake.package_version", string(r.PackageVersion))
	if r.Source != nil {
		attrs = appendString(attrs, "wake.source", string(*r.Source))
	}
	attrs = appendString(attrs, "wake.via_skill", string(r.ViaSkill))
	attrs = appendString(attrs, "wake.via_agent", string(r.ViaAgent))
	attrs = appendString(attrs, "wake.model", string(r.Model))
	// gen_ai.request.model is the OpenTelemetry semantic-convention spelling of
	// the same bounded identifier, so a generic receiver can read it too.
	attrs = appendString(attrs, "gen_ai.request.model", string(r.Model))
	attrs = appendString(attrs, "wake.effort", string(r.Effort))
	attrs = appendString(attrs, "wake.invoker", string(r.Invoker))
	if r.Outcome != nil {
		// The exact enum string, not a re-derived label: a denial must stay
		// distinguishable from an unreported outcome, which status alone cannot
		// express since both render UNSET (ADR-0005).
		attrs = appendString(attrs, "wake.outcome", string(*r.Outcome))
	}
	if r.DurationMS != nil {
		// Gated on nil, never on the value. Emitting 0 for an unreported
		// duration would turn "not measured" into "measured as instant".
		attrs = appendInt(attrs, "wake.duration_ms", *r.DurationMS)
	}
	attrs = appendString(attrs, "langfuse.observation.type", observationType(r.Kind))

	return attrs
}

// observationType maps a wake Kind onto the Langfuse observation taxonomy.
//
// Every kind of the enum is listed explicitly rather than folded into the
// default, so a new Kind added to record shows up as a missing case in review
// and in the mapping test, instead of silently inheriting "span".
func observationType(kind record.Kind) string {
	switch kind {
	case record.KindMCPTool:
		return "tool"
	case record.KindSubagent:
		return "agent"
	case record.KindSkill, record.KindMCPServer, record.KindCommand,
		record.KindPlugin, record.KindHook, record.KindSessionStart, record.KindSessionEnd:
		return "span"
	default:
		// Unreachable for a validated record: record.Validate rejects any Kind
		// outside the enum above before encodeSpan gets here.
		return "span"
	}
}

// appendString is the single mechanical implementation of "omit, never blank".
// An unreported optional is skipped entirely, because an empty stringValue on
// the wire is indistinguishable at the receiver from a real empty value.
func appendString(attrs []keyValue, key, val string) []keyValue {
	if val == "" {
		return attrs
	}
	return append(attrs, keyValue{Key: key, Value: value{StringValue: val}})
}

// appendInt emits a 64-bit integer as the decimal string proto3 JSON requires.
// It does not gate on the value: the caller gates on nil, so a genuine zero is
// emitted and an unreported value never reaches here.
func appendInt(attrs []keyValue, key string, val int64) []keyValue {
	return append(attrs, keyValue{Key: key, Value: value{IntValue: strconv.FormatInt(val, 10)}})
}

// traceID groups every span of one session under one trace.
//
// record.DeriveEventID is SHA256(harness ‖ 0x00 ‖ sourceEvent) hex-encoded, so
// calling it with the session id computes exactly the SHA256(harness ‖ 0x00 ‖
// session_id) this trace id is specified as. Reusing the keystone derivation
// rather than re-implementing SHA-256 here keeps one definition of "derived id"
// in the codebase (ADR-0004) and keeps crypto/sha256 and encoding/hex out of
// this package's import set. A trace id is 16 bytes: the first 32 hex chars.
func traceID(r record.Record) string {
	return string(record.DeriveEventID(r.Harness, r.SessionID))[:32]
}

// spanID is where the record's EventID goes. EventID is mapped onto the wire by
// derivation into spanId, not copied into a wake.* attribute — the span id is
// the identity a receiver deduplicates on, which is precisely the job ADR-0004
// gives the event id, so a second copy would be redundant, not safer.
//
// Truncating is safe because record.Validate has already run and guarantees 64
// lowercase hex characters. A span id is 8 bytes: the first 16 hex chars.
func spanID(r record.Record) string { return string(r.EventID)[:16] }

// spanTimes renders the record's Timestamp and DurationMS as proto3 JSON
// decimal strings, reporting false when the pair is not representable.
//
// Timestamp is mapped onto the wire by derivation into startTimeUnixNano rather
// than as its own attribute. An unknown duration renders a zero-length span:
// end == start states "the harness did not report how long this took", where a
// guessed duration would state a number nobody measured (ADR-0005's rule applied
// to time — unknown is never filled in).
func spanTimes(r record.Record) (start, end string, ok bool) {
	// Range-check the seconds BEFORE converting to nanoseconds. Two distinct
	// classes are rejected here and both must be, because both are silent:
	//
	//   - Pre-epoch. OTLP's nano fields are unsigned, so a timestamp before
	//     1970 has no representation on the wire at all.
	//   - Outside UnixNano's defined range (~1678-2262). UnixNano wraps there
	//     instead of saturating, and a pre-1678 timestamp wraps to a large
	//     POSITIVE value — year 1600 comes out as year 2184. A sign check on
	//     the converted value cannot see that, which is why the check happens
	//     on Unix() seconds instead.
	//
	// Either way the record is dropped and counted, never wrapped: a fabricated
	// timestamp would land in a receiver store that can never be rebuilt from
	// our side (ADR-0027), so a definite-looking wrong number is worse than a
	// reported blind spot (plan §12).
	sec := r.Timestamp.UTC().Unix()
	if sec < 0 || sec > maxUnixSecond {
		return "", "", false
	}
	startNano := r.Timestamp.UTC().UnixNano()
	endNano := startNano
	if r.DurationMS != nil {
		// Checked before the multiply, not after: signed overflow would
		// silently produce a negative end time.
		if *r.DurationMS > (math.MaxInt64-startNano)/nanosPerMilli {
			return "", "", false
		}
		endNano = startNano + *r.DurationMS*nanosPerMilli
	}
	return strconv.FormatInt(startNano, 10), strconv.FormatInt(endNano, 10), true
}

// statusCode maps a nullable outcome onto an OTLP status code.
//
// A nil outcome is UNSET, never OK. The harness did not report one on roughly
// 61% of Claude Code tool results, and rendering that absence as success is the
// single mistake that would make every health number this tool prints a lie
// (ADR-0005).
//
// denied_policy and denied_user are also UNSET, and for a different reason: they
// are known outcomes that are neither success nor failure. A permission denial
// is the system working. record.IsFailure already draws that line for the local
// metrics, so it is reused here rather than reimplemented — one classification,
// one place to change it. It takes a non-pointer Outcome and is only reachable
// after the nil branch has returned.
func statusCode(outcome *record.Outcome) int {
	switch {
	case outcome == nil:
		return codeUnset
	case *outcome == record.OutcomeOK:
		return codeOK
	case record.IsFailure(*outcome):
		return codeError
	default:
		return codeUnset
	}
}

// resourceAttributes identifies the producer, and identifies nothing else. Two
// keys, both constants of this build — no hostname, no username, no working
// directory, none of the environment detail an OTLP SDK would attach by default.
// That absence is the point: the resource block is the usual place identifying
// information reaches a collector without anyone deciding it should.
func resourceAttributes() []keyValue {
	return []keyValue{
		{Key: "service.name", Value: value{StringValue: serviceName}},
		{Key: "service.version", Value: value{StringValue: version.Version}},
	}
}
