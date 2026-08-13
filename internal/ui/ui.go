// Package ui renders and serves Wake's local dashboard from derived metrics.
package ui

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

//go:embed dashboard.html
var assets embed.FS

var page = template.Must(template.ParseFS(assets, "dashboard.html"))

// Handler serves a server-rendered dashboard. Every request reads only the
// local event store and aggregates records before rendering HTML.
func Handler(source *store.Store) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(writer, request)
			return
		}
		entries, err := source.Entries(0)
		if err != nil {
			http.Error(writer, "cannot read local Wake store", http.StatusInternalServerError)
			return
		}
		records := make([]record.Record, 0, len(entries))
		for _, entry := range entries {
			records = append(records, entry.Record)
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(writer, view(metrics.Aggregate(records))); err != nil {
			return
		}
	})
}

// ListenAndServe binds the dashboard to loopback only.
func ListenAndServe(port int, source *store.Store) error {
	return http.ListenAndServe("127.0.0.1:"+strconv.Itoa(port), Handler(source))
}

type dashboardView struct {
	Empty        bool
	Updated      string
	Invocations  string
	Sessions     string
	LastObserved string
	ErrorRate    string
	ErrorDetail  string
	Outcomes     []outcomeView
	Excluded     string
	Primitives   []primitiveView
}

type outcomeView struct{ Name, Count, Percent string }
type primitiveView struct{ Name, Kind, Harness, LastUsed, Invocations, Sessions, ErrorRate, ErrorDetail string }

func view(summary metrics.Summary) dashboardView {
	result := dashboardView{Empty: summary.Invocations == 0, Invocations: number(summary.Invocations), Sessions: number(summary.Sessions), ErrorRate: rate(summary.ErrorRate), ErrorDetail: ratioDetail(summary.ErrorRate), Excluded: number(summary.ErrorRate.Excluded())}
	if !summary.LastObserved.IsZero() {
		result.Updated = "Last observed " + summary.LastObserved.Local().Format("2006-01-02 15:04")
		result.LastObserved = summary.LastObserved.Local().Format("Jan 02")
	} else {
		result.Updated = "No activity observed yet"
		result.LastObserved = "-"
	}
	for _, outcome := range []record.Outcome{record.OutcomeOK, record.OutcomeError, record.OutcomeDeniedPolicy, record.OutcomeDeniedUser, record.OutcomeTimeout, record.OutcomeInterrupted, record.OutcomeNotFound, record.OutcomeBadArgs} {
		count := summary.Outcomes[outcome]
		if count == 0 {
			continue
		}
		percent := float64(count) * 100 / float64(summary.ErrorRate.Denominator())
		result.Outcomes = append(result.Outcomes, outcomeView{Name: strings.ReplaceAll(string(outcome), "_", " "), Count: number(count), Percent: fmt.Sprintf("%.1f", percent)})
	}
	for _, primitive := range summary.Primitives {
		result.Primitives = append(result.Primitives, primitiveView{Name: string(primitive.Name), Kind: strings.ReplaceAll(string(primitive.Kind), "_", " "), Harness: string(primitive.Harness), LastUsed: primitive.LastUsed.Local().Format("Jan 02 15:04"), Invocations: number(primitive.Invocations), Sessions: number(primitive.Sessions), ErrorRate: rate(primitive.ErrorRate), ErrorDetail: ratioDetail(primitive.ErrorRate)})
	}
	return result
}

func number(value uint64) string { return strconv.FormatUint(value, 10) }
func rate(ratio metrics.Ratio) string {
	if percent, ok := ratio.Percent(); ok {
		return fmt.Sprintf("%.1f%%", percent)
	}
	return "not available"
}
func ratioDetail(ratio metrics.Ratio) string {
	return number(ratio.Numerator()) + " / " + number(ratio.Denominator()) + " known; " + number(ratio.Excluded()) + " excluded"
}
