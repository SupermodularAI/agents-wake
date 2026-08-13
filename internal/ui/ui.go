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

// Handler serves a server-rendered dashboard. Every request reads only the
// local event store and aggregates records before rendering HTML.
func Handler(source *store.Store, available []inventory.Primitive) http.Handler {
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
		if err := page.Execute(writer, view(metrics.Aggregate(records), available)); err != nil {
			return
		}
	})
}

// ListenAndServe binds the dashboard to loopback only.
func ListenAndServe(port int, source *store.Store, available []inventory.Primitive) error {
	return http.ListenAndServe("127.0.0.1:"+strconv.Itoa(port), Handler(source, available))
}

type dashboardView struct {
	Empty        bool
	Updated      string
	Invocations  string
	Sessions     string
	LastObserved string
	ErrorRate    string
	ErrorDetail  string
	Primitives   []primitiveView
}

type primitiveView struct{ Name, Kind, Harness, Invoker, Agent, Availability, LastUsed, Invocations, Sessions, ErrorRate, ErrorDetail string }

func view(summary metrics.Summary, available []inventory.Primitive) dashboardView {
	result := dashboardView{Empty: summary.Invocations == 0 && len(available) == 0, Invocations: number(summary.Invocations), Sessions: number(summary.Sessions), ErrorRate: rate(summary.ErrorRate), ErrorDetail: ratioDetail(summary.ErrorRate)}
	if !summary.LastObserved.IsZero() {
		result.Updated = "Last observed " + summary.LastObserved.Local().Format("2006-01-02 15:04")
		result.LastObserved = summary.LastObserved.Local().Format("Jan 02")
	} else {
		result.Updated = "No activity observed yet"
		result.LastObserved = "-"
	}
	observed := make(map[primitiveKey]struct{})
	for _, primitive := range summary.Primitives {
		if primitive.Kind == record.KindBuiltinTool {
			continue
		}
		observed[primitiveKey{kind: primitive.Kind, name: primitive.Name, harness: primitive.Harness}] = struct{}{}
		agent := string(primitive.ViaAgent)
		if agent == "" {
			agent = "-"
		}
		result.Primitives = append(result.Primitives, primitiveView{Name: string(primitive.Name), Kind: strings.ReplaceAll(string(primitive.Kind), "_", " "), Harness: string(primitive.Harness), Invoker: string(primitive.Invoker), Agent: agent, Availability: "observed", LastUsed: primitive.LastUsed.Local().Format("Jan 02 15:04"), Invocations: number(primitive.Invocations), Sessions: number(primitive.Sessions), ErrorRate: rate(primitive.ErrorRate), ErrorDetail: ratioDetail(primitive.ErrorRate)})
	}
	for _, primitive := range available {
		key := primitiveKey{kind: primitive.Kind, name: primitive.Name, harness: primitive.Harness}
		if _, found := observed[key]; found {
			continue
		}
		result.Primitives = append(result.Primitives, primitiveView{Name: string(primitive.Name), Kind: strings.ReplaceAll(string(primitive.Kind), "_", " "), Harness: string(primitive.Harness), Invoker: "-", Agent: "-", Availability: "not observed", LastUsed: "-", Invocations: "0", Sessions: "0", ErrorRate: "not observed", ErrorDetail: "available; no invocation found"})
	}
	return result
}

type primitiveKey struct {
	kind    record.Kind
	name    record.Identifier
	harness record.Identifier
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
