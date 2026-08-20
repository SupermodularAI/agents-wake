// Package style holds Wake's small set of terminal presentation primitives —
// ANSI color codes built around its lime brand accent — shared by every
// renderer that draws for a human's terminal. Every function here takes the
// "pretty" decision as a parameter rather than making it: that decision is
// isatty(stdout), and it belongs to internal/cli, the one layer that can hold
// an *os.File (ADR-0001, plan §6.2). A caller that never passes pretty=true
// gets exactly the plain bytes it always did, which is what keeps this
// package additive to the non-TTY contract rather than a change to it
// (ADR-0011, plan §7.3, §8).
package style

import "strings"

// Lime is Wake's brand color, #f6ff9a — the same value as the dashboard's
// dark-mode accent (internal/ui/dashboard.html: --accent). A terminal gives
// no way to detect a light background the way that dashboard's own media
// query can, so this reads best on a dark terminal theme; a light one is the
// tradeoff a fixed brand color makes.
const (
	Reset = "\x1b[0m"
	Bold  = "\x1b[1m"
	Dim   = "\x1b[2m"
	Lime  = "\x1b[38;2;246;255;154m"
	Green = "\x1b[38;2;43;141;23m"
	Red   = "\x1b[38;2;158;37;48m"
)

// Paint wraps text in an ANSI code, or returns it untouched when pretty is
// false. Every styled string in this codebase passes through here (or
// Heading), so "does non-TTY output ever carry an escape code" has one
// place to be true.
func Paint(pretty bool, code, text string) string {
	if !pretty || text == "" {
		return text
	}
	return code + text + Reset
}

// Heading is the recurring look for a section title: bold, lime.
func Heading(pretty bool, text string) string { return Paint(pretty, Bold+Lime, text) }

// VisibleWidth counts the runes a terminal actually draws, skipping any ANSI
// escape sequence. A colored cell is longer in bytes than what it draws, and
// column-alignment code that measured raw rune count would size a colored
// column wider than every plain one beside it.
func VisibleWidth(s string) int {
	width := 0
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\x1b' {
			for i < len(runes) && runes[i] != 'm' {
				i++
			}
			continue
		}
		width++
	}
	return width
}

// Pad right-pads text to width, measured visibly rather than by byte length,
// so a colored cell still lines up with its plain neighbors.
func Pad(text string, width int) string {
	if gap := width - VisibleWidth(text); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}
