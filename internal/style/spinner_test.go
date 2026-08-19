package style

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSpinnerAnimatesThenClearsItsLine is timing-based by necessity — a
// spinner's entire job is to redraw on a tick — so it asserts the two things
// that must be true regardless of how many frames land: at least one frame
// was drawn, and Stop leaves the line blank rather than mid-frame.
//
// Reading buf after Stop is race-free without its own lock: Stop only
// returns once the spinner's goroutine has observed the stop signal and
// exited (it blocks on <-s.done), so nothing but this goroutine touches buf
// by the time String() runs.
func TestSpinnerAnimatesThenClearsItsLine(t *testing.T) {
	var buf bytes.Buffer
	spinner := NewSpinner(&buf, "working")
	time.Sleep(200 * time.Millisecond)
	spinner.Stop()

	out := buf.String()
	if !strings.Contains(out, "working") {
		t.Fatalf("spinner never drew its label:\n%q", out)
	}
	drewAFrame := false
	for _, frame := range frames {
		if strings.Contains(out, frame) {
			drewAFrame = true
			break
		}
	}
	if !drewAFrame {
		t.Fatalf("spinner drew no recognizable frame:\n%q", out)
	}
	// The very last thing written must be the clearing sequence: a carriage
	// return, blanks covering the label, and a final carriage return — anything
	// else left behind is a frame the next print would land in the middle of.
	tail := strings.Repeat(" ", VisibleWidth("working")+2)
	if !strings.HasSuffix(out, "\r"+tail+"\r") {
		t.Fatalf("spinner did not clear its line on Stop; out = %q", out)
	}
}

func TestWithSpinnerRunsPlainWhenNotPretty(t *testing.T) {
	var buf bytes.Buffer
	called := false
	if err := WithSpinner(&buf, false, "working", func() error { called = true; return nil }); err != nil {
		t.Fatalf("WithSpinner() error = %v", err)
	}
	if !called {
		t.Fatal("WithSpinner(pretty=false) never ran fn")
	}
	if buf.Len() != 0 {
		t.Fatalf("WithSpinner(pretty=false) wrote %q, want nothing drawn", buf.String())
	}
}

func TestWithSpinnerAnimatesAndClearsThenReturnsFnsError(t *testing.T) {
	var buf bytes.Buffer
	sentinel := errors.New("boom")
	err := WithSpinner(&buf, true, "working", func() error {
		time.Sleep(200 * time.Millisecond)
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithSpinner() error = %v, want %v", err, sentinel)
	}
	out := buf.String()
	if !strings.Contains(out, "working") {
		t.Fatalf("WithSpinner(pretty=true) never drew its label:\n%q", out)
	}
	tail := strings.Repeat(" ", VisibleWidth("working")+2)
	if !strings.HasSuffix(out, "\r"+tail+"\r") {
		t.Fatalf("WithSpinner() did not clear its line before returning fn's error; out = %q", out)
	}
}
