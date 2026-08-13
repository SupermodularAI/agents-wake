package ui

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	response := httptest.NewRecorder()
	Handler(source).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	for _, want := range []string{"Terminal invocations", ">2<", "50.0%", "review", "Local-only telemetry"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard is missing %q: %s", want, body)
		}
	}
}

func TestHandlerRendersEmptyState(t *testing.T) {
	response := httptest.NewRecorder()
	Handler(store.New(filepath.Join(t.TempDir(), "events.ndjson"))).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "No terminal events yet") {
		t.Fatalf("empty dashboard = %d: %s", response.Code, response.Body.String())
	}
}

func event(id string, outcome *record.Outcome) record.Record {
	return record.Record{SchemaVersion: record.SchemaVersion, EventID: record.DeriveEventID("claude-code", record.Identifier(id)), Timestamp: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC), Harness: "claude-code", SessionID: "session-1", Repo: "0123456789abcdef0123456789abcdef", Kind: record.KindSkill, Name: "review", Invoker: record.InvokerModel, Outcome: outcome}
}
