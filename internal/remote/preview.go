package remote

import (
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// Payloads is what the next flush would put on the wire: one entry per batch, in
// the order a flush would post them.
//
// The bytes are Encode's output and nothing else — the same projection a real
// flush sends, so the attribute allowlist that governs the wire governs this too
// (ADR-0007, ADR-0027). Gzip is transport framing applied after encoding, so it
// is deliberately absent here: what ADR-0027 asks `--dry-run` to show is the
// payload, not the compression of it.
type Payloads struct {
	// Batches is one encoded body per batch a flush would post.
	Batches [][]byte
	// Dropped is how many records the encoder omitted across all batches.
	Dropped int
}

// PreviewFlush reports what the next flush would send, sends nothing, and writes
// nothing.
//
// It exists because ADR-0027 spent the byte-identity guarantee ADR-0017 gave —
// the wire payload is no longer the file on disk — so printing the payload
// through the real encoder is the only remaining way to inspect what would
// leave. It therefore goes through Encode and batches, the same two functions
// flushLocked uses, rather than rendering anything of its own.
//
// Three gates a real flush applies are deliberately absent, because each guards
// a request and no request is made here: the enabled/endpoint/credential check,
// the single-flight lock, and the minimum interval. Inspecting what would leave
// before turning delivery on is the whole reason this exists.
//
// It never writes. The watermark advances only on a 2xx for a complete batch
// (ADR-0018), and a preview has no 2xx to advance on.
func PreviewFlush(p config.Paths) (Payloads, error) {
	events := store.New(eventsPath(p))
	head, err := events.Head()
	if err != nil {
		return Payloads{}, err
	}

	// The same self-heal view flushLocked applies at the top of a run: a
	// watermark past head means the spool was rebuilt under it, so the preview
	// must show what the next flush would actually re-send rather than what a
	// stale position implies. Duplicated rather than shared, because folding it
	// into a helper would move flushLocked's store read to before its
	// minimum-interval gate; TestPreviewFlushMatchesWhatFlushPosts pins the two
	// together behaviourally instead.
	//
	// Flush's other view — no position is ever recorded over a spool this build
	// cannot read whole — needs nothing here, and that is the point of putting it
	// where the position is written rather than where it is read: a stale spool's
	// state file says "delivered through nothing", so this reader and every other one
	// already agrees with what the next flush will send.
	state := readDeliveryState(deliveryStatePath(p))
	if state.Position > head {
		state.Position = 0
	}

	entries, err := events.Entries(state.Position)
	if err != nil {
		return Payloads{}, err
	}

	// The same resolution the real flush performs, through the same exported
	// function, because "--dry-run prints the exact bytes a flush would send"
	// (ADR-0030) is only true if the labels are resolved identically. Deliberately
	// not duplicated logic: config.ProjectLabels is the single answer, and
	// TestPreviewAndFlushAgreeOnTheRepoLabel pins the two callers together
	// behaviourally, as TestPreviewFlushMatchesWhatFlushPosts already does for the
	// watermark view. It reads and writes nothing, which is what keeps this
	// function's "It never writes" true.
	labels := RepoLabels(config.ProjectLabels(p))

	var preview Payloads
	for _, batch := range batches(entries) {
		payload, dropped, encodeErr := Encode(recordsOf(batch), labels)
		if encodeErr != nil {
			return Payloads{}, encodeErr
		}
		preview.Dropped += dropped
		if len(batch) != dropped {
			preview.Batches = append(preview.Batches, payload)
		}
	}
	return preview, nil
}
