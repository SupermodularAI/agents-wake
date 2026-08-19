package style

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// frames is a classic braille spinner — the same glyph set most terminal
// tools reach for, so it needs no explanation the first time someone sees it.
var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner animates one status line for a step with no incremental progress to
// report. It is not a TUI (ADR-0011): there is no alternate screen and
// nothing to navigate, only one line that redraws in place with a carriage
// return and is gone the moment Stop is called — the step's own, plain,
// deterministic result is what a reader (or a script) is left looking at.
//
// A caller only ever constructs one when Pretty is true, so there is nothing
// here to gate on non-TTY: on a pipe, a redirect, or in a test writing to a
// bytes.Buffer, Wake never starts one at all.
type Spinner struct {
	writer io.Writer
	label  string
	tick   chan struct{}
	done   chan struct{}
}

// NewSpinner starts animating immediately in its own goroutine.
func NewSpinner(w io.Writer, label string) *Spinner {
	s := &Spinner{writer: w, label: label, tick: make(chan struct{}), done: make(chan struct{})}
	go s.run()
	return s
}

func (s *Spinner) run() {
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	frame := 0
	for {
		select {
		case <-ticker.C:
			fmt.Fprintf(s.writer, "\r%s%s%s %s", Lime, frames[frame%len(frames)], Reset, s.label)
			frame++
		case <-s.tick:
			close(s.done)
			return
		}
	}
}

// Stop clears the spinner's line and blocks until its goroutine has actually
// stopped drawing, so whatever the caller prints next can never land behind a
// frame that was already queued to overwrite it.
func (s *Spinner) Stop() {
	close(s.tick)
	<-s.done
	fmt.Fprintf(s.writer, "\r%s\r", strings.Repeat(" ", VisibleWidth(s.label)+2))
}

// WithSpinner runs fn, animating a spinner labeled label on w while it runs —
// or, when pretty is false, just runs fn with no drawing at all. Every
// command that wraps a step in a spinner does it through here rather than
// its own Start/Stop pair, so "the spinner is always stopped, success or
// error, before its result reaches the caller" is true in one place instead
// of at every call site.
func WithSpinner(w io.Writer, pretty bool, label string, fn func() error) error {
	if !pretty {
		return fn()
	}
	spinner := NewSpinner(w, label)
	err := fn()
	spinner.Stop()
	return err
}
