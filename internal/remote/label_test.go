package remote

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// registerRepo consents a real directory and seeds n spool records attributed to
// it, returning the id and the label recorded for it. The label has to come from a
// real Register: the id is derived under the machine salt, so a hand-written table
// entry would be refused rather than resolved (ADR-0019 §3).
func registerRepo(t *testing.T, p config.Paths, label string, n int) (record.Hash, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), label)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("creating the repository root: %v", err)
	}
	repos, err := config.OpenRepos(p)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	id, err := repos.Register(root, label, time.Time{})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	records := testRecords(0, n)
	for i := range records {
		records[i].Repo = record.Hash(id)
	}
	if _, err := store.New(eventsPath(p)).Append(records); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	return record.Hash(id), label
}

// stringValueOf returns the stringValue of one attribute, and reports whether the
// attribute was emitted at all. The two answers are separate because "absent" and
// "blank" are different outcomes on this wire (ADR-0027).
func stringValueOf(t *testing.T, attrs map[string]map[string]any, key string) (string, bool) {
	t.Helper()
	attr, present := attrs[key]
	if !present {
		return "", false
	}
	value, ok := attr["stringValue"].(string)
	if !ok {
		t.Fatalf("%s stringValue is %T, want a string", key, attr["stringValue"])
	}
	return value, true
}

// spanAttributesOf gunzips a captured request body and flattens the attributes of
// every span it carries.
func spanAttributesOf(t *testing.T, body []byte) []map[string]map[string]any {
	t.Helper()
	spans := spansOf(t, gunzip(t, body))
	flattened := make([]map[string]map[string]any, 0, len(spans))
	for _, span := range spans {
		flattened = append(flattened, attributesOf(t, span, "attributes"))
	}
	return flattened
}

// TestRepoLabelTravelsBesideTheHash is the acceptance criterion: the label rides
// beside the hash, and does not replace it. The hash is still the join key
// (ADR-0033 §4).
func TestRepoLabelTravelsBesideTheHash(t *testing.T) {
	attrs := attributesOf(t, encodeOneLabelled(t, fullRecord(), testLabels()), "attributes")

	hash, present := stringValueOf(t, attrs, "wake.repo")
	if !present {
		t.Fatal("wake.repo is absent; the hashed id is unconditional and is the join key")
	}
	if want := string(fullRecord().Repo); hash != want {
		t.Errorf("wake.repo = %q, want %q; the label must not replace the hash", hash, want)
	}
	label, present := stringValueOf(t, attrs, "wake.repo_label")
	if !present {
		t.Fatal("wake.repo_label is absent for a repository that has a recorded label")
	}
	if label != testLabel {
		t.Errorf("wake.repo_label = %q, want %q", label, testLabel)
	}
}

// TestNoRecordedLabelEmitsNoLabelAttribute pins "absent, never blank". An empty
// stringValue is indistinguishable at the receiver from a real value, so a
// repository with nothing recorded sends the hash alone.
//
// The fixture is the trace root, so both projections of the label — wake.repo_label
// and langfuse.trace.name — are in play under one set of cases.
func TestNoRecordedLabelEmitsNoLabelAttribute(t *testing.T) {
	for name, labels := range map[string]RepoLabels{
		"a nil map":                    nil,
		"an empty map":                 {},
		"a map keyed by another repo":  {"ffffffffffffffffffffffffffffffff": "elsewhere"},
		"a map holding an empty label": {string(fullSessionEndRecord().Repo): ""},
	} {
		t.Run(name, func(t *testing.T) {
			payload, dropped, err := Encode([]record.Record{fullSessionEndRecord()}, labels)
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			if dropped != 0 {
				t.Fatalf("Encode() dropped = %d, want 0; a missing label never drops a span", dropped)
			}
			spans := spansOf(t, payload)
			if len(spans) != 1 {
				t.Fatalf("Encode() emitted %d spans, want 1", len(spans))
			}
			attrs := attributesOf(t, spans[0], "attributes")
			if _, present := stringValueOf(t, attrs, "wake.repo"); !present {
				t.Error("wake.repo is absent; the hashed id is unconditional")
			}
			for _, key := range []string{"wake.repo_label", "langfuse.trace.name"} {
				if _, present := attrs[key]; present {
					t.Errorf("%s was emitted with no label recorded; it must be absent, never blank", key)
				}
				if bytes.Contains(payload, []byte(key)) {
					t.Errorf("the payload names %q; the key itself must not appear when there is no label", key)
				}
			}
		})
	}
}

// labelCorpus is this package's hostile corpus plus the label-specific edges: the
// length bound, the two whitespace forms BoundedToken would repair rather than
// refuse, a leading separator character, a Windows drive prefix, and an internal
// space.
//
// It is deliberately not partitioned by hand. Each case asks record.BoundedToken
// itself whether the value passes untouched, and then asserts the outcome the
// encoder owes for that answer — so the test states the contract rather than a
// snapshot of which corpus entries happen to fail today. Hand-splitting the list
// would let a widened grammar move an entry across the line and still pass.
var labelCorpus = slices.Concat(hostileIdentifiers, []string{
	strings.Repeat("a", 129), " agents-wake", "agents-wake ", "-leading-dash",
	"C:label", "UPPER lower",
})

// TestALabelThatFailsValidationEmitsNoLabelAttribute is the fail-closed half of
// ADR-0033 §3, run against the trace root so one corpus pass covers both projections
// of the label. Every refusal produces the same output — no attribute — because a
// truncated, escaped, or sanitised repository name is a wrong answer that looks
// like a right one. A refused label never drops the span it belongs to.
//
// One corpus entry passes, and that is correct rather than a hole:
// "sk-ant-api03-DEADBEEF" is a well-formed bounded token, and it is in the corpus
// because an API-key-shaped string appearing in a *record identifier* means
// transcript content leaked into a field derived from a harness log. A label is not
// derived from a log — it is what `wake init` recorded for a directory the user
// consented — and no rule distinguishes a secret-shaped directory name from a real
// one. Refusing it would take a denylist no ADR asks for, and would silently drop
// the label of a legitimately-named repository. So the assertion for a passing
// value is that it is emitted verbatim: neither refused nor repaired.
func TestALabelThatFailsValidationEmitsNoLabelAttribute(t *testing.T) {
	admitted := 0
	for _, candidate := range labelCorpus {
		t.Run(strconv.Quote(candidate), func(t *testing.T) {
			token, tokenErr := record.BoundedToken(candidate)
			passes := tokenErr == nil && string(token) == candidate
			if passes {
				admitted++
			}

			r := fullSessionEndRecord()
			payload, dropped, err := Encode([]record.Record{r}, RepoLabels{string(r.Repo): candidate})
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			if dropped != 0 {
				t.Fatalf("Encode() dropped = %d, want 0; a bad label never drops a record", dropped)
			}
			spans := spansOf(t, payload)
			if len(spans) != 1 {
				t.Fatalf("Encode() emitted %d spans, want 1", len(spans))
			}
			attrs := attributesOf(t, spans[0], "attributes")
			if _, present := stringValueOf(t, attrs, "wake.repo"); !present {
				t.Error("wake.repo is absent; a refused label must not cost the hash")
			}

			for _, key := range []string{"wake.repo_label", "langfuse.trace.name"} {
				label, present := stringValueOf(t, attrs, key)
				if !passes {
					if present {
						t.Errorf("%s was emitted for a label that fails validation", key)
					}
					continue
				}
				if !present {
					t.Errorf("%s is absent for a label that passes validation; a passing value is emitted, not dropped", key)
					continue
				}
				if label != candidate {
					t.Errorf("%s = %q, want %q verbatim; a passing value is never repaired", key, label, candidate)
				}
			}
			if !passes && candidate != "" && bytes.Contains(payload, []byte(candidate)) {
				t.Error("a refused label reached the payload")
			}
		})
	}

	// Both branches above have to run, or the refusal assertions would be the only
	// thing this test ever checks and a grammar that refused everything would pass.
	if admitted == 0 {
		t.Error("no corpus value passed validation; the emit branch was never exercised")
	}
}

// TestNoPathSeparatorReachesThePayloadThroughALabel is the label's version of
// TestPayloadHasNoPathSeparator: the new attribute must not become the way a path
// fragment gets onto the wire. Nothing this encoder legitimately emits contains a
// separator, so the assertion is over the whole payload (plan §3.4, BC-3).
func TestNoPathSeparatorReachesThePayloadThroughALabel(t *testing.T) {
	// Both fixtures, so the check covers the trace root's projection of the label as
	// well as wake.repo_label's.
	for _, r := range []record.Record{fullRecord(), fullSessionEndRecord()} {
		for _, hostile := range hostileIdentifiers {
			if !strings.ContainsAny(hostile, `/\`) {
				continue
			}
			t.Run(string(r.Kind)+"/"+strconv.Quote(hostile), func(t *testing.T) {
				payload, _, err := Encode([]record.Record{r}, RepoLabels{string(r.Repo): hostile})
				if err != nil {
					t.Fatalf("Encode() error = %v", err)
				}
				if bytes.ContainsAny(payload, `/\`) {
					t.Fatal("a path separator reached the payload through the repository label")
				}
			})
		}
	}
}

// TestLabelDoesNotWidenTheAlwaysPresentKeySet is BC-8: the key set grows by
// exactly one key, and that key is conditional. A minimal record with no label
// still emits exactly the always-present set.
func TestLabelDoesNotWidenTheAlwaysPresentKeySet(t *testing.T) {
	unlabelled := attributeKeys(attributesOf(t, encodeOneLabelled(t, validRecord(), nil), "attributes"))
	if !slices.Equal(unlabelled, frozenAlwaysPresentKeys) {
		t.Fatalf("unlabelled span attribute keys = %v, always-present set = %v", unlabelled, frozenAlwaysPresentKeys)
	}

	want := slices.Sorted(slices.Values(append(slices.Clone(frozenAlwaysPresentKeys), "wake.repo_label")))
	labelled := attributeKeys(attributesOf(t, encodeOneLabelled(t, validRecord(), testLabels()), "attributes"))
	if !slices.Equal(labelled, want) {
		t.Fatalf("labelled span attribute keys = %v, want the always-present set plus wake.repo_label: %v", labelled, want)
	}

	// The same at the trace root, where the label has a second projection: no label
	// still means no key at all, even on the span that names the trace.
	end := validRecord()
	end.Kind, end.Name, end.Invoker = record.KindSessionEnd, "session", record.InvokerAuto

	unlabelledRoot := attributeKeys(attributesOf(t, encodeOneLabelled(t, end, nil), "attributes"))
	if !slices.Equal(unlabelledRoot, frozenAlwaysPresentKeys) {
		t.Fatalf("unlabelled trace-root attribute keys = %v, always-present set = %v", unlabelledRoot, frozenAlwaysPresentKeys)
	}

	wantRoot := slices.Sorted(slices.Values(append(slices.Clone(frozenAlwaysPresentKeys), "wake.repo_label", "langfuse.trace.name")))
	labelledRoot := attributeKeys(attributesOf(t, encodeOneLabelled(t, end, testLabels()), "attributes"))
	if !slices.Equal(labelledRoot, wantRoot) {
		t.Fatalf("labelled trace-root attribute keys = %v, want the always-present set plus both label projections: %v", labelledRoot, wantRoot)
	}
}

// TestFlushSendsTheRepoLabelForAConsentedRepository is the end-to-end criterion
// through the real internal/config path: a repository consented by `wake init`
// has its label on the wire, keyed to the id its own salt derives.
func TestFlushSendsTheRepoLabelForAConsentedRepository(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	id, label := registerRepo(t, paths, "alpha", 3)

	if _, err := FlushReport(paths); err != nil {
		t.Fatalf("FlushReport() error = %v", err)
	}
	if got := receiver.count(); got != 1 {
		t.Fatalf("the receiver got %d requests, want 1", got)
	}

	spans := spanAttributesOf(t, receiver.request(t, 0).body)
	if len(spans) != 3 {
		t.Fatalf("the posted batch carries %d spans, want 3", len(spans))
	}
	for i, attrs := range spans {
		if got, _ := stringValueOf(t, attrs, "wake.repo"); got != string(id) {
			t.Errorf("span %d wake.repo = %q, want the registered id %q", i, got, string(id))
		}
		got, present := stringValueOf(t, attrs, "wake.repo_label")
		if !present {
			t.Errorf("span %d carries no wake.repo_label for a consented repository", i)
			continue
		}
		if got != label {
			t.Errorf("span %d wake.repo_label = %q, want %q", i, got, label)
		}
	}
}

// TestFlushNamesTheTraceAfterTheRepository is DG-94 end to end through the real
// internal/config path: a repository consented by `wake init` names the trace of every
// session anchored in it, keyed to the id its own salt derives. The name rides on the
// session_end span alone, so a batch carrying a whole session posts exactly one span
// that names its trace.
func TestFlushNamesTheTraceAfterTheRepository(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	id, label := registerRepo(t, paths, "alpha", 3)

	end := fullSessionEndRecord()
	end.Repo = id
	if _, err := store.New(eventsPath(paths)).Append([]record.Record{end}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	if _, err := FlushReport(paths); err != nil {
		t.Fatalf("FlushReport() error = %v", err)
	}

	named := 0
	for i, attrs := range spanAttributesOf(t, receiver.request(t, 0).body) {
		got, present := stringValueOf(t, attrs, "langfuse.trace.name")
		if !present {
			continue
		}
		named++
		if got != label {
			t.Errorf("span %d langfuse.trace.name = %q, want %q", i, got, label)
		}
	}
	if named != 1 {
		t.Errorf("%d posted spans name the trace, want exactly the session_end span", named)
	}
}

// TestPreviewAndFlushAgreeOnTheRepoLabel is the byte-identity clause with a label
// in play: `--dry-run` prints the exact bytes a flush would send, and that stops
// being true the moment the two resolve labels differently (ADR-0030).
func TestPreviewAndFlushAgreeOnTheRepoLabel(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	registerRepo(t, paths, "alpha", 3)

	preview, err := PreviewFlush(paths)
	if err != nil {
		t.Fatalf("PreviewFlush() error = %v", err)
	}
	if len(preview.Batches) != 1 {
		t.Fatalf("PreviewFlush() shows %d batches, want 1", len(preview.Batches))
	}
	// Asserted before the equality below, so the equality cannot pass vacuously
	// over a payload that carries no label at all.
	if !bytes.Contains(preview.Batches[0], []byte("wake.repo_label")) {
		t.Fatal("the preview carries no wake.repo_label; the equality below would be vacuous")
	}

	if _, err := FlushReport(paths); err != nil {
		t.Fatalf("FlushReport() error = %v", err)
	}
	if got := gunzip(t, receiver.request(t, 0).body); !bytes.Equal(got, preview.Batches[0]) {
		t.Errorf("the posted batch differs from the preview:\n%s\n%s", got, preview.Batches[0])
	}
}

// TestFlushSendsNoLabelWithNoProjectTable covers ProjectLabels' fail-soft path on
// the real flush path: no table means no label, and never a failed flush
// (ADR-0018).
func TestFlushSendsNoLabelWithNoProjectTable(t *testing.T) {
	paths := testPaths(t)
	receiver, endpoint := serve(t, http.StatusOK)
	enable(t, paths, endpoint)
	seed(t, paths, 2)

	if _, err := FlushReport(paths); err != nil {
		t.Fatalf("FlushReport() error = %v", err)
	}
	body := gunzip(t, receiver.request(t, 0).body)
	if bytes.Contains(body, []byte("wake.repo_label")) {
		t.Error("a label was sent for a repository no table maps")
	}
	for i, attrs := range spanAttributesOf(t, receiver.request(t, 0).body) {
		if _, present := stringValueOf(t, attrs, "wake.repo"); !present {
			t.Errorf("span %d carries no wake.repo; the hash is unconditional", i)
		}
	}
}

// TestNoLabelFieldReachedTheRecordOrTheSpool is the acceptance criterion "Record
// and the spool are unchanged". ADR-0033 narrowed where the label may go, not
// what a record is: the label is a flush-time projection, exactly as wake.repo
// already is, and it must not have arrived as a field on the way through
// (ADR-0007, BC-10).
//
// A forbidden-substring net rather than an exact field list, mirroring
// internal/config's TestExportedTypesCarryNoPathOrLabelField: whoever adds the
// field has to justify the name as well as the field.
func TestNoLabelFieldReachedTheRecordOrTheSpool(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[record.Record](),
		reflect.TypeFor[store.Entry](),
	} {
		for i := range typ.NumField() {
			name := strings.ToLower(typ.Field(i).Name)
			for _, forbidden := range []string{"label", "path", "root", "dir", "cwd"} {
				if strings.Contains(name, forbidden) {
					t.Errorf("%s.%s names a repository location or label; the label reaches the wire only as a flush-time projection", typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}
