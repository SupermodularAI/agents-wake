// Package ui renders and serves Wake's local dashboard from derived metrics.
package ui

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

//go:embed dashboard.html
var assets embed.FS

var page = template.Must(template.ParseFS(assets, "dashboard.html"))

// Handler serves a server-rendered dashboard. Every request reads the event
// spool and the latest primitive snapshot, so hook-driven refreshes appear
// without restarting the dashboard.
func Handler(source *store.Store, primitives *inventory.Store) http.Handler {
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
		available, err := primitives.Read()
		if err != nil {
			http.Error(writer, "cannot read local Wake primitive inventory", http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(writer, view(metrics.Aggregate(records), available)); err != nil {
			return
		}
	})
}

// ListenAndServe binds the dashboard to loopback only.
func ListenAndServe(port int, source *store.Store, primitives *inventory.Store) error {
	return http.ListenAndServe("127.0.0.1:"+strconv.Itoa(port), Handler(source, primitives))
}

type dashboardView struct {
	Empty        bool
	Updated      string
	Invocations  string
	Sessions     string
	LastObserved string
	ErrorRate    string
	ErrorDetail  string
	Usage        []primitiveView
	Unused       []primitiveView
}

type primitiveView struct{ Name, Kind, Harness, LastUsed, Invocations string }

func view(summary metrics.Summary, available []inventory.Usage) dashboardView {
	result := dashboardView{Empty: summary.Invocations == 0 && len(available) == 0, Invocations: number(summary.Invocations), Sessions: number(summary.Sessions), ErrorRate: rate(summary.ErrorRate), ErrorDetail: ratioDetail(summary.ErrorRate)}
	if !summary.LastObserved.IsZero() {
		result.Updated = "Last observed " + summary.LastObserved.Local().Format("2006-01-02 15:04")
		result.LastObserved = summary.LastObserved.Local().Format("Jan 02")
	} else {
		result.Updated = "No activity observed yet"
		result.LastObserved = "-"
	}
	for _, primitive := range available {
		view := primitiveView{Name: string(primitive.Name), Kind: strings.ReplaceAll(string(primitive.Kind), "_", " "), Harness: string(primitive.Harness), Invocations: number(primitive.Invocations)}
		if primitive.Invocations == 0 {
			result.Unused = append(result.Unused, view)
			continue
		}
		view.LastUsed = primitive.LastUsed.Local().Format("Jan 02 15:04")
		result.Usage = append(result.Usage, view)
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
