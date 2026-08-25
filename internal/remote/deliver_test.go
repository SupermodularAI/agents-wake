package remote

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/lockfile"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// The credential every test configures. Both halves are distinctive strings so
// TestNoErrorPathLeaksTheEndpointOrCredential can search for either one, and for
// the base64 form the Authorization header carries.
const (
	testPublicKey  = "pk-lf-test"
	testSecretKey  = "sk-lf-test"
	testCredential = testPublicKey + ":" + testSecretKey
)

// captured is one request the spy received, with the body exactly as it arrived
// — still gzipped, because whether it is gzipped is one of the things under
// test.
type captured struct {
	header http.Header
	body   []byte
}

// spy is a test receiver that records every request and answers with the status
// its statuses slice names for that request's index, repeating the last one once
// the slice runs out. That is what lets a test say "batch 1 succeeds, batch 2
// fails" without knowing when the batches arrive.
//
// It never calls t.Fatal: it runs on the server's goroutine, where a Fatal would
// not stop the test it belongs to. A read failure is recorded and asserted from
// the test's own goroutine instead.
type spy struct {
	mu       sync.Mutex
	statuses []int
	got      []captured
	readErr  error
}

func (s *spy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)

	s.mu.Lock()
	if err != nil && s.readErr == nil {
		s.readErr = err
	}
	index := len(s.got)
	s.got = append(s.got, captured{header: r.Header.Clone(), body: body})
	status := http.StatusOK
	switch {
	case index < len(s.statuses):
		status = s.statuses[index]
	case len(s.statuses) > 0:
		status = s.statuses[len(s.statuses)-1]
	}
	s.mu.Unlock()

	w.WriteHeader(status)
}

func (s *spy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func (s *spy) request(t *testing.T, index int) captured {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		t.Fatalf("the receiver could not read a request body: %v", s.readErr)
	}
	if index >= len(s.got) {
		t.Fatalf("the receiver got %d requests, want at least %d", len(s.got), index+1)
	}
	return s.got[index]
}

// serve starts a receiver answering with statuses and returns it with its URL.
func serve(t *testing.T, statuses ...int) (*spy, string) {
	t.Helper()
	receiver := &spy{statuses: statuses}
	server := httptest.NewServer(receiver)
	t.Cleanup(server.Close)
	return receiver, server.URL
}

// enable writes a real credential store through the boundary that owns it and
// turns the minimum interval off, so a test's second flush is gated by "no new
// records" rather than by the clock.
func enable(t *testing.T, p config.Paths, endpoint string) {
	t.Helper()
	if err := config.SetRemoteAuth(p, config.RemoteAuth{Endpoint: endpoint, Enabled: true, Credential: testCredential}); err != nil {
		t.Fatalf("SetRemoteAuth() error = %v", err)
	}
	setMinInterval(t, p, "0s")
}

func setMinInterval(t *testing.T, p config.Paths, value string) {
	t.Helper()
	if _, err := config.Set(p, minIntervalKey, value); err != nil {
		t.Fatalf("Set(%s, %s) error = %v", minIntervalKey, value, err)
	}
}

// seed appends n distinct valid records to the spool. Each derives its own event
// id from its own source event, as ADR-0004 requires, so the store's own
// deduplication does not silently collapse them into one.
func seed(t *testing.T, p config.Paths, n int) {
	t.Helper()
	seedFrom(t, p, 0, n)
}

// seedFrom appends n records whose source events start at from, so a test can
// add records the spool does not already hold. Repeating seed would append
// nothing: the store deduplicates on the derived event id, which is the property
// that makes re-scanning safe (ADR-0004).
func seedFrom(t *testing.T, p config.Paths, from, n int) {
	t.Helper()
	result, err := store.New(eventsPath(p)).Append(testRecords(from, n))
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if result.Written != n {
		t.Fatalf("Append() wrote %d of %d records (%+v)", result.Written, n, result)
	}
}

func testRecords(from, n int) []record.Record {
	records := make([]record.Record, 0, n)
	for i := from; i < from+n; i++ {
		r := validRecord()
		r.EventID = record.DeriveEventID("claude-code", record.Identifier("source-event-"+strconv.Itoa(i)))
		records = append(records, r)
	}
	return records
}

func testEntries(n int) []store.Entry {
	records := testRecords(0, n)
	entries := make([]store.Entry, 0, n)
	for i, r := range records {
		entries = append(entries, store.Entry{Position: uint64(i) + 1, Record: r})
	}
	return entries
}

// shortTimeouts points delivery at a client that gives up in 100ms, so the
// dead-endpoint tests measure the timeout rather than wait out the real one.
func shortTimeouts(t *testing.T) {
	t.Helper()
	previous := deliveryClient
	deliveryClient = &http.Client{Timeout: 100 * time.Millisecond}
	t.Cleanup(func() { deliveryClient = previous })
}

func storedPosition(t *testing.T, p config.Paths) uint64 {
	t.Helper()
	return readDeliveryState(deliveryStatePath(p)).Position
}

func gunzip(t *testing.T, body []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the body is not gzipped: %v", err)
	}
	defer reader.Close()
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the gzipped body: %v", err)
	}
	return decompressed
}

func TestSuccessfulBatchAdvancesWatermarkToLastPosition(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)

	if err := Flush(paths); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if got := storedPosition(t, paths); got != 3 {
		t.Errorf("position = %d, want 3", got)
	}
	if got := receiver.count(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestServerErrorLeavesWatermarkUntouched(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusInternalServerError)
	enable(t, paths, endpoint)
	seed(t, paths, 3)

	err := Flush(paths)
	if err == nil {
		t.Fatal("Flush() error = nil, want a delivery failure")
	}
	if !errors.Is(err, ErrDeliveryRejected) {
		t.Errorf("Flush() error = %v, want ErrDeliveryRejected", err)
	}
	if got := storedPosition(t, paths); got != 0 {
		t.Errorf("position = %d, want 0 — a rejected batch must not advance the watermark", got)
	}
	if got := receiver.count(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

// TestPartialBatchAdvancesExactlyToTheLastSuccessfulBatch is the test the whole
// design exists for: 501 records split 500/1 by the record ceiling, the first
// batch accepted and the second refused. The watermark must land on 500 exactly.
// Advancing past it would open a gap nothing ever closes, and not advancing at
// all would re-send 500 records the receiver already has.
func TestPartialBatchAdvancesExactlyToTheLastSuccessfulBatch(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK, http.StatusInternalServerError)
	enable(t, paths, endpoint)
	seed(t, paths, maxBatchRecords+1)

	if err := Flush(paths); err == nil {
		t.Fatal("Flush() error = nil, want the second batch's failure")
	}
	if got := storedPosition(t, paths); got != maxBatchRecords {
		t.Errorf("position = %d, want exactly %d", got, maxBatchRecords)
	}
	if got := receiver.count(); got != 2 {
		t.Errorf("requests = %d, want 2 — the run must stop at the first failure", got)
	}
}

// TestAFailedBatchStopsTheRun is the other half of "the first non-2xx stops the
// run, so no gap can open", and the half the 501-record split above cannot
// reach: with only two batches, skipping the failed one and carrying on looks
// identical to stopping. Three batches tell them apart.
//
// If the run continued past the failure, batch 3 would be posted and accepted,
// and the watermark would advance to its last position — over records batch 2
// never delivered. The watermark is a single position and cannot describe a
// hole, so that gap would never be noticed and never be closed.
func TestAFailedBatchStopsTheRun(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK, http.StatusInternalServerError, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 2*maxBatchRecords+1)

	if err := Flush(paths); err == nil {
		t.Fatal("Flush() error = nil, want the second batch's failure")
	}
	if got := receiver.count(); got != 2 {
		t.Errorf("requests = %d, want 2 — the run continued past a failed batch", got)
	}
	if got := storedPosition(t, paths); got != maxBatchRecords {
		t.Errorf("position = %d, want exactly %d", got, maxBatchRecords)
	}
}

// TestWatermarkPastHeadResetsAndResends covers `ingest --rebuild`: store.Discard
// re-derives the spool, so a watermark past head means the store shrank. Reset
// and re-send rather than clamp — at-least-once is free because the receiver
// deduplicates on a span id derived from the deterministic event id, whereas
// clamping would skip records permanently.
func TestWatermarkPastHeadResetsAndResends(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)
	if err := writeDeliveryState(deliveryStatePath(paths), deliveryState{Position: 99}); err != nil {
		t.Fatalf("writeDeliveryState() error = %v", err)
	}

	if err := Flush(paths); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if got := storedPosition(t, paths); got != 3 {
		t.Errorf("position = %d, want 3", got)
	}
	if got := receiver.count(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	if got := spansOf(t, gunzip(t, receiver.request(t, 0).body)); len(got) != 3 {
		t.Errorf("payload carried %d spans, want all 3 re-sent", len(got))
	}
}

// TestDeadEndpointReturnsPromptly is ADR-0018's "no command ever waits on the
// network", in the two shapes that differ: a refused connection, which fails
// immediately, and a hung one, which fails only because the client carries an
// explicit timeout. Go's default client has none, so without one the second case
// would block forever.
func TestDeadEndpointReturnsPromptly(t *testing.T) {
	t.Run("closed listener", func(t *testing.T) {
		paths := testPaths(t)
		shortTimeouts(t)
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		endpoint := server.URL
		server.Close()

		enable(t, paths, endpoint)
		seed(t, paths, 3)
		assertPromptFailure(t, paths)
	})

	t.Run("hanging connection", func(t *testing.T) {
		paths := testPaths(t)
		shortTimeouts(t)
		block := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
		// LIFO: the handler is released before Close waits for it, so the
		// blocked request cannot deadlock the cleanup.
		t.Cleanup(server.Close)
		t.Cleanup(func() { close(block) })

		enable(t, paths, server.URL)
		seed(t, paths, 3)
		assertPromptFailure(t, paths)
	})
}

func assertPromptFailure(t *testing.T, paths config.Paths) {
	t.Helper()
	startedAt := time.Now()
	err := Flush(paths)
	elapsed := time.Since(startedAt)

	if err == nil {
		t.Fatal("Flush() error = nil, want a transport failure")
	}
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Errorf("Flush() error = %v, want ErrDeliveryFailed", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Flush() took %v, want under 2s — no command may wait on the network", elapsed)
	}
	if got := storedPosition(t, paths); got != 0 {
		t.Errorf("position = %d, want 0", got)
	}
}

func TestSecondFlushWithNoNewRecordsSendsNothing(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)

	if err := Flush(paths); err != nil {
		t.Fatalf("first Flush() error = %v", err)
	}
	if err := Flush(paths); err != nil {
		t.Fatalf("second Flush() error = %v", err)
	}
	if got := receiver.count(); got != 1 {
		t.Errorf("requests = %d, want 1 — an empty batch must not be posted", got)
	}
	if got := storedPosition(t, paths); got != 3 {
		t.Errorf("position = %d, want 3", got)
	}
}

// TestConcurrentFlushIsANoOp asserts single-flight, not a queue: a flush that
// finds the lock held has nothing to add by repeating what the holder is already
// doing, and a skipped run is not a failure (ADR-0018).
//
// flock is held per open file description, so a second acquisition inside this
// same process does not succeed — which is what makes the in-process holder a
// faithful stand-in for a detached child.
func TestConcurrentFlushIsANoOp(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)

	held := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- lockfile.WithLock(filepath.Join(paths.DataDir, deliveryLockName), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	err := Flush(paths)
	close(release)
	if holderErr := <-holderDone; holderErr != nil {
		t.Fatalf("the lock holder failed: %v", holderErr)
	}

	if err != nil {
		t.Errorf("Flush() error = %v, want nil — a skipped run is not a failure", err)
	}
	if got := receiver.count(); got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
	if got := storedPosition(t, paths); got != 0 {
		t.Errorf("position = %d, want 0", got)
	}
}

// TestNoRequestIsConstructedWhenDisabledOrUnconfigured is the acceptance
// criterion "a remote off build sends nothing even with a valid endpoint and
// credentials present", plus the two unconfigured shapes. The assertion is that
// nothing was constructed at all — no request, and no delivery state written —
// rather than that a request was built and then not sent.
func TestNoRequestIsConstructedWhenDisabledOrUnconfigured(t *testing.T) {
	cases := map[string]func(t *testing.T, p config.Paths, endpoint string){
		"off with a valid endpoint and credential": func(t *testing.T, p config.Paths, endpoint string) {
			t.Helper()
			if err := config.SetRemoteAuth(p, config.RemoteAuth{Endpoint: endpoint, Enabled: false, Credential: testCredential}); err != nil {
				t.Fatalf("SetRemoteAuth() error = %v", err)
			}
		},
		"endpoint unset with a credential present": func(t *testing.T, _ config.Paths, _ string) {
			t.Helper()
			t.Setenv(config.EnvRemoteAuthorization, testCredential)
		},
		"no credential store at all": func(*testing.T, config.Paths, string) {},
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			paths := testPaths(t)
			receiver, endpoint := serve(t, http.StatusOK)
			configure(t, paths, endpoint)
			seed(t, paths, 3)

			if err := Flush(paths); err != nil {
				t.Fatalf("Flush() error = %v, want nil", err)
			}
			if got := receiver.count(); got != 0 {
				t.Errorf("requests = %d, want 0", got)
			}
			if _, err := os.Stat(deliveryStatePath(paths)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Stat(delivery state) = %v, want ErrNotExist — nothing should have been written", err)
			}
		})
	}
}

// TestRequestCarriesTheRequiredHeadersAndGzippedBody pins the wire shape. Only
// four headers are asserted and only four are set: Content-Encoding is here
// because a gzipped body without it is undecodable at the receiver, so it is
// implied by "gzipped body" rather than added scope.
func TestRequestCarriesTheRequiredHeadersAndGzippedBody(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)

	if err := Flush(paths); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	got := receiver.request(t, 0)

	want := map[string]string{
		"Content-Type":                 "application/json",
		"Content-Encoding":             "gzip",
		"X-Langfuse-Ingestion-Version": "4",
		"Authorization":                "Basic " + base64.StdEncoding.EncodeToString([]byte(testCredential)),
	}
	for header, value := range want {
		if found := got.header.Get(header); found != value {
			t.Errorf("%s = %q, want %q", header, found, value)
		}
	}

	// The body is the encoder's output for exactly these records and nothing
	// else: one serialiser, no second projection built at delivery time
	// (ADR-0027).
	expected, dropped, err := Encode(testRecords(0, 3))
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if dropped != 0 {
		t.Fatalf("Encode() dropped = %d, want 0", dropped)
	}
	if body := gunzip(t, got.body); !bytes.Equal(body, expected) {
		t.Errorf("body = %s\nwant %s", body, expected)
	}
}

// TestBatchesRespectBothCeilings covers the record ceiling through batches and
// the byte ceiling through batchesWithin. The byte ceiling cannot be reached
// with a real record — every Record field is a bounded identifier, so no record
// marshals to anything near 4 MiB — so the property is asserted at a ceiling
// small enough to reach rather than left untested.
func TestBatchesRespectBothCeilings(t *testing.T) {
	t.Run("record ceiling", func(t *testing.T) {
		got := batches(testEntries(maxBatchRecords + 1))
		if len(got) != 2 {
			t.Fatalf("batches = %d, want 2", len(got))
		}
		if len(got[0]) != maxBatchRecords || len(got[1]) != 1 {
			t.Fatalf("batch sizes = %d and %d, want %d and 1", len(got[0]), len(got[1]), maxBatchRecords)
		}
		if got[0][maxBatchRecords-1].Position != maxBatchRecords {
			t.Errorf("first batch ends at position %d, want %d", got[0][maxBatchRecords-1].Position, maxBatchRecords)
		}
	})

	t.Run("an oversized record still forms a batch of one", func(t *testing.T) {
		got := batchesWithin(testEntries(3), maxBatchRecords, 1)
		if len(got) != 3 {
			t.Fatalf("batches = %d, want 3 — an oversized record must never be dropped or emitted in an empty batch", len(got))
		}
		for i, batch := range got {
			if len(batch) != 1 {
				t.Errorf("batch %d holds %d entries, want 1", i, len(batch))
			}
		}
	})

	t.Run("no entries produce no batches", func(t *testing.T) {
		if got := batches(nil); len(got) != 0 {
			t.Errorf("batches = %d, want 0 — an empty batch must never be posted", len(got))
		}
	})
}

// TestLastFlushIsNeverACursor is the mechanical proof of the acceptance
// criterion "no timestamp is used anywhere as a delivery cursor". The state file
// holds a time as well as a position, which is exactly the arrangement in which
// someone later selects records by the wrong one — so a far-future LastFlush
// must filter nothing out.
func TestLastFlushIsNeverACursor(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)
	future := time.Now().UTC().Add(72 * time.Hour)
	if err := writeDeliveryState(deliveryStatePath(paths), deliveryState{Position: 0, LastFlush: future}); err != nil {
		t.Fatalf("writeDeliveryState() error = %v", err)
	}

	if err := Flush(paths); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if got := receiver.count(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	if got := spansOf(t, gunzip(t, receiver.request(t, 0).body)); len(got) != 3 {
		t.Errorf("payload carried %d spans, want 3 — record selection consulted a timestamp", len(got))
	}
	if got := storedPosition(t, paths); got != 3 {
		t.Errorf("position = %d, want 3", got)
	}
}

// TestNoErrorPathLeaksTheEndpointOrCredential applies internal/config's boundary
// idiom to this package. ADR-0028's rule is "never echo what was read", and the
// easiest way to break it here is to wrap http.Client.Do's error: it returns a
// *url.Error that embeds the URL it failed on.
func TestNoErrorPathLeaksTheEndpointOrCredential(t *testing.T) {
	const endpointToken = "wake-boundary-endpoint.invalid"
	secrets := []string{
		endpointToken,
		testPublicKey,
		testSecretKey,
		base64.StdEncoding.EncodeToString([]byte(testCredential)),
	}

	cases := map[string]func(t *testing.T, p config.Paths){
		"closed listener": func(t *testing.T, p config.Paths) {
			t.Helper()
			shortTimeouts(t)
			enable(t, p, "http://"+endpointToken+"/v1/traces")
		},
		"rejected batch": func(t *testing.T, p config.Paths) {
			t.Helper()
			_, endpoint := serve(t, http.StatusInternalServerError)
			enable(t, p, endpoint)
		},
		"hanging connection": func(t *testing.T, p config.Paths) {
			t.Helper()
			shortTimeouts(t)
			block := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
			t.Cleanup(server.Close)
			t.Cleanup(func() { close(block) })
			enable(t, p, server.URL)
		},
		"a credential store this build refuses": func(t *testing.T, p config.Paths) {
			t.Helper()
			enable(t, p, "http://"+endpointToken+"/v1/traces")
			corruptCredentialStore(t, p)
		},
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			paths := testPaths(t)
			configure(t, paths)
			seed(t, paths, 3)

			err := Flush(paths)
			if err == nil {
				// Without this the case proves nothing about error messages.
				t.Fatal("Flush() error = nil, want a failure to inspect")
			}
			for _, secret := range secrets {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("Flush() error names a value it must never echo: %v", err)
				}
			}
		})
	}
}

// corruptCredentialStore makes the store unparseable without naming it. The file
// is found by listing the config root rather than spelled here, because
// internal/config's boundary test asserts that only that package names it — and
// the bytes being made unparseable are the credential itself, which is the point
// of the case.
func corruptCredentialStore(t *testing.T, p config.Paths) {
	t.Helper()
	entries, err := os.ReadDir(p.ConfigDir)
	if err != nil {
		t.Fatalf("ReadDir(config root) error = %v", err)
	}
	found := ""
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			found = filepath.Join(p.ConfigDir, entry.Name())
		}
	}
	if found == "" {
		t.Fatal("no credential store was written into the config root")
	}
	raw, err := os.ReadFile(found)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if err := os.WriteFile(found, append([]byte("{{"), raw...), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

// TestMinIntervalSuppressesTheSecondFlush covers the sizing knob ADR-0018 asks
// for: a burst of hook-triggered scans must not become a burst of flushes.
func TestMinIntervalSuppressesTheSecondFlush(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	setMinInterval(t, paths, "15m")
	seed(t, paths, 3)

	if err := Flush(paths); err != nil {
		t.Fatalf("first Flush() error = %v", err)
	}
	if got := receiver.count(); got != 1 {
		t.Fatalf("requests after the first flush = %d, want 1", got)
	}

	seedFrom(t, paths, 3, 3)
	if err := Flush(paths); err != nil {
		t.Fatalf("second Flush() error = %v", err)
	}
	if got := receiver.count(); got != 1 {
		t.Errorf("requests = %d, want 1 — the minimum interval did not suppress the second flush", got)
	}
	if got := storedPosition(t, paths); got != 3 {
		t.Errorf("position = %d, want 3", got)
	}
}

// TestStoreIsNotMutated is the acceptance criterion "internal/store/store.go is
// unmodified", asserted behaviourally rather than only by reading the diff: a
// per-record delivered flag would have to show up in these bytes.
func TestStoreIsNotMutated(t *testing.T) {
	paths := testPaths(t)
	_, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)

	before, err := os.ReadFile(eventsPath(paths))
	if err != nil {
		t.Fatalf("ReadFile(spool) error = %v", err)
	}
	if flushErr := Flush(paths); flushErr != nil {
		t.Fatalf("Flush() error = %v", flushErr)
	}
	after, err := os.ReadFile(eventsPath(paths))
	if err != nil {
		t.Fatalf("ReadFile(spool) error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("delivery rewrote the spool: the store is append-only and carries no delivered flag")
	}
}

// TestFlushReportCountsWhatItSent is the data `remote flush` prints. It is
// counted here rather than derived in internal/cli by differencing Describe
// across the call, which would be a second, subtly different answer to a
// question this package already knows (ADR-0001).
func TestFlushReportCountsWhatItSent(t *testing.T) {
	paths := testPaths(t)
	_, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 3)

	report, err := FlushReport(paths)
	if err != nil {
		t.Fatalf("FlushReport() error = %v", err)
	}
	if report.Batches != 1 {
		t.Errorf("Batches = %d, want 1", report.Batches)
	}
	if report.Records != 3 {
		t.Errorf("Records = %d, want 3", report.Records)
	}
	if report.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0", report.Dropped)
	}
}

// A run that sent nothing reports nothing. The zero value is what lets
// `remote flush` distinguish "delivery is off" from "delivered zero records"
// without a second flag saying which (plan §12).
func TestFlushReportIsZeroWhenDeliveryIsOff(t *testing.T) {
	paths := testPaths(t)
	seed(t, paths, 3)

	report, err := FlushReport(paths)
	if err != nil {
		t.Fatalf("FlushReport() error = %v", err)
	}
	if report != (Report{}) {
		t.Errorf("Report = %+v, want the zero value", report)
	}
}

// The counts must describe what the receiver accepted, not what the run
// attempted. A report that counted the refused batch would tell a user 501
// records left the machine when 500 did.
func TestFlushReportCountsOnlyAcceptedBatches(t *testing.T) {
	paths := testPaths(t)
	_, endpoint := serve(t, http.StatusOK, http.StatusInternalServerError)
	enable(t, paths, endpoint)
	seed(t, paths, maxBatchRecords+1)

	report, err := FlushReport(paths)
	if err == nil {
		t.Fatal("FlushReport() error = nil, want the second batch's failure")
	}
	if report.Batches != 1 {
		t.Errorf("Batches = %d, want 1 — only the accepted batch counts", report.Batches)
	}
	if report.Records != maxBatchRecords {
		t.Errorf("Records = %d, want %d", report.Records, maxBatchRecords)
	}
}

// TestReportFieldsAreExactly mirrors TestStatusFieldsAreExactly for the struct
// DG-66 adds. Equality rather than containment: `remote flush` prints this, and
// the temptation a later change will feel is to add "and here is why" as a
// string (ADR-0007).
func TestReportFieldsAreExactly(t *testing.T) {
	want := []string{"Batches", "Records", "Dropped", "Suppressed"}
	counts := []string{"Batches", "Records", "Dropped"}

	reportType := reflect.TypeOf(Report{})
	got := make([]string, 0, reportType.NumField())
	for i := range reportType.NumField() {
		field := reportType.Field(i)
		got = append(got, field.Name)

		// A count is an int and everything else is a bool. Stated as an
		// exhaustive pair rather than as "not a string", because the field this
		// admits is the first non-count one and the next change will feel the
		// same temptation the int-only rule was written against: "and here is
		// why", as a string (ADR-0007).
		wantKind := reflect.Bool
		if slices.Contains(counts, field.Name) {
			wantKind = reflect.Int
		}
		if field.Type.Kind() != wantKind {
			t.Errorf("Report.%s is %s, want %s", field.Name, field.Type, wantKind)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("Report fields = %v, want exactly %v", got, want)
	}
}

// A run the minimum interval held back is not a run that read the spool and
// found nothing: both are three zero counts and a nil error, so the report has
// to say which happened or `remote flush` cannot (DG-72, Issue 1).
func TestFlushReportSaysWhenTheMinimumIntervalSuppressedIt(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	setMinInterval(t, paths, "15m")
	seed(t, paths, 3)

	first, err := FlushReport(paths)
	if err != nil {
		t.Fatalf("first FlushReport() error = %v", err)
	}
	if first.Suppressed {
		t.Error("Suppressed = true on the first flush, which nothing held back")
	}

	seedFrom(t, paths, 3, 3)
	second, err := FlushReport(paths)
	if err != nil {
		t.Fatalf("second FlushReport() error = %v", err)
	}
	if !second.Suppressed {
		t.Error("Suppressed = false, want true — the minimum interval held this run back")
	}
	if second.Batches != 0 || second.Records != 0 || second.Dropped != 0 {
		t.Errorf("counts = %+v, want zeroes — a suppressed run reads nothing", second)
	}
	if got := receiver.count(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

// The other half of the same distinction: a run that did happen and had nothing
// to send is not suppressed, and stays the zero value.
func TestFlushReportIsNotSuppressedWhenThereIsNothingToSend(t *testing.T) {
	paths := testPaths(t)
	_, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)

	report, err := FlushReport(paths)
	if err != nil {
		t.Fatalf("FlushReport() error = %v", err)
	}
	if report.Suppressed {
		t.Error("Suppressed = true for a flush that ran and found nothing pending")
	}
	if report != (Report{}) {
		t.Errorf("Report = %+v, want the zero value", report)
	}
}
