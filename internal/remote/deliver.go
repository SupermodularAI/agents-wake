//go:build remote

package remote

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/lockfile"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

const (
	// eventsFileName is the spool internal/activation writes. Spelled here as
	// well because this package must not import internal/activation — that
	// package owns consent and triggers and would drag both onto the delivery
	// path.
	eventsFileName = "events.ndjson"

	// deliveryLockName is the single-flight lock for a flush. It is a different
	// file from ingest.lock and from the spool's own append lock, because the
	// three answer different questions: this one keeps two flushes from both
	// posting, and a flush that finds it held has nothing to add by repeating
	// what the holder is already doing (ADR-0018).
	deliveryLockName = "remote-flush.lock"

	minIntervalKey = "remote.min_interval"

	// The batch ceilings, constants rather than configuration. ADR-0014 keeps
	// the config surface deliberately small and remote.min_interval is the one
	// sizing knob a decision asks for; a key with no decision behind it is scope
	// creep rather than a convenience.
	//
	// maxBatchBytes counts marshalled records before gzip, so it is a bound on
	// what is held in memory as much as on what is sent. No real record comes
	// close to it — every Record field is a bounded identifier — so in practice
	// the record ceiling is what splits a batch; the byte ceiling is the guard
	// against a future record shape for which that stops being true.
	maxBatchRecords = 500
	maxBatchBytes   = 4 << 20

	// requestTimeout is why a hung endpoint cannot hold a flush open. Go's
	// default client has no timeout at all, and ADR-0018 requires that a dead
	// endpoint be indistinguishable from an absent one from the user's point of
	// view — which a request that never returns is not.
	requestTimeout = 10 * time.Second

	contentTypeHeader      = "Content-Type"
	contentEncodingHeader  = "Content-Encoding"
	authorizationHeader    = "Authorization"
	ingestionVersionHeader = "x-langfuse-ingestion-version"

	contentTypeJSON = "application/json"
	encodingGzip    = "gzip"
	// ingestionVersion is Langfuse's ingestion-API version header. It is
	// additive request metadata, not a second wire format: ADR-0027 fixes the
	// format as backend-agnostic OTLP/HTTP JSON, and any other OTLP receiver
	// ignores this header. It is implemented as specified rather than
	// generalised on this package's own authority.
	ingestionVersion = "4"
)

// The two ways delivery fails, both valueless.
//
// ADR-0028's rule is "never echo what was read", and http.Client.Do returns a
// *url.Error that embeds the URL it failed on — so the transport's error is
// replaced here and never wrapped. That is the same reason config's
// isHTTPEndpoint discards url.Parse's error, and it is the single easiest
// privacy regression on this path: wrapping reads as diligence and leaks the
// endpoint into every log line a caller writes.
var (
	errDeliveryFailed   = errors.New("the remote endpoint could not be reached")
	errDeliveryRejected = errors.New("the remote endpoint rejected a batch")
)

// deliveryClient is the only outbound HTTP client in the binary, and it is a
// variable so a test can give it a sub-second timeout — the idiom
// internal/cli's hookChild uses for the same reason.
//
// A test that swaps it is why no test in this package may call t.Parallel.
var deliveryClient = &http.Client{Timeout: requestTimeout}

// Flush delivers everything the spool holds past the watermark and advances the
// watermark over what the receiver accepted.
//
// It is the only place in this codebase that opens an outbound connection, and
// it is compiled only under the remote build tag: the default binary links no
// network client at all (ADR-0012, ADR-0026).
//
// Nothing here prints. A caller on the hook-invoked path is forbidden to report
// a failure (ADR-0016) and hands the returned error to internal/cli's discard;
// the error exists for `remote flush`, which a user ran deliberately.
//
// The order of the first two steps is the acceptance criterion "assert no
// request is ever constructed when the endpoint is unset or state is off": the
// credential store is consulted before anything else happens, and a build that
// is off returns before a lock file, a state file, or a request exists.
func Flush(p config.Paths) error {
	auth, err := config.LoadRemoteAuth(p)
	if err != nil {
		// Returned as-is. ADR-0028 already guarantees this package's errors name
		// neither the endpoint nor the credential, so wrapping would only add a
		// sentence this package cannot check.
		return err
	}
	if !auth.Enabled || auth.Endpoint == "" || auth.Credential == "" {
		return nil
	}

	cfg, err := config.Load(p)
	if err != nil {
		return err
	}
	// The sentinel bool is discarded because remote.min_interval registers no
	// sentinel words: there is no "never" for it, so every value is a duration.
	minInterval, _, err := cfg.Duration(minIntervalKey)
	if err != nil {
		return err
	}

	// Single-flight, not a queue. TryWithLock reports a skipped run as
	// (false, nil), and a skipped run is a correct outcome rather than a
	// failure: whatever this flush would have sent, the holder is sending.
	_, err = lockfile.TryWithLock(filepath.Join(p.DataDir, deliveryLockName), func() error {
		return flushLocked(p, auth, minInterval)
	})
	return err
}

// flushLocked is the run itself, with the single-flight lock held.
func flushLocked(p config.Paths, auth config.RemoteAuth, minInterval time.Duration) error {
	statePath := deliveryStatePath(p)
	state := readDeliveryState(statePath)
	startedAt := time.Now().UTC()

	if suppressed(state.LastFlush, startedAt, minInterval) {
		return nil
	}

	events := store.New(eventsPath(p))
	head, err := events.Head()
	if err != nil {
		return err
	}
	// Self-heal after a rebuild. `ingest --rebuild` calls store.Discard and
	// re-derives the spool, so positions shift; a watermark past head means the
	// store shrank under it. Reset and re-send rather than clamp: at-least-once
	// is free because span_id is derived from the deterministic event_id, so the
	// receiver collapses what it already holds (ADR-0004, ADR-0018, ADR-0027),
	// whereas clamping to head would skip every record between the new head and
	// the stale position, permanently and silently.
	if state.Position > head {
		state.Position = 0
	}

	// The watermark is the only cursor. Entries(after uint64) already takes
	// exactly this parameter, so delivery needs no store change and introduces
	// no per-record delivered flag — the store is append-only and mutating it is
	// what ADR-0018 rejected.
	entries, err := events.Entries(state.Position)
	if err != nil {
		return err
	}

	var deliveryErr error
	for _, batch := range batches(entries) {
		// Encode's dropped count is deliberately discarded. A record the encoder
		// refuses is one it will refuse on every future run, so holding the
		// watermark behind it would stall delivery permanently — and this
		// package has nowhere to report it: the hook-invoked path may not print
		// (ADR-0016) and Status' field list is fixed by ADR-0018's visibility
		// model. Surfacing it belongs in a health.Report counter, which is a
		// schema bump on a shared untagged file and therefore its own ticket.
		payload, _, encodeErr := Encode(recordsOf(batch))
		if encodeErr != nil {
			deliveryErr = encodeErr
			break
		}
		body, gzipErr := gzipped(payload)
		if gzipErr != nil {
			deliveryErr = gzipErr
			break
		}
		if postErr := post(auth.Endpoint, auth.Credential, body); postErr != nil {
			// The first non-2xx stops the run. Continuing past a failed batch
			// and advancing over a later successful one would open a gap nothing
			// ever closes, because the watermark is a single position and cannot
			// describe a hole.
			deliveryErr = postErr
			break
		}
		state.Position = batch[len(batch)-1].Position
	}

	// Written on the failure path too, and deliberately. The partial position
	// has to survive or the next run re-sends batches the receiver already
	// accepted, and LastFlush has to advance or a dead endpoint is retried on
	// every single trigger — which is what the minimum interval exists to
	// prevent.
	state.LastFlush = startedAt
	return errors.Join(deliveryErr, writeDeliveryState(statePath, state))
}

// suppressed reports whether the minimum interval has not elapsed yet.
//
// A zero or negative interval turns the gate off entirely rather than meaning
// "no time may pass at all". A LastFlush in the future — a clock that moved
// backwards, or a state file somebody edited — counts as due rather than as
// suppressed: flushing early costs one extra POST the receiver deduplicates,
// while suppressing would stop delivery until real time caught up, which is a
// silent outage.
func suppressed(lastFlush, now time.Time, minInterval time.Duration) bool {
	if minInterval <= 0 || lastFlush.IsZero() {
		return false
	}
	elapsed := now.Sub(lastFlush)
	return elapsed >= 0 && elapsed < minInterval
}

// eventsPath is the spool this package reads and never writes.
func eventsPath(p config.Paths) string {
	return filepath.Join(p.DataDir, eventsFileName)
}

// recordsOf projects a batch of store entries onto the records the encoder
// takes. The positions stay behind: they are the watermark's business and
// nothing the receiver is told about.
func recordsOf(batch []store.Entry) []record.Record {
	records := make([]record.Record, 0, len(batch))
	for _, entry := range batch {
		records = append(records, entry.Record)
	}
	return records
}

// batches splits entries at the ceilings a delivery run uses.
func batches(entries []store.Entry) [][]store.Entry {
	return batchesWithin(entries, maxBatchRecords, maxBatchBytes)
}

// batchesWithin is batches with the ceilings supplied, so the byte ceiling can
// be exercised at a size a real record can reach. Both ceilings are checked
// before an entry is added and only against a non-empty batch, which is what
// guarantees that an entry larger than maxBytes forms a batch of one instead of
// being dropped or producing an empty batch nothing could post.
func batchesWithin(entries []store.Entry, maxRecords, maxBytes int) [][]store.Entry {
	var grouped [][]store.Entry
	batch := make([]store.Entry, 0, min(len(entries), maxRecords))
	size := 0
	for _, entry := range entries {
		entrySize := marshalledSize(entry)
		if len(batch) > 0 && (len(batch) >= maxRecords || size+entrySize > maxBytes) {
			grouped = append(grouped, batch)
			batch = make([]store.Entry, 0, min(len(entries), maxRecords))
			size = 0
		}
		batch = append(batch, entry)
		size += entrySize
	}
	if len(batch) > 0 {
		grouped = append(grouped, batch)
	}
	return grouped
}

// marshalledSize is what one entry contributes to the byte ceiling. A record
// that cannot be marshalled contributes nothing: Encode re-validates on the way
// out and drops it anyway, so counting its size would only shrink batches around
// a record that never reaches the wire.
func marshalledSize(entry store.Entry) int {
	marshalled, err := record.Marshal(entry.Record)
	if err != nil {
		return 0
	}
	return len(marshalled)
}

// gzipped compresses one payload. The compressed bytes are held for exactly one
// POST and then discarded — the payload is a projection, never state (ADR-0027).
func gzipped(payload []byte) ([]byte, error) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		return nil, errors.Join(err, writer.Close())
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

// post sends one gzipped batch and reports whether the receiver accepted it.
//
// Exactly four headers are set. Content-Encoding is not extra scope: a gzipped
// body without it is undecodable at the receiver, so it is implied by the
// requirement rather than added to it.
//
// No error this function returns carries the endpoint, the credential, or the
// response body. The body is drained into io.Discard rather than read into a
// message, because a receiver can echo the request back in its error text.
func post(endpoint, credential string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		// Replaced, not wrapped: NewRequest's error embeds the URL it could not
		// parse. Unreachable in practice — config validated the endpoint as an
		// absolute http:// or https:// URL on the way in.
		return errDeliveryFailed
	}
	req.Header.Set(contentTypeHeader, contentTypeJSON)
	req.Header.Set(contentEncodingHeader, encodingGzip)
	req.Header.Set(ingestionVersionHeader, ingestionVersion)
	req.Header.Set(authorizationHeader, "Basic "+base64.StdEncoding.EncodeToString([]byte(credential)))

	resp, err := deliveryClient.Do(req)
	if err != nil {
		return errDeliveryFailed
	}
	// Drained before the status is judged, so the connection is reusable for the
	// next batch of the same run.
	_, copyErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// The status code is a number this package produced no part of, so it is
		// safe to name and is the one thing worth naming.
		return fmt.Errorf("status %d: %w", resp.StatusCode, errDeliveryRejected)
	}
	if copyErr != nil || closeErr != nil {
		// The batch was accepted but the exchange did not complete cleanly, so
		// the run stops here without advancing. That costs a re-send, which the
		// receiver collapses. Valueless for the same reason Do's error is: a
		// transport error on this side can carry the connection's address too.
		return errDeliveryFailed
	}
	return nil
}
