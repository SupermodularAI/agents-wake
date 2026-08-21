// Package report renders Wake's derived local metrics for a terminal.
package report

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/inventory"
	"github.com/SupermodularAI/agents-wake/internal/metrics"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/store"
	"github.com/SupermodularAI/agents-wake/internal/style"
)

// Options selects a report's primitive sections and presentation. With
// neither Usage nor Unused set, only usage is shown: the primitives you've
// used is what a repeat `wake report` is for, and the unused list is a
// deliberate ask (--unused), not a default a busy screen buries it in.
//
// Pretty is internal/cli's call, not this package's: it knows whether stdout
// is a terminal, this package only knows how to draw one either way.
type Options struct {
	Usage  bool
	Unused bool
	Pretty bool
}

// Print reads the local event and primitive stores and writes current metrics.
func Print(writer io.Writer, source *store.Store, primitives *inventory.Store, options Options) error {
	entries, err := source.Entries(0)
	if err != nil {
		return err
	}
	records := make([]record.Record, 0, len(entries))
	for _, entry := range entries {
		records = append(records, entry.Record)
	}
	available, err := primitives.Read()
	if err != nil {
		return err
	}
	return Render(writer, metrics.Aggregate(records), available, options)
}

// Render writes one readable report. Its content is identical whatever the
// destination; Options.Pretty only changes how that same content is drawn
// (ADR-0011's non-TTY contract governs the former, not the latter).
func Render(writer io.Writer, summary metrics.Summary, available []inventory.Usage, options Options) error {
	pretty := options.Pretty
	if _, err := fmt.Fprintln(writer, heading(pretty, "WAKE REPORT")); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, style.Paint(pretty, style.Dim, "Local-only activity from Wake's event store")); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, style.Paint(pretty, style.Lime, strings.Repeat("=", 72))); err != nil {
		return err
	}
	if summary.Invocations == 0 && len(available) == 0 {
		_, err := fmt.Fprintln(writer, "No terminal events yet. Run `wake ingest` after using a consented harness.")
		return err
	}
	if err := lastObserved(writer, summary); err != nil {
		return err
	}

	// A bare `--unused` asks what was never invoked, not how much invocation
	// happened — so it gets a count of unused primitives by kind instead of the
	// usage view's tables.
	options = normalized(options)
	if !options.Usage {
		if err := unusedOverview(writer, available, pretty); err != nil {
			return err
		}
	}
	if options.Usage {
		if err := primitiveUsage(writer, available, pretty); err != nil {
			return err
		}
	}
	if options.Unused {
		if err := unusedPrimitives(writer, available, pretty); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(writer, "All figures are derived metadata; raw prompts, arguments, paths, and repository labels are never shown.")
	return err
}

// lastObserved is a one-line stand-in for what used to be a whole OVERVIEW
// section (invocation counts, error rate, outcomes breakdown): those numbers
// spanned every primitive combined, which never reconciled with any single
// row of USED PRIMITIVES below them. The timestamp has no such mismatch — it
// is worth keeping on its own.
func lastObserved(writer io.Writer, summary metrics.Summary) error {
	observed := "not observed"
	if !summary.LastObserved.IsZero() {
		observed = summary.LastObserved.UTC().Format(time.RFC3339)
	}
	_, err := fmt.Fprintf(writer, "Last observed: %s\n", observed)
	return err
}

// unusedOverview stands in for a bare `--unused` report's overview:
// a count of unused primitives by kind, so the one figure the ask was for is
// the first thing on screen rather than something to find inside the table
// below. It prints nothing when every discovered primitive has activity —
// unusedPrimitives' own "Every discovered primitive has activity." already
// says that, and a second copy of the same sentence up here would not add
// anything.
func unusedOverview(writer io.Writer, available []inventory.Usage, pretty bool) error {
	counts := map[record.Kind]int{}
	total := 0
	for _, usage := range available {
		if usage.Invocations > 0 {
			continue
		}
		counts[usage.Kind]++
		total++
	}
	if total == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(writer, "\n"+heading(pretty, "OVERVIEW")); err != nil {
		return err
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, string(k))
	}
	slices.Sort(kinds)
	table := tabwriter.NewWriter(writer, 0, 0, 2, ' ', 0)
	for _, k := range kinds {
		if _, err := fmt.Fprintf(table, "%s\t%d\n", capitalize(kind(record.Kind(k))), counts[record.Kind(k)]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(table, "Total unused\t%d\n", total); err != nil {
		return err
	}
	return table.Flush()
}

func primitiveUsage(writer io.Writer, available []inventory.Usage, pretty bool) error {
	if _, err := fmt.Fprintln(writer, "\n"+heading(pretty, "USED PRIMITIVES")); err != nil {
		return err
	}
	rows := newTable("PRIMITIVE", "TYPE", "HARNESS", "LAST USED", "CALLS", "ERRORS")
	for _, usage := range available {
		if usage.Invocations == 0 {
			continue
		}
		rows.add(string(usage.Name), kind(usage.Kind), string(usage.Harness), usage.LastUsed.UTC().Format(time.RFC3339), fmt.Sprintf("%d", usage.Invocations), errorCell(usage))
	}
	if len(rows.rows) == 0 {
		_, err := fmt.Fprintln(writer, "No primitive activity observed.")
		return err
	}
	if err := rows.write(writer, pretty); err != nil {
		return err
	}
	_, err := fmt.Fprintln(writer, "Only currently discovered, non-built-in primitives are listed.")
	return err
}

func unusedPrimitives(writer io.Writer, available []inventory.Usage, pretty bool) error {
	if _, err := fmt.Fprintln(writer, "\n"+heading(pretty, "UNUSED PRIMITIVES")); err != nil {
		return err
	}
	rows := newTable("PRIMITIVE", "TYPE", "HARNESS")
	for _, usage := range available {
		if usage.Invocations > 0 {
			continue
		}
		rows.add(string(usage.Name), kind(usage.Kind), string(usage.Harness))
	}
	if len(rows.rows) == 0 {
		_, err := fmt.Fprintln(writer, "Every discovered primitive has activity.")
		return err
	}
	return rows.write(writer, pretty)
}

// normalized fills in the default section selection without disturbing any
// other option (Pretty included) — see Options' doc comment for what the
// default is and why.
func normalized(options Options) Options {
	if !options.Usage && !options.Unused {
		options.Usage = true
	}
	return options
}

func kind(value record.Kind) string { return strings.ReplaceAll(string(value), "_", " ") }

// capitalize upper-cases a kind label's first letter to match the sentence
// case of OVERVIEW's other rows ("Terminal invocations", not "terminal
// invocations"). Every record.Kind is plain ASCII, so a byte slice is safe.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// errorCell is a single per-primitive table cell, not the OVERVIEW-wide rate:
// a count first because that is what a busy reader scans a column for, then
// the percentage in parentheses for the ones who want the rate too. A
// primitive with no failures says "0" rather than "0 (0.0%)" — a rate is only
// interesting once there is one.
func errorCell(usage inventory.Usage) string {
	if usage.Failures == 0 {
		return "0"
	}
	ratio := metrics.NewRatio(usage.Failures, usage.Invocations-usage.Unknown, usage.Unknown, usage.Invocations)
	if percent, ok := ratio.Percent(); ok {
		return fmt.Sprintf("%d (%.1f%%)", usage.Failures, percent)
	}
	return fmt.Sprintf("%d", usage.Failures)
}
