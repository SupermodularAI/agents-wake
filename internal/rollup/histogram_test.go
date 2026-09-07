package rollup

import (
	"testing"
)

// TestBucketCountMatchesBounds pins the relationship the format depends on. A
// block stores bucket counts without the boundaries they were computed against,
// so a build whose slice length disagrees with what it wrote would reinterpret
// every block on disk rather than fail to read it.
func TestBucketCountMatchesBounds(t *testing.T) {
	if got, want := bucketCount(), len(LatencyBounds)+1; got != want {
		t.Errorf("bucketCount() = %d, want len(LatencyBounds)+1 = %d", got, want)
	}
	var h Histogram
	if err := h.Observe(1); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got := len(h.Buckets); got != bucketCount() {
		t.Errorf("a histogram allocated %d buckets, want %d", got, bucketCount())
	}
}

// TestLatencyBoundsAreSortedAndPositive guards the binary search's precondition.
// An unsorted slice would place observations in arbitrary buckets with no error
// anywhere, and the blocks would be sealed that way permanently.
func TestLatencyBoundsAreSortedAndPositive(t *testing.T) {
	for index, bound := range LatencyBounds {
		if bound <= 0 {
			t.Errorf("bound %d is %d, want positive", index, bound)
		}
		if index > 0 && bound <= LatencyBounds[index-1] {
			t.Errorf("bound %d (%d) does not exceed bound %d (%d): bounds must be strictly ascending",
				index, bound, index-1, LatencyBounds[index-1])
		}
	}
}

// TestBucketsAreUpperInclusive asserts the boundary rule the doc states, at the
// boundary itself — the one value where an off-by-one is invisible in aggregate
// but shifts a reported percentile by a whole bucket.
func TestBucketsAreUpperInclusive(t *testing.T) {
	for index, bound := range LatencyBounds {
		var h Histogram
		if err := h.Observe(bound); err != nil {
			t.Fatalf("Observe(%d): %v", bound, err)
		}
		if h.Buckets[index] != 1 {
			t.Errorf("observation of %d ms did not land in bucket %d (its own bound's bucket)", bound, index)
		}
	}
}

// TestObserveRejectsNegative asserts a negative duration is refused rather than
// absorbed by bucket 0. record.Validate already refuses one, so this is about
// not silently laundering a caller's bug into a plausible number.
func TestObserveRejectsNegative(t *testing.T) {
	var h Histogram
	if err := h.Observe(-1); err == nil {
		t.Error("Observe(-1) returned no error, want a refusal")
	}
	if h.Count != 0 {
		t.Errorf("a refused observation still incremented Count to %d", h.Count)
	}
}

// TestOverflowBucketCountsBeyondLastBound asserts the final bucket catches
// everything above the last bound, so a very slow invocation is counted rather
// than dropped.
func TestOverflowBucketCountsBeyondLastBound(t *testing.T) {
	var h Histogram
	huge := LatencyBounds[len(LatencyBounds)-1] * 10
	if err := h.Observe(huge); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got := h.Buckets[len(LatencyBounds)]; got != 1 {
		t.Errorf("overflow bucket = %d, want 1 for an observation of %d ms", got, huge)
	}
}

// TestQuantileReportsBucketEdge asserts a quantile is a bucket bound and not an
// interpolation. The exact durations are gone once bucketed, so a bound is the
// strongest true statement available and interpolating would invent precision.
func TestQuantileReportsBucketEdge(t *testing.T) {
	var h Histogram
	// Ninety-nine observations at 1 ms and one at 3000 ms: p50 must sit in the
	// fast bucket and p100 must reach the slow one.
	for range 99 {
		if err := h.Observe(1); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	if err := h.Observe(3000); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	median, ok := h.Quantile(0.5)
	if !ok || median != 1 {
		t.Errorf("p50 = %d (ok=%v), want the 1 ms bound", median, ok)
	}
	top, ok := h.Quantile(1)
	if !ok || top != 5000 {
		t.Errorf("p100 = %d (ok=%v), want the 5000 ms bound containing a 3000 ms observation", top, ok)
	}
}

// TestQuantileOnEmptyHistogram asserts an unanswerable quantile says so instead
// of reporting zero. Zero is a latency, so returning it for "nothing measured"
// would be indistinguishable from a very fast call.
func TestQuantileOnEmptyHistogram(t *testing.T) {
	var h Histogram
	if _, ok := h.Quantile(0.95); ok {
		t.Error("Quantile on an empty histogram reported a value, want ok=false")
	}
	if _, ok := h.Mean(); ok {
		t.Error("Mean on an empty histogram reported a value, want ok=false")
	}
}

// TestMeanIsExact asserts the mean comes from the stored sum and not from bucket
// midpoints. Deriving it from buckets would make it an estimate presented with
// the authority of a measurement.
func TestMeanIsExact(t *testing.T) {
	var h Histogram
	for _, ms := range []int64{10, 20, 30} {
		if err := h.Observe(ms); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	mean, ok := h.Mean()
	if !ok || mean != 20 {
		t.Errorf("Mean = %v (ok=%v), want exactly 20", mean, ok)
	}
}

// TestMergeIsExactOnBuckets asserts the operation the tiering depends on. It is
// the unit-level statement of what TestMergeEqualsDirectReduce proves end to
// end.
func TestMergeIsExactOnBuckets(t *testing.T) {
	var left, right, both Histogram
	for _, ms := range []int64{1, 5, 100} {
		if err := left.Observe(ms); err != nil {
			t.Fatal(err)
		}
		if err := both.Observe(ms); err != nil {
			t.Fatal(err)
		}
	}
	for _, ms := range []int64{2, 5, 900} {
		if err := right.Observe(ms); err != nil {
			t.Fatal(err)
		}
		if err := both.Observe(ms); err != nil {
			t.Fatal(err)
		}
	}
	left.Merge(right)
	if left.Count != both.Count || left.Sum != both.Sum {
		t.Errorf("merged count/sum = %d/%d, want %d/%d", left.Count, left.Sum, both.Count, both.Sum)
	}
	for index := range both.Buckets {
		if left.Buckets[index] != both.Buckets[index] {
			t.Errorf("bucket %d = %d, want %d", index, left.Buckets[index], both.Buckets[index])
		}
	}
}

// TestMergeEmptyIsIdentity asserts merging nothing changes nothing, which is
// what lets Seal merge a set of children without special-casing an empty one.
func TestMergeEmptyIsIdentity(t *testing.T) {
	var h Histogram
	if err := h.Observe(42); err != nil {
		t.Fatal(err)
	}
	before := h.Count
	h.Merge(Histogram{})
	if h.Count != before {
		t.Errorf("merging an empty histogram changed Count from %d to %d", before, h.Count)
	}
}
