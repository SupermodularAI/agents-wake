package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// echoPrompter answers from a fixed script and writes the question into the
// command's own output stream, so a test can assert by index that the disclosure
// was printed before the question was put — ordering fakeTerminal's separate
// transcript cannot show.
type echoPrompter struct {
	out    io.Writer
	answer string
}

func (e *echoPrompter) Line(prompt string) (string, error) {
	if _, err := fmt.Fprint(e.out, prompt); err != nil {
		return "", err
	}
	return e.answer, nil
}

// Secret is never reached: a yes/no confirmation asks for no secret, and a call
// here would mean the gate had grown a second question.
func (e *echoPrompter) Secret(string) (string, error) {
	return "", errors.New("a confirmation never asks for a secret")
}

// --yes is answered before there is anything to ask, so a run carrying it must
// never touch standard input at all — not even to decide whether it is a
// terminal. The factory fails the test if it is consulted.
func TestAssumeYesNeverConstructsAPrompter(t *testing.T) {
	factory := func(*cobra.Command) prompter {
		t.Fatal("--yes consulted the terminal; it is an answer, not a way of asking")
		return nil
	}

	gate, err := newConfirmer(&cobra.Command{}, factory, true)

	if err != nil {
		t.Fatalf("newConfirmer() error = %v", err)
	}
	proceed, err := gate.ask("Permanently delete all of this? [y/N]: ")
	if err != nil {
		t.Fatalf("ask() error = %v", err)
	}
	if !proceed {
		t.Error("ask() = false with --yes, want true")
	}
}

// The ADR-0043 §2 refusal: no terminal and no --yes deletes nothing, and the
// error has to name the one way through or the user is stuck.
func TestNoTerminalRefusesAndNamesYes(t *testing.T) {
	_, err := newConfirmer(&cobra.Command{}, func(*cobra.Command) prompter { return nil }, false)

	if !errors.Is(err, errNoTerminalToConfirmOn) {
		t.Fatalf("newConfirmer() error = %v, want errNoTerminalToConfirmOn", err)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("refusal = %q, want it to name --yes", err.Error())
	}
}

// A bare Return is a no, and so is anything that is not a yes. The prompt says
// [y/N] and the deletions behind it do not come back.
func TestOnlyYesProceeds(t *testing.T) {
	for _, c := range []struct {
		answer string
		want   bool
	}{
		{"y", true}, {"yes", true}, {"Y", true}, {"YES", true},
		{"n", false}, {"", false}, {"no", false}, {" ", false}, {"yep", false},
	} {
		t.Run(fmt.Sprintf("%q", c.answer), func(t *testing.T) {
			fake := &fakeTerminal{answers: []string{c.answer}}
			gate, err := newConfirmer(&cobra.Command{}, fake.factory(), false)
			if err != nil {
				t.Fatalf("newConfirmer() error = %v", err)
			}

			got, err := gate.ask("Permanently delete all of this? [y/N]: ")

			if err != nil {
				t.Fatalf("ask() error = %v", err)
			}
			if got != c.want {
				t.Errorf("ask(%q) = %t, want %t", c.answer, got, c.want)
			}
		})
	}
}

// A Ctrl-D at the question is a decline, not a fault: there is no value here for
// an error to carry, and `Error: EOF` on a command that deleted nothing reads as
// a failure rather than the abort it is.
func TestCtrlDAtTheQuestionIsADecline(t *testing.T) {
	fake := &fakeTerminal{}
	gate, err := newConfirmer(&cobra.Command{}, fake.factory(), false)
	if err != nil {
		t.Fatalf("newConfirmer() error = %v", err)
	}

	proceed, err := gate.ask("Permanently delete all of this? [y/N]: ")

	if err != nil {
		t.Fatalf("ask() error = %v, want a decline reported as (false, nil)", err)
	}
	if proceed {
		t.Error("ask() = true on an ended stream; a Ctrl-D is not a yes")
	}
}

// One question, echoed. A yes/no answer is not a credential, so nothing here
// turns terminal echo off (ADR-0028 names only the secret half as one).
func TestTheQuestionIsAskedWithEcho(t *testing.T) {
	fake := &fakeTerminal{answers: []string{"y"}}
	gate, err := newConfirmer(&cobra.Command{}, fake.factory(), false)
	if err != nil {
		t.Fatalf("newConfirmer() error = %v", err)
	}
	const question = "Permanently delete all of this? [y/N]: "

	if _, err := gate.ask(question); err != nil {
		t.Fatalf("ask() error = %v", err)
	}

	if len(fake.shown) != 1 || fake.shown[0] != question {
		t.Errorf("shown = %q, want exactly one entry %q", fake.shown, question)
	}
	if len(fake.masked) != 0 {
		t.Errorf("masked = %q, want none: a confirmation is not a secret", fake.masked)
	}
}
