package cli

import (
	"bytes"
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

// streamPrompter wires echoPrompter the way osPrompter wires the real terminal:
// the question goes to promptStream(cmd), not to a buffer the test picked. It is
// what lets a test with stdout and stderr kept apart assert which of the two the
// disclosure and the question actually landed on — every other helper in this
// package points both at one buffer, which is exactly what hid the split.
func streamPrompter(answer string) promptFactory {
	return func(cmd *cobra.Command) prompter {
		return &echoPrompter{out: promptStream(cmd), answer: answer}
	}
}

// The disclosure has to go where the question goes, or a redirection separates
// the paths from the question they authorise (ADR-0043 §1). With --yes there is
// no question and the disclosure is a record of what happened, which belongs on
// the answer stream.
func TestDiscloseToFollowsTheQuestion(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	asking, err := newConfirmer(cmd, (&fakeTerminal{answers: []string{"y"}}).factory(), false)
	if err != nil {
		t.Fatalf("newConfirmer() error = %v", err)
	}
	if got := asking.discloseTo(cmd); got != promptStream(cmd) {
		t.Errorf("discloseTo() = %p, want the stream the question is put on (%p)", got, promptStream(cmd))
	}
	if asking.discloseTo(cmd) == cmd.OutOrStdout() {
		t.Error("discloseTo() is stdout while the question is not; `wake uninstall > log` would ask about paths the user cannot see")
	}

	answered, err := newConfirmer(cmd, func(*cobra.Command) prompter { return nil }, true)
	if err != nil {
		t.Fatalf("newConfirmer(--yes) error = %v", err)
	}
	if got := answered.discloseTo(cmd); got != cmd.OutOrStdout() {
		t.Errorf("discloseTo() with --yes = %p, want stdout (%p)", got, cmd.OutOrStdout())
	}
}

// A gate nobody constructed has no authority to proceed on. ADR-0043
// § Consequences hands this decision to whatever destructive command comes
// third, and a safety gate whose zero value means yes is one a later author
// disarms by forgetting a line — which compiles, lints, and deletes.
func TestTheUnconstructedGateDeclines(t *testing.T) {
	var gate confirmer

	proceed, err := gate.ask("Permanently delete all of this? [y/N]: ")

	if err != nil {
		t.Fatalf("ask() error = %v", err)
	}
	if proceed {
		t.Error("ask() = true on a confirmer nobody constructed; the zero value must refuse, not delete")
	}
}
