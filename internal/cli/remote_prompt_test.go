package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// fakeTerminal is the injected terminal every interactive test runs against, so
// nothing here needs a real TTY.
//
// It answers from a script and records two things: every prompt string it was
// shown, and whether each was asked with echo or masked. The first is what lets
// a test assert that the confirmation named the host and nothing else; the
// second is what lets it assert that the secret key was asked for with echo off.
// An exhausted script returns io.EOF, which is what a Ctrl-D does, so no test
// can hang in the wizard's re-prompt loop.
type fakeTerminal struct {
	answers []string
	shown   []string
	masked  []string
	echoed  []string
}

func (f *fakeTerminal) next() (string, error) {
	if len(f.answers) == 0 {
		return "", io.EOF
	}
	answer := f.answers[0]
	f.answers = f.answers[1:]
	return answer, nil
}

func (f *fakeTerminal) Line(prompt string) (string, error) {
	f.shown = append(f.shown, prompt)
	answer, err := f.next()
	if err != nil {
		return "", err
	}
	f.echoed = append(f.echoed, answer)
	return answer, nil
}

func (f *fakeTerminal) Secret(prompt string) (string, error) {
	f.shown = append(f.shown, prompt)
	answer, err := f.next()
	if err != nil {
		return "", err
	}
	f.masked = append(f.masked, answer)
	return answer, nil
}

// transcript is everything the terminal displayed. Assertions about what a
// prompt may not carry run over this, because a prompt is output even though it
// is not on stdout.
func (f *fakeTerminal) transcript() string { return strings.Join(f.shown, "\n") }

func (f *fakeTerminal) factory() promptFactory {
	return func(*cobra.Command) prompter { return f }
}

// Everything that is not a terminal takes the unchanged scripted path, and the
// nil that says so has to be an untyped one: a typed nil *termPrompter would
// satisfy `prompt != nil` and send a piped invocation into a prompt loop reading
// from a stream that has already ended.
func TestOsPrompterIsNilWithoutATerminal(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	t.Cleanup(func() {
		_ = read.Close()
		_ = write.Close()
	})

	cases := map[string]io.Reader{
		"a strings.Reader": strings.NewReader("x"),
		"a bytes.Buffer":   &bytes.Buffer{},
		"a real pipe":      read,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetIn(in)

			got := osPrompter(cmd)
			if got != nil {
				t.Errorf("osPrompter() = %#v, want nil", got)
			}
			if rendered := fmt.Sprintf("%v", got); rendered != "<nil>" {
				t.Errorf("osPrompter() renders as %s, want <nil>: a typed nil takes a human's branch", rendered)
			}
		})
	}
}

// The positive half of the branch, without a TTY: /dev/null is a character
// device on both platforms ADR-0021 supports, which is the exact property
// root.go's isTerminal tests. Asserting through it is what pins the seam to that
// check rather than to term.IsTerminal.
func TestOsPrompterUsesTheCharacterDeviceCheck(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("os.Open(%q) = %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })

	cmd := &cobra.Command{}
	cmd.SetIn(devNull)

	if osPrompter(cmd) == nil {
		t.Error("osPrompter() = nil for a character device, want a prompter")
	}
}
