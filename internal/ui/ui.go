// Package ui renders and serves Wake's local dashboard from derived metrics.
package ui

import (
	"embed"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// timeouts bounds each phase of a request. A dashboard request is one template
// render over the local store, so none of these needs to be generous: an
// unbounded phase is what lets a half-written request hold a descriptor forever.
type timeouts struct{ Header, Read, Write, Idle time.Duration }

func defaultTimeouts() timeouts {
	return timeouts{Header: 5 * time.Second, Read: 15 * time.Second, Write: 30 * time.Second, Idle: 60 * time.Second}
}

// Listen binds the dashboard to loopback only, and to nothing else: the address is
// fixed at 127.0.0.1 so no caller can widen it.
//
// Binding is separate from serving so the caller announces the URL only after the
// bind succeeded — an occupied port must not print an active-server message.
func Listen(port int) (net.Listener, error) {
	address := "127.0.0.1:" + strconv.Itoa(port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("cannot bind the dashboard to %s: %w", address, err)
	}
	return listener, nil
}

// Serve serves the dashboard over an already-bound listener, with every request
// phase bounded.
func Serve(listener net.Listener, source *store.Store, primitives *inventory.Store) error {
	return serve(listener, Handler(source, primitives), defaultTimeouts())
}

// serve exists so a test can bound the phases in milliseconds instead of seconds.
func serve(listener net.Listener, handler http.Handler, limits timeouts) error {
	return newServer(handler, limits).Serve(listener)
}

func newServer(handler http.Handler, limits timeouts) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: limits.Header,
		ReadTimeout:       limits.Read,
		WriteTimeout:      limits.Write,
		IdleTimeout:       limits.Idle,
	}
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

type primitiveView struct{ Name, Kind, Harness, LastUsed, Invocations, Errors string }

func view(summary metrics.Summary, available []inventory.Usage) dashboardView {
	result := dashboardView{Empty: !summary.Observed() && len(available) == 0, Invocations: number(summary.Invocations), Sessions: number(summary.Sessions), ErrorRate: rate(summary.ErrorRate), ErrorDetail: ratioDetail(summary.ErrorRate)}
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
		view.Errors = errorCell(primitive)
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

// errorCell mirrors internal/report's cell of the same name: a count first,
// then the percentage in parentheses once there is a failure to rate.
func errorCell(usage inventory.Usage) string {
	if usage.Failures == 0 {
		return "0"
	}
	ratio := metrics.NewRatio(usage.Failures, usage.Invocations-usage.Unknown, usage.Unknown, usage.Invocations)
	if percent, ok := ratio.Percent(); ok {
		return fmt.Sprintf("%d (%.1f%%)", usage.Failures, percent)
	}
	return number(usage.Failures)
}
