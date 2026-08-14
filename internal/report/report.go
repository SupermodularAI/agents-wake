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
)

// Options selects primitive sections in a report. With neither field set, both
// sections are shown.
type Options struct {
	Usage  bool
	Unused bool
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

// Render writes one readable, deterministic report. It intentionally uses only
// ASCII so the layout remains useful in terminals without Unicode support.
func Render(writer io.Writer, summary metrics.Summary, available []inventory.Usage, options Options) error {
	if _, err := fmt.Fprintln(writer, "WAKE REPORT"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, "Local-only activity from Wake's event store"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, strings.Repeat("=", 72)); err != nil {
		return err
	}
	if summary.Invocations == 0 && len(available) == 0 {
		_, err := fmt.Fprintln(writer, "No terminal events yet. Run `wake ingest` after using a consented harness.")
		return err
	}

	if err := overview(writer, summary); err != nil {
		return err
	}
	if err := outcomes(writer, summary); err != nil {
		return err
	}
	options = normalized(options)
	if options.Usage {
		if err := primitiveUsage(writer, available); err != nil {
			return err
		}
	}
	if options.Unused {
		if err := unusedPrimitives(writer, available); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(writer, "All figures are derived metadata; raw prompts, arguments, paths, and repository labels are never shown.")
	return err
}

func overview(writer io.Writer, summary metrics.Summary) error {
	if _, err := fmt.Fprintln(writer, "\nOVERVIEW"); err != nil {
		return err
	}
	lastObserved := "not observed"
	if !summary.LastObserved.IsZero() {
		lastObserved = summary.LastObserved.UTC().Format(time.RFC3339)
	}
	table := tabwriter.NewWriter(writer, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(table, "Terminal invocations\t%d\nDistinct sessions\t%d\nError rate\t%s\nLast observed\t%s\n", summary.Invocations, summary.Sessions, rate(summary.ErrorRate), lastObserved); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(table, "Health coverage\t%d known; %d unknown excluded\n", summary.ErrorRate.Denominator(), summary.ErrorRate.Excluded()); err != nil {
		return err
	}
	return table.Flush()
}

func outcomes(writer io.Writer, summary metrics.Summary) error {
	if _, err := fmt.Fprintln(writer, "\nOUTCOMES"); err != nil {
		return err
	}
	outcomeNames := make([]string, 0, len(summary.Outcomes))
	for outcome := range summary.Outcomes {
		outcomeNames = append(outcomeNames, string(outcome))
	}
	slices.Sort(outcomeNames)
	table := tabwriter.NewWriter(writer, 0, 0, 2, ' ', 0)
	if len(outcomeNames) == 0 && summary.ErrorRate.Excluded() == 0 {
		if _, err := fmt.Fprintln(table, "No outcomes reported."); err != nil {
			return err
		}
	}
	for _, name := range outcomeNames {
		if _, err := fmt.Fprintf(table, "%s\t%d\n", name, summary.Outcomes[record.Outcome(name)]); err != nil {
			return err
		}
	}
	if summary.ErrorRate.Excluded() > 0 {
		if _, err := fmt.Fprintf(table, "unknown\t%d (excluded from health rates)\n", summary.ErrorRate.Excluded()); err != nil {
			return err
		}
	}
	return table.Flush()
}

func primitiveUsage(writer io.Writer, available []inventory.Usage) error {
	if _, err := fmt.Fprintln(writer, "\nUSED PRIMITIVES"); err != nil {
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "PRIMITIVE\tTYPE\tHARNESS\tLAST USED\tCALLS"); err != nil {
		return err
	}
	var displayed uint64
	for _, usage := range available {
		if usage.Invocations == 0 {
			continue
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\n", usage.Name, kind(usage.Kind), usage.Harness, usage.LastUsed.UTC().Format(time.RFC3339), usage.Invocations); err != nil {
			return err
		}
		displayed++
	}
	if displayed == 0 {
		if _, err := fmt.Fprintln(table, "No primitive activity observed."); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(writer, "Only currently discovered, non-built-in primitives are listed.")
	return err
}

func unusedPrimitives(writer io.Writer, available []inventory.Usage) error {
	if _, err := fmt.Fprintln(writer, "\nUNUSED PRIMITIVES"); err != nil {
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "PRIMITIVE\tTYPE\tHARNESS"); err != nil {
		return err
	}
	var displayed uint64
	for _, usage := range available {
		if usage.Invocations > 0 {
			continue
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\n", usage.Name, kind(usage.Kind), usage.Harness); err != nil {
			return err
		}
		displayed++
	}
	if displayed == 0 {
		if _, err := fmt.Fprintln(table, "Every discovered primitive has activity."); err != nil {
			return err
		}
	}
	return table.Flush()
}

func normalized(options Options) Options {
	if !options.Usage && !options.Unused {
		return Options{Usage: true, Unused: true}
	}
	return options
}

func kind(value record.Kind) string { return strings.ReplaceAll(string(value), "_", " ") }

func rate(ratio metrics.Ratio) string {
	if percent, ok := ratio.Percent(); ok {
		return fmt.Sprintf("%.1f%% (%d/%d known; %d unknown)", percent, ratio.Numerator(), ratio.Denominator(), ratio.Excluded())
	}
	return fmt.Sprintf("not available (0 known; %d unknown)", ratio.Excluded())
}
