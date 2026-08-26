// Package metrics is Wake's single aggregation layer. Renderers receive counts
// and Ratio values from here rather than computing percentages themselves.
package metrics

import (
	"cmp"
	"slices"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// Ratio carries the measured population for any rate. Its fields are private so
// callers cannot construct a percentage without a denominator and excluded
// count; NewRatio is the only constructor.
type Ratio struct {
	numerator   uint64
	denominator uint64
	excluded    uint64
	total       uint64
}

// NewRatio creates an auditable rate. It panics for impossible counts because
// such a result would be a bug in the metrics layer, not recoverable data.
func NewRatio(numerator, denominator, excluded, total uint64) Ratio {
	if numerator > denominator || denominator+excluded > total {
		panic("invalid ratio counts")
	}
	return Ratio{numerator: numerator, denominator: denominator, excluded: excluded, total: total}
}

// Numerator is the number matching the condition.
func (r Ratio) Numerator() uint64 { return r.numerator }

// Denominator is the population actually measured.
func (r Ratio) Denominator() uint64 { return r.denominator }

// Excluded is the population omitted because the source did not report it.
func (r Ratio) Excluded() uint64 { return r.excluded }

// Total is the full population before exclusions.
func (r Ratio) Total() uint64 { return r.total }

// Percent reports the ratio for display only. A zero denominator has no rate.
func (r Ratio) Percent() (float64, bool) {
	if r.denominator == 0 {
		return 0, false
	}
	return float64(r.numerator) / float64(r.denominator) * 100, true
}

// PrimitiveUsage is one primitive's observed activity.
type PrimitiveUsage struct {
	Name        record.Identifier
	Kind        record.Kind
	Harness     record.Identifier
	Invoker     record.Invoker
	ViaAgent    record.Identifier
	Invocations uint64
	Sessions    uint64
	LastUsed    time.Time
	ErrorRate   Ratio
}

// Summary is the MVP dashboard's real-data contract.
type Summary struct {
	Invocations  uint64
	Sessions     uint64
	LastObserved time.Time
	Outcomes     map[record.Outcome]uint64
	ErrorRate    Ratio
	Primitives   []PrimitiveUsage
}

// Aggregate derives the MVP's metrics from terminal records. Unknown outcomes
// remain usage evidence but are excluded from health-rate denominators.
//
// Built-in tool calls (Bash, Read, Edit, ...) are excluded before anything is
// counted. Every renderer built on Summary — OVERVIEW, OUTCOMES, the dashboard's
// stat tiles — is meant to describe the primitives Wake tracks, the same
// population USED PRIMITIVES lists below them; a session's ordinary tool calls
// outnumber its primitive calls and would otherwise dominate every rate with
// activity the primitive table never shows.
//
// Session-grain records (ADR-0002) are excluded from the same counts for a
// different reason: they are not invocations of anything. One counts toward the
// session population and the last-observed instant — which is what makes a session
// with no primitive use visible at all, the plan §2.7 baseline — and toward nothing
// else. Its Sessions figure therefore includes sessions no primitive row accounts
// for, which is the point.
func Aggregate(records []record.Record) Summary {
	summary := Summary{Outcomes: make(map[record.Outcome]uint64)}
	allSessions := make(map[record.Identifier]struct{})
	primitives := make(map[primitiveKey]*primitiveAccumulator)
	var known, unknown, failures uint64

	for _, event := range records {
		if !record.IsTerminal(event) {
			continue
		}
		// A session-grain record is evidence that a session existed and nothing else.
		// It is the plan §2.7 baseline — a session with zero primitive use is exactly
		// the row that makes every rate above it meaningful — so it counts toward the
		// session population and the last-observed instant, and toward nothing else:
		// not Invocations, not an outcome tally, and never a primitive of its own
		// (ADR-0002, ADR-0006). A primitive named "session" would read as something the
		// user invoked, and its absent outcome would be excluded from a rate it was
		// never in the denominator of.
		if record.IsSessionGrain(event.Kind) {
			allSessions[event.SessionID] = struct{}{}
			if event.Timestamp.After(summary.LastObserved) {
				summary.LastObserved = event.Timestamp
			}
			continue
		}
		if event.Kind == record.KindBuiltinTool {
			continue
		}
		summary.Invocations++
		allSessions[event.SessionID] = struct{}{}
		if event.Timestamp.After(summary.LastObserved) {
			summary.LastObserved = event.Timestamp
		}
		if event.Outcome == nil {
			unknown++
		} else {
			known++
			summary.Outcomes[*event.Outcome]++
			if record.IsFailure(*event.Outcome) {
				failures++
			}
		}

		key := primitiveKey{name: event.Name, kind: event.Kind, harness: event.Harness, invoker: event.Invoker, viaAgent: event.ViaAgent}
		accumulator := primitives[key]
		if accumulator == nil {
			accumulator = &primitiveAccumulator{PrimitiveUsage: PrimitiveUsage{Name: event.Name, Kind: event.Kind, Harness: event.Harness, Invoker: event.Invoker, ViaAgent: event.ViaAgent}, sessions: map[record.Identifier]struct{}{}}
			primitives[key] = accumulator
		}
		accumulator.Invocations++
		accumulator.sessions[event.SessionID] = struct{}{}
		if event.Timestamp.After(accumulator.LastUsed) {
			accumulator.LastUsed = event.Timestamp
		}
		if event.Outcome == nil {
			accumulator.unknown++
		} else {
			accumulator.known++
			if record.IsFailure(*event.Outcome) {
				accumulator.failures++
			}
		}
	}

	summary.Sessions = uint64(len(allSessions))
	summary.ErrorRate = NewRatio(failures, known, unknown, summary.Invocations)
	summary.Primitives = make([]PrimitiveUsage, 0, len(primitives))
	for _, accumulator := range primitives {
		accumulator.Sessions = uint64(len(accumulator.sessions))
		accumulator.ErrorRate = NewRatio(accumulator.failures, accumulator.known, accumulator.unknown, accumulator.Invocations)
		summary.Primitives = append(summary.Primitives, accumulator.PrimitiveUsage)
	}
	slices.SortFunc(summary.Primitives, func(left, right PrimitiveUsage) int {
		return cmp.Or(cmp.Compare(right.Invocations, left.Invocations), cmp.Compare(string(left.Harness), string(right.Harness)), cmp.Compare(string(left.Name), string(right.Name)), cmp.Compare(string(left.Invoker), string(right.Invoker)), cmp.Compare(string(left.ViaAgent), string(right.ViaAgent)))
	})
	return summary
}

type primitiveKey struct {
	name     record.Identifier
	kind     record.Kind
	harness  record.Identifier
	invoker  record.Invoker
	viaAgent record.Identifier
}

type primitiveAccumulator struct {
	PrimitiveUsage
	sessions map[record.Identifier]struct{}
	known    uint64
	unknown  uint64
	failures uint64
}
