package ui

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/repolabel"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

func TestHandlerRendersStoredMetrics(t *testing.T) {
	source := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	ok := record.OutcomeOK
	failed := record.OutcomeError
	if _, err := source.Append([]record.Record{event("one", &ok), event("two", &failed)}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := inventory.New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(source, inventory.Discovery{Primitives: []inventory.Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "review"}}, ProjectScanned: true}, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	response := httptest.NewRecorder()
	Handler(source, primitives, nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	for _, want := range []string{"Terminal invocations", ">2<", "50.0%", "review", "Primitive usage", "Unused primitives", "Local-only telemetry"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard is missing %q: %s", want, body)
		}
	}
}

func TestHandlerRendersEmptyState(t *testing.T) {
	response := httptest.NewRecorder()
	Handler(store.New(filepath.Join(t.TempDir(), "events.ndjson")), inventory.New(filepath.Join(t.TempDir(), "primitives.json")), nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "No primitive inventory or terminal events yet") {
		t.Fatalf("empty dashboard = %d: %s", response.Code, response.Body.String())
	}
}

// TestHandlerDoesNotClaimAnEmptyStoreForASessionWithNoPrimitiveUse is the plan §2.7
// baseline at the dashboard. A session that invoked no primitive contributes nothing
// to Invocations by design, so a gate keyed on Invocations alone hides the whole view
// behind an empty state for a store that holds a terminal session_end — the exact
// population the session grain exists to expose.
func TestHandlerDoesNotClaimAnEmptyStoreForASessionWithNoPrimitiveUse(t *testing.T) {
	source := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := source.Append([]record.Record{sessionEnd("session-1")}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	response := httptest.NewRecorder()
	Handler(source, inventory.New(filepath.Join(t.TempDir(), "primitives.json")), nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if strings.Contains(body, "No primitive inventory or terminal events yet") {
		t.Fatalf("dashboard called a store holding a session_end empty: %s", body)
	}
	for _, want := range []string{"Distinct sessions", ">1<"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard is missing %q: %s", want, body)
		}
	}
	// The tile counts this session, so it cannot go on calling its population
	// "with primitive activity" — this one had none.
	if strings.Contains(body, "with primitive activity") {
		t.Fatalf("session tile still claims every counted session had primitive activity: %s", body)
	}
}

func TestHandlerExcludesBuiltinToolsFromPrimitiveTable(t *testing.T) {
	source := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	ok := record.OutcomeOK
	builtin := event("builtin", &ok)
	builtin.Name = "Bash"
	builtin.Kind = record.KindBuiltinTool
	skill := event("skill", &ok)
	skill.Name = "pr-review"
	if _, err := source.Append([]record.Record{builtin, skill}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := inventory.New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(source, inventory.Discovery{Primitives: []inventory.Primitive{{Harness: "claude-code", Kind: record.KindBuiltinTool, Name: "Bash"}, {Harness: "claude-code", Kind: record.KindSkill, Name: "pr-review"}}, ProjectScanned: true}, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	response := httptest.NewRecorder()
	Handler(source, primitives, nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if strings.Contains(body, ">Bash<") || !strings.Contains(body, ">pr-review<") {
		t.Fatalf("primitive table did not filter built-ins: %s", body)
	}
}

func TestHandlerShowsPerPrimitiveErrorCount(t *testing.T) {
	source := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	ok := record.OutcomeOK
	failed := record.OutcomeError
	if _, err := source.Append([]record.Record{event("one", &ok), event("two", &failed), event("three", &failed)}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := inventory.New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(source, inventory.Discovery{Primitives: []inventory.Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "review"}}, ProjectScanned: true}, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	response := httptest.NewRecorder()
	Handler(source, primitives, nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if !strings.Contains(body, "Errors") || !strings.Contains(body, "2 of 3 rated (66.7%)") {
		t.Fatalf("dashboard did not show review's per-primitive error count: %s", body)
	}
}

// TestHandlerShowsAPartiallyRatedPrimitiveWithItsRatedPopulation is DG-103 at
// the dashboard: 2 calls, 1 error, 1 outcome the harness never reported. The
// rate is 100% of the single rated call, and the cell has to say so — beside a
// Calls column reading 2, a bare "1 (100.0%)" reads as every call failing.
func TestHandlerShowsAPartiallyRatedPrimitiveWithItsRatedPopulation(t *testing.T) {
	source := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	failed := record.OutcomeError
	if _, err := source.Append([]record.Record{event("one", &failed), event("two", nil)}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := inventory.New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(source, inventory.Discovery{Primitives: []inventory.Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "review"}}, ProjectScanned: true}, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	response := httptest.NewRecorder()
	Handler(source, primitives, nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if !strings.Contains(body, "1 of 1 rated (100.0%); 1 unrated") {
		t.Fatalf("dashboard error cell lost its rated population: %s", body)
	}
	if strings.Contains(body, "1 (100.0%)") {
		t.Fatalf("dashboard still prints a percentage with no denominator: %s", body)
	}
}

func TestHandlerShowsAvailablePrimitivesWithoutUsage(t *testing.T) {
	response := httptest.NewRecorder()
	available := []inventory.Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "available-skill"}}
	source := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	primitives := inventory.New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(source, inventory.Discovery{Primitives: available, ProjectScanned: true}, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	Handler(source, primitives, nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	for _, want := range []string{">available-skill<", "Unused primitives", "without any recorded activity"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard is missing %q: %s", want, body)
		}
	}
}

// TestHandlerShowsARepositoryColumnPerRepository is DG-93 on the dashboard: the
// same grain the terminal report renders, checked in the same change.
func TestHandlerShowsARepositoryColumnPerRepository(t *testing.T) {
	const labelled, unlabelled = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	source := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := source.Append([]record.Record{repoEvent("here", labelled), repoEvent("there", unlabelled)}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := inventory.New(filepath.Join(t.TempDir(), "primitives.json"))
	discovered := inventory.Discovery{Primitives: []inventory.Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "review"}}, ProjectScanned: true}
	if err := primitives.Refresh(source, discovered, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	response := httptest.NewRecorder()
	Handler(source, primitives, repolabel.Labels{labelled: "agents-wake"}, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	for _, want := range []string{">Project<", ">agents-wake<", ">repo-fedcba987654<"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard is missing %q: %s", want, body)
		}
	}
}

// TestHandlerShowsNoRepositoryColumnForUnusedPrimitives: the usage table has the
// column, the unused table does not — an uninvoked primitive has no repository
// (ADR-0002), so exactly one Project header appears in a body rendering both tables.
func TestHandlerShowsNoRepositoryColumnForUnusedPrimitives(t *testing.T) {
	ok := record.OutcomeOK
	source := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := source.Append([]record.Record{event("one", &ok)}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := inventory.New(filepath.Join(t.TempDir(), "primitives.json"))
	discovered := inventory.Discovery{Primitives: []inventory.Primitive{
		{Harness: "claude-code", Kind: record.KindSkill, Name: "review"},
		{Harness: "claude-code", Kind: record.KindSkill, Name: "never-used"},
	}, ProjectScanned: true}
	if err := primitives.Refresh(source, discovered, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	response := httptest.NewRecorder()
	Handler(source, primitives, nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	for _, want := range []string{">review<", ">never-used<"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard did not render both tables (%q missing): %s", want, body)
		}
	}
	if count := strings.Count(body, ">Project<"); count != 1 {
		t.Fatalf("Project header count = %d, want 1 (the usage table only): %s", count, body)
	}
}

func TestHandlerMakesNoClaimThatRepositoryLabelsAreNeverShown(t *testing.T) {
	response := httptest.NewRecorder()
	Handler(store.New(filepath.Join(t.TempDir(), "events.ndjson")), inventory.New(filepath.Join(t.TempDir(), "primitives.json")), nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if strings.Contains(body, "repository labels") {
		t.Fatalf("dashboard still claims repository labels are never shown: %s", body)
	}
	if !strings.Contains(body, "repository paths") {
		t.Fatalf("dashboard dropped its claim about repository paths: %s", body)
	}
}

func repoEvent(id string, repo record.Hash) record.Record {
	r := event(id, nil)
	r.Repo = repo
	return r
}

func event(id string, outcome *record.Outcome) record.Record {
	return record.Record{SchemaVersion: record.SchemaVersion, EventID: record.DeriveEventID("claude-code", record.Identifier(id)), Timestamp: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC), Harness: "claude-code", SessionID: "session-1", Repo: "0123456789abcdef0123456789abcdef", Kind: record.KindSkill, Name: "review", Invoker: record.InvokerModel, Outcome: outcome}
}

// sessionEnd is a session that invoked nothing: no outcome, no duration, and zero
// counted calls (ADR-0002's session grain).
func sessionEnd(sessionID record.Identifier) record.Record {
	var zero int64
	return record.Record{SchemaVersion: record.SchemaVersion, EventID: record.DeriveEventID("claude-code", sessionID+"\x1esession_end"), Timestamp: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC), Harness: "claude-code", SessionID: sessionID, Repo: "0123456789abcdef0123456789abcdef", Kind: record.KindSessionEnd, Name: "session", Invoker: record.InvokerAuto, ToolCalls: &zero, BuiltinToolCalls: &zero}
}

func TestListenBindsLoopbackOnly(t *testing.T) {
	listener, err := Listen(0)
	if err != nil {
		t.Fatalf("Listen(0) error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if got := listener.Addr().String(); !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("Listen(0) bound %s, want a 127.0.0.1 address", got)
	}
}

// Acceptance: a port collision is reported, so the caller can decline to
// announce a dashboard that is not listening.
func TestListenReportsAnOccupiedPort(t *testing.T) {
	first, err := Listen(0)
	if err != nil {
		t.Fatalf("Listen(0) error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	port := first.Addr().(*net.TCPAddr).Port
	second, err := Listen(port)
	if err == nil {
		_ = second.Close()
		t.Fatalf("Listen(%d) on an occupied port = nil error, want a failure", port)
	}
}

func TestServerBoundsEveryRequestPhase(t *testing.T) {
	limits := defaultTimeouts()
	server := newServer(nil, limits)
	for _, phase := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", server.ReadHeaderTimeout, limits.Header},
		{"ReadTimeout", server.ReadTimeout, limits.Read},
		{"WriteTimeout", server.WriteTimeout, limits.Write},
		{"IdleTimeout", server.IdleTimeout, limits.Idle},
	} {
		if phase.got != phase.want {
			t.Errorf("%s = %v, want %v", phase.name, phase.got, phase.want)
		}
		if phase.got <= 0 {
			t.Errorf("%s is unbounded", phase.name)
		}
	}
}

// Acceptance: a half-written request cannot hold a connection indefinitely. The
// headers below are deliberately never terminated; the header timeout is what
// makes the read return instead of blocking until the client gives up.
func TestPartialRequestDoesNotHoldTheConnection(t *testing.T) {
	listener, err := Listen(0)
	if err != nil {
		t.Fatalf("Listen(0) error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	handler := Handler(
		store.New(filepath.Join(t.TempDir(), "events.ndjson")),
		inventory.New(filepath.Join(t.TempDir(), "primitives.json")),
		nil,
		nil,
	)
	go func() {
		_ = serve(listener, handler, timeouts{Header: 50 * time.Millisecond, Read: 100 * time.Millisecond, Write: time.Second, Idle: 100 * time.Millisecond})
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: dashboard\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	// A deadline error here means the server kept the half-written request open.
	if _, err := io.ReadAll(conn); err != nil {
		t.Errorf("reading a half-written request's connection error = %v, want the server to close it", err)
	}
}

// TestViewLabelsAnUnmatchedServer pins that the dashboard reads its kind label from
// the same accessor `wake report` does. Two renderers each deriving a display value
// from raw fields is how the ERRORS cell drifted (DG-103).
func TestViewLabelsAnUnmatchedServer(t *testing.T) {
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	result := view(metrics.Aggregate(nil, nil), []inventory.Usage{
		{Harness: "claude-code", Kind: record.KindMCPServer, Name: "linear-server", Repo: "0123456789abcdef0123456789abcdef", Invocations: 3, Unmatched: true, LastUsed: at},
	}, repolabel.Labels{})

	if len(result.Usage) != 1 || len(result.Unused) != 0 {
		t.Fatalf("view() = %+v, want the used server in Usage", result)
	}
	if result.Usage[0].Kind != "mcp server (unmatched)" {
		t.Errorf("Kind = %q, want %q", result.Usage[0].Kind, "mcp server (unmatched)")
	}
}

// AC 3 at the dashboard. It goes through view() rather than a rendered page
// because the point is that the dashboard reads the same cell renderer
// `wake report` does: two renderers each deriving a display value from raw
// fields is how the ERRORS cell drifted (DG-103), and a second copy of this rule
// is the thing to prevent, not the thing to test twice.
func TestViewMarksASubagentRowWithNoRatedPopulationAsUnrated(t *testing.T) {
	at := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	result := view(metrics.Aggregate(nil, nil), []inventory.Usage{
		{Harness: "claude-code", Kind: record.KindSubagent, Name: "explorer", Repo: "0123456789abcdef0123456789abcdef", Invocations: 3, Unknown: 3, LastUsed: at},
	}, repolabel.Labels{})

	if len(result.Usage) != 1 {
		t.Fatalf("view() = %+v, want the used subagent in Usage", result)
	}
	if result.Usage[0].Errors == "0" {
		t.Errorf("Errors = %q, a bare zero that reads as health for a kind nothing has rated", result.Usage[0].Errors)
	}
	if result.Usage[0].Errors != "unrated (0 of 3 rated)" {
		t.Errorf("Errors = %q, want %q", result.Usage[0].Errors, "unrated (0 of 3 rated)")
	}
}

// TestHandlerNamesTheProjectColumnAndNeverTheSessionGrain is DG-105 on the dashboard,
// the same check the terminal report carries. The usage section names the project an
// invocation is attributed to (ADR-0002), and does not reach for "session" — the
// session grain is a different thing and ADR-0038 §2 rejected anchoring to it.
func TestHandlerNamesTheProjectColumnAndNeverTheSessionGrain(t *testing.T) {
	const labelled = "0123456789abcdef0123456789abcdef"
	source := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	if _, err := source.Append([]record.Record{repoEvent("here", labelled)}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	primitives := inventory.New(filepath.Join(t.TempDir(), "primitives.json"))
	discovered := inventory.Discovery{Primitives: []inventory.Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "review"}}, ProjectScanned: true}
	if err := primitives.Refresh(source, discovered, nil); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	response := httptest.NewRecorder()
	Handler(source, primitives, repolabel.Labels{labelled: "agents-wake"}, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	const note = "Project is the project each invocation's own working directory resolved to"
	start := strings.Index(body, note)
	if start < 0 {
		t.Fatalf("dashboard does not say what the Project column holds: %s", body)
	}
	end := strings.Index(body[start:], "</section>")
	if end < 0 {
		t.Fatalf("usage section is unterminated: %s", body)
	}
	section := body[start : start+end]
	if strings.Contains(strings.ToLower(section), "session") {
		t.Errorf("the Project column's copy names the session grain: %s", section)
	}
	if strings.Contains(body, ">Repo<") {
		t.Errorf("dashboard still headers the column Repo: %s", body)
	}
}
