package rollup

import (
	"errors"
	"fmt"
	"slices"
)

// LatencyBounds are the upper edges, in milliseconds, of the latency buckets a
// block stores. Bucket i counts durations d where bounds[i-1] < d <= bounds[i],
// with bucket 0 counting d <= bounds[0] and the final overflow bucket counting
// everything above the last bound.
//
// These are frozen. A sealed block stores bucket counts and not the boundaries
// they were computed against, so changing this slice would reinterpret every
// block already on disk rather than migrate it — a p95 computed from buckets
// that mean something else is wrong with no symptom. A build that needs
// different boundaries bumps BlockVersion, which refuses the old blocks and
// seals them again.
//
// The scale is roughly 1-2-5 per decade from a millisecond to ten minutes,
// which is the range a primitive invocation actually occupies: sub-millisecond
// hook dispatches at one end and a long agent turn at the other. Nineteen
// bounds means twenty uint64 counters per key, so a block's cost is bounded by
// its key cardinality and is the same whether the key saw ten invocations or
// ten thousand.
var LatencyBounds = []int64{
	1, 2, 5,
	10, 20, 50,
	100, 200, 500,
	1_000, 2_000, 5_000,
	10_000, 20_000, 50_000,
	100_000, 200_000, 500_000,
	600_000,
}

// Histogram is a fixed-boundary bucket count.
//
// This type is the reason the tiered rollup works at all, so the reasoning is
// worth stating where the type is defined. The upstream design says to roll up
// "counts and percentiles". Counts roll up; percentiles do not. The p95 of two
// ranges combined is not derivable from each range's p95 — it needs the
// distribution, which a stored percentile has already thrown away. A block
// holding percentile values could therefore not seal a higher tier from the
// blocks below it without going back to the records, and "sealed once, never
// recomputed" is the property the whole design rests on.
//
// Bucket counts have none of that problem. They are integers, so Merge is
// elementwise addition: associative, commutative, exact, and independent of the
// order blocks are merged in. Percentiles are derived from a merged histogram at
// view-assembly time by Quantile, and never stored.
type Histogram struct {
	// Buckets has len(LatencyBounds)+1 entries: one per bound plus the
	// overflow. A zero-length Histogram is valid and means no observation was
	// recorded, which is not the same as an observation of zero.
	Buckets []uint64 `json:"buckets"`

	// Count is the number of observations, and Sum their total. Both are
	// redundant with Buckets for counting but not for reporting: a bucketed
	// value has lost its exact magnitude, so a mean derived from bucket
	// midpoints would be wrong in a way nobody could see. These two make the
	// mean exact while the percentiles stay bucketed.
	Count uint64 `json:"count"`
	Sum   int64  `json:"sum"`
}

// errNegativeObservation rejects a negative duration. record.Validate already
// refuses one, so reaching this is a bug in a caller rather than bad data, and
// it is reported instead of bucketed because bucket 0 would silently absorb it.
var errNegativeObservation = errors.New("a latency observation must not be negative")

// bucketCount is the number of counters a Histogram carries.
func bucketCount() int { return len(LatencyBounds) + 1 }

// Observe records one duration in milliseconds.
func (h *Histogram) Observe(ms int64) error {
	if ms < 0 {
		return fmt.Errorf("%d ms: %w", ms, errNegativeObservation)
	}
	if len(h.Buckets) == 0 {
		h.Buckets = make([]uint64, bucketCount())
	}
	h.Buckets[bucketFor(ms)]++
	h.Count++
	h.Sum += ms
	return nil
}

// bucketFor returns the index of the bucket a duration falls in. The bounds are
// sorted and short, so a binary search is used rather than a scan — not for
// speed at this length, but because BinarySearch states the invariant the scan
// would only imply.
func bucketFor(ms int64) int {
	index, exact := slices.BinarySearch(LatencyBounds, ms)
	if exact {
		// Buckets are upper-inclusive: a duration equal to a bound belongs to
		// that bound's bucket, not the next one.
		return index
	}
	return index
}

// Merge adds other into h. It is the whole of the tiered reduce: a tier-k+1
// block's histogram is the elementwise sum of the tier-k histograms below it,
// and because addition is associative and commutative the result does not
// depend on the order blocks were merged or the order records arrived.
func (h *Histogram) Merge(other Histogram) {
	if other.Count == 0 && len(other.Buckets) == 0 {
		return
	}
	if len(h.Buckets) == 0 {
		h.Buckets = make([]uint64, bucketCount())
	}
	for index, count := range other.Buckets {
		if index >= len(h.Buckets) {
			// A histogram from a block with more buckets than this build has
			// cannot be merged into one that would drop its tail. The version
			// stamp is what prevents this from being reachable; the guard is
			// here because silently discarding the slowest observations would
			// present as latency quietly improving.
			break
		}
		h.Buckets[index] += count
	}
	h.Count += other.Count
	h.Sum += other.Sum
}

// Quantile reports the upper bound of the bucket containing the q'th quantile,
// with q in [0,1], and whether there was any observation to answer from.
//
// The value is a bucket edge and not an interpolation, and the distinction is
// the honest one: the exact durations are gone, so the only truthful statement
// available is "at least q of the observations were at or below this bound".
// Interpolating within a bucket would manufacture a precision the stored data
// does not carry. The final overflow bucket has no upper bound, so a quantile
// landing there reports the last real bound and false — there is no number that
// would be true.
func (h Histogram) Quantile(q float64) (int64, bool) {
	if h.Count == 0 || q < 0 || q > 1 {
		return 0, false
	}
	// Ceiling of q*Count, so q=0.95 over 100 observations is the 95th and not
	// the 94th. Computed in integer arithmetic to keep the answer identical
	// across platforms: a float multiplication landing a hair below an integer
	// would pick the bucket below.
	target := (uint64(q*1000)*h.Count + 999) / 1000
	if target == 0 {
		target = 1
	}
	var seen uint64
	for index, count := range h.Buckets {
		seen += count
		if seen >= target {
			if index >= len(LatencyBounds) {
				return LatencyBounds[len(LatencyBounds)-1], false
			}
			return LatencyBounds[index], true
		}
	}
	return 0, false
}

// Mean reports the exact arithmetic mean, and whether there was anything to
// average. It is exact because Sum is stored: deriving it from bucket midpoints
// would be an estimate presented as a measurement.
func (h Histogram) Mean() (float64, bool) {
	if h.Count == 0 {
		return 0, false
	}
	return float64(h.Sum) / float64(h.Count), true
}
