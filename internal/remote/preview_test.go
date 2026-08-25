package remote

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"testing"
)

// TestPreviewFlushMatchesWhatFlushPosts is the acceptance criterion
// "--dry-run output is byte-identical to the body a real flush would send",
// asserted at the source rather than at the rendering: the preview is compared
// to the bytes a receiver actually got, gunzipped, for the same records.
//
// It is also what makes the duplicated watermark selection in PreviewFlush safe:
// the two are pinned behaviourally rather than by sharing a function that could
// still be called differently.
func TestPreviewFlushMatchesWhatFlushPosts(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)

	payloads, err := PreviewFlush(paths)
	if err != nil {
		t.Fatalf("PreviewFlush() error = %v", err)
	}
	if len(payloads.Batches) != 1 {
		t.Fatalf("Batches = %d, want 1", len(payloads.Batches))
	}

	if _, flushErr := FlushReport(paths); flushErr != nil {
		t.Fatalf("FlushReport() error = %v", flushErr)
	}
	if got := receiver.count(); got != len(payloads.Batches) {
		t.Fatalf("the flush posted %d requests, the preview showed %d", got, len(payloads.Batches))
	}
	for i, payload := range payloads.Batches {
		if body := gunzip(t, receiver.request(t, i).body); !bytes.Equal(body, payload) {
			t.Errorf("batch %d: posted body = %s\npreview = %s", i, body, payload)
		}
	}
}

// The multi-batch half of the same guarantee, at the size where the ceiling
// constant is visible. A preview that showed one merged payload would be a
// different document from any request a flush makes.
func TestPreviewFlushSplitsIntoTheSameBatchesAFlushWould(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, maxBatchRecords+1)

	payloads, err := PreviewFlush(paths)
	if err != nil {
		t.Fatalf("PreviewFlush() error = %v", err)
	}
	if len(payloads.Batches) != 2 {
		t.Fatalf("Batches = %d, want 2 — the record ceiling must split the preview too", len(payloads.Batches))
	}

	if _, flushErr := FlushReport(paths); flushErr != nil {
		t.Fatalf("FlushReport() error = %v", flushErr)
	}
	if got := receiver.count(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	for i, payload := range payloads.Batches {
		if body := gunzip(t, receiver.request(t, i).body); !bytes.Equal(body, payload) {
			t.Errorf("batch %d does not match the body a flush posted", i)
		}
	}
}

// The whole point of the flag: inspecting what would leave without anything
// leaving.
func TestPreviewFlushMakesNoRequest(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)

	if _, err := PreviewFlush(paths); err != nil {
		t.Fatalf("PreviewFlush() error = %v", err)
	}
	if got := receiver.count(); got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
}

// The watermark advances only on a 2xx for a complete batch (ADR-0018), and a
// preview has no 2xx to advance on. A preview that stamped LastFlush would also
// suppress the next real flush through the minimum interval, which is a silent
// outage caused by looking.
func TestPreviewFlushDoesNotTouchTheDeliveryState(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)

	if _, err := PreviewFlush(paths); err != nil {
		t.Fatalf("PreviewFlush() error = %v", err)
	}
	if _, err := os.Stat(deliveryStatePath(paths)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(delivery state) = %v, want ErrNotExist — a preview writes nothing", err)
	}

	if _, err := FlushReport(paths); err != nil {
		t.Fatalf("FlushReport() error = %v", err)
	}
	if got := receiver.count(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	before := readDeliveryState(deliveryStatePath(paths))

	seedFrom(t, paths, 3, 2)
	if _, err := PreviewFlush(paths); err != nil {
		t.Fatalf("second PreviewFlush() error = %v", err)
	}
	after := readDeliveryState(deliveryStatePath(paths))

	if after.Position != before.Position {
		t.Errorf("position = %d, want %d unchanged", after.Position, before.Position)
	}
	if !after.LastFlush.Equal(before.LastFlush) {
		t.Errorf("LastFlush = %v, want %v unchanged", after.LastFlush, before.LastFlush)
	}
}

// Inspecting what would leave before turning delivery on is the whole reason
// this exists, so the enabled/endpoint/credential gate is deliberately absent:
// it guards a request, and no request is made here.
func TestPreviewFlushWorksWhileDeliveryIsOff(t *testing.T) {
	paths := testPaths(t)
	seed(t, paths, 2)

	payloads, err := PreviewFlush(paths)
	if err != nil {
		t.Fatalf("PreviewFlush() error = %v", err)
	}
	if len(payloads.Batches) != 1 {
		t.Fatalf("Batches = %d, want 1", len(payloads.Batches))
	}
	if got := spansOf(t, payloads.Batches[0]); len(got) != 2 {
		t.Errorf("payload carried %d spans, want 2", len(got))
	}
}

// The minimum interval throttles requests, and a preview makes none. A `flush
// --dry-run` that printed nothing because a flush happened four minutes ago
// would look like an empty spool.
func TestPreviewFlushIsNotGatedByTheMinimumInterval(t *testing.T) {
	paths := testPaths(t)
	_, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	setMinInterval(t, paths, "1h")
	seed(t, paths, 3)

	if _, err := FlushReport(paths); err != nil {
		t.Fatalf("FlushReport() error = %v", err)
	}
	seedFrom(t, paths, 3, 2)

	payloads, err := PreviewFlush(paths)
	if err != nil {
		t.Fatalf("PreviewFlush() error = %v", err)
	}
	if len(payloads.Batches) != 1 {
		t.Fatalf("Batches = %d, want 1 — the minimum interval must not gate a preview", len(payloads.Batches))
	}
	if got := spansOf(t, payloads.Batches[0]); len(got) != 2 {
		t.Errorf("payload carried %d spans, want the 2 records past the watermark", len(got))
	}
}

// Nothing pending is no batches, not one empty batch: an empty batch is
// something a flush never posts, so showing one would misdescribe the run.
func TestPreviewFlushIsEmptyWhenNothingIsPending(t *testing.T) {
	paths := testPaths(t)

	payloads, err := PreviewFlush(paths)
	if err != nil {
		t.Fatalf("PreviewFlush() error = %v", err)
	}
	if len(payloads.Batches) != 0 {
		t.Errorf("Batches = %d, want 0", len(payloads.Batches))
	}
}

// The same self-heal flushLocked applies: after `ingest --rebuild` a watermark
// past head means the spool shrank under it, so the preview must show what the
// next flush would actually re-send rather than what a stale position implies.
func TestPreviewFlushSelfHealsAWatermarkPastHead(t *testing.T) {
	paths := testPaths(t)
	if err := writeDeliveryState(deliveryStatePath(paths), deliveryState{Position: 99}); err != nil {
		t.Fatalf("writeDeliveryState() error = %v", err)
	}
	seed(t, paths, 2)

	payloads, err := PreviewFlush(paths)
	if err != nil {
		t.Fatalf("PreviewFlush() error = %v", err)
	}
	if len(payloads.Batches) != 1 {
		t.Fatalf("Batches = %d, want 1", len(payloads.Batches))
	}
	if got := spansOf(t, payloads.Batches[0]); len(got) != 2 {
		t.Errorf("payload carried %d spans, want both records re-sent", len(got))
	}
}
