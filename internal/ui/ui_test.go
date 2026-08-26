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
	"github.com/SupermodularAI/agents-wake/internal/record"
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
	if err := primitives.Refresh(source, inventory.Discovery{Primitives: []inventory.Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "review"}}, ProjectScanned: true}); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	response := httptest.NewRecorder()
	Handler(source, primitives).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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
	Handler(store.New(filepath.Join(t.TempDir(), "events.ndjson")), inventory.New(filepath.Join(t.TempDir(), "primitives.json"))).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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
	Handler(source, inventory.New(filepath.Join(t.TempDir(), "primitives.json"))).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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
	if err := primitives.Refresh(source, inventory.Discovery{Primitives: []inventory.Primitive{{Harness: "claude-code", Kind: record.KindBuiltinTool, Name: "Bash"}, {Harness: "claude-code", Kind: record.KindSkill, Name: "pr-review"}}, ProjectScanned: true}); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	response := httptest.NewRecorder()
	Handler(source, primitives).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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
	if err := primitives.Refresh(source, inventory.Discovery{Primitives: []inventory.Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "review"}}, ProjectScanned: true}); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	response := httptest.NewRecorder()
	Handler(source, primitives).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if !strings.Contains(body, "Errors") || !strings.Contains(body, "2 (66.7%)") {
		t.Fatalf("dashboard did not show review's per-primitive error count: %s", body)
	}
}

func TestHandlerShowsAvailablePrimitivesWithoutUsage(t *testing.T) {
	response := httptest.NewRecorder()
	available := []inventory.Primitive{{Harness: "claude-code", Kind: record.KindSkill, Name: "available-skill"}}
	source := store.New(filepath.Join(t.TempDir(), "events.ndjson"))
	primitives := inventory.New(filepath.Join(t.TempDir(), "primitives.json"))
	if err := primitives.Refresh(source, inventory.Discovery{Primitives: available, ProjectScanned: true}); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	Handler(source, primitives).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	for _, want := range []string{">available-skill<", "Unused primitives", "without any recorded activity"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard is missing %q: %s", want, body)
		}
	}
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
