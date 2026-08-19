package report

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/SupermodularAI/agents-wake/internal/style"
)

func heading(pretty bool, text string) string { return style.Heading(pretty, text) }

// outcomePaint colors a reported outcome so a scan of the OUTCOMES table reads
// at a glance: ok is the only outcome that is not, in some way, a thing to look
// into (ADR-0005 — outcome is nullable and unknown is never success, so "ok" is
// the sole good case this can highlight).
func outcomePaint(pretty bool, name string) string {
	if name == "ok" {
		return style.Paint(pretty, style.Green, name)
	}
	return style.Paint(pretty, style.Red, name)
}

// table is a small buffered table: rows are collected before anything is
// written because the boxed form needs every column's width up front, unlike
// tabwriter's streaming alignment.
type table struct {
	headers []string
	rows    [][]string
}

func newTable(headers ...string) *table { return &table{headers: headers} }

func (t *table) add(cols ...string) { t.rows = append(t.rows, cols) }

// write renders the table plain (tabwriter, ASCII-only, unchanged from before
// Pretty existed) or, when pretty, as a lime-bordered box with a bold header
// row — never both in the same run, so a non-TTY consumer's bytes never
// depend on what a human's terminal can display.
func (t *table) write(w io.Writer, pretty bool) error {
	if !pretty {
		return t.writePlain(w)
	}
	return t.writeBoxed(w)
}

func (t *table) writePlain(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(t.headers, "\t")); err != nil {
		return err
	}
	for _, row := range t.rows {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func (t *table) writeBoxed(w io.Writer) error {
	widths := make([]int, len(t.headers))
	for i, header := range t.headers {
		widths[i] = style.VisibleWidth(header)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if n := style.VisibleWidth(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}

	border := func(left, mid, right string) error {
		segments := make([]string, len(widths))
		for i, width := range widths {
			segments[i] = strings.Repeat("─", width+2)
		}
		_, err := fmt.Fprintln(w, style.Lime+left+strings.Join(segments, mid)+right+style.Reset)
		return err
	}
	row := func(cols []string, bold bool) error {
		cells := make([]string, len(cols))
		for i, cell := range cols {
			padded := style.Pad(cell, widths[i])
			if bold {
				padded = style.Bold + padded + style.Reset
			}
			cells[i] = " " + padded + " "
		}
		_, err := fmt.Fprintln(w, style.Lime+"│"+style.Reset+strings.Join(cells, style.Lime+"│"+style.Reset)+style.Lime+"│"+style.Reset)
		return err
	}

	if err := border("┌", "┬", "┐"); err != nil {
		return err
	}
	if err := row(t.headers, true); err != nil {
		return err
	}
	if err := border("├", "┼", "┤"); err != nil {
		return err
	}
	for _, r := range t.rows {
		if err := row(r, false); err != nil {
			return err
		}
	}
	return border("└", "┴", "┘")
}
