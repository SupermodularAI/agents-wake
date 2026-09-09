// The confirmation gate the two destructive commands share (ADR-0043 §1, §2).
//
// It reuses remote_prompt.go's prompter seam whole — the same interface, the
// same osPrompter, the same isAffirmative — rather than defining a second way to
// ask a question. That is also what keeps interactivity decided on standard
// input by term.IsTerminal's real ioctl, never on stdout and never through an
// os.ModeCharDevice proxy: /dev/null is a character device, and `wake uninstall
// < /dev/null` is exactly the unattended shape §2 refuses rather than one it
// should mistake for a person at a keyboard.
//
// The gate is two calls rather than one because the order matters. Whether the
// run can be authorised at all is decided *before* the disclosure, so a run that
// is going to refuse never first promises a deletion it will not make — the same
// ordering PlanUninstall already uses for the refusals it can pre-check. The
// question is put *after* the disclosure, because a confirmation the user cannot
// read the paths for is not one (ADR-0043 §1).
package cli

import (
	"errors"
	"io"

	"github.com/spf13/cobra"
)

// errNoTerminalToConfirmOn is what a scripted destructive run is refused with.
//
// Lowercase and unpunctuated for revive's error-strings rule, and it names --yes
// because that is the only way through (ADR-0043 §2). A script running either
// command today succeeds and starts failing here until --yes is added, which is
// the accepted cost of never deleting what nobody confirmed.
var errNoTerminalToConfirmOn = errors.New(
	"standard input is not a terminal, so this deletion cannot be confirmed; nothing was deleted — re-run with --yes to delete without being asked")

// nothingDeleted is what an aborted run says, on stdout, so a person who
// answered no is told plainly that the command changed nothing (ADR-0043 §1).
const nothingDeleted = "Nothing was deleted. Wake is unchanged."

// confirmer carries the decision made before the disclosure: a terminal to ask
// on, or an answer already settled by --yes.
//
// authorized is what makes the zero value refuse rather than proceed. Nothing
// but newConfirmer sets it, so a third destructive command that declares a
// `var gate confirmer` and forgets to construct it deletes nothing instead of
// deleting silently — ADR-0043 § Consequences says a third command inherits
// this decision, and a safety gate whose zero value is "yes" is one a later
// author can disarm by omission and still compile, lint and pass review.
type confirmer struct {
	prompt     prompter
	authorized bool
}

// newConfirmer decides whether this run can be authorised at all, before
// anything has been disclosed.
//
// --yes is an answer rather than a way of asking, so it never constructs a
// prompter and never touches standard input. Without it, a run with no terminal
// to ask on is refused here — nothing has been printed, and nothing is deleted.
func newConfirmer(cmd *cobra.Command, newPrompter promptFactory, assumeYes bool) (confirmer, error) {
	if assumeYes {
		return confirmer{authorized: true}, nil
	}
	prompt := newPrompter(cmd)
	if prompt == nil {
		return confirmer{}, errNoTerminalToConfirmOn
	}
	return confirmer{prompt: prompt}, nil
}

// discloseTo is the stream this run's disclosure has to be written to.
//
// When there is going to be a question, it is the stream the question is put on:
// the paths are read in order to answer, so a redirection that separates them
// from the question leaves the user authorising a deletion they cannot see
// (ADR-0043 §1). When --yes settled it, nobody is going to read anything at a
// prompt and the disclosure is a record of what the command did, which belongs
// in the answer stream every other command keeps on stdout.
func (c confirmer) discloseTo(cmd *cobra.Command) io.Writer {
	if c.prompt == nil {
		return cmd.OutOrStdout()
	}
	return promptStream(cmd)
}

// ask puts the question and reports whether to proceed. A --yes gate has nothing
// to ask and proceeds; an unconstructed one has no authority to proceed on and
// declines.
func (c confirmer) ask(question string) (bool, error) {
	if c.prompt == nil {
		return c.authorized, nil
	}
	answer, err := c.prompt.Line(question)
	if errors.Is(err, io.EOF) {
		// A Ctrl-D at the question is not a yes. It is treated as the decline it
		// is rather than surfaced as `Error: EOF`, because there is no value here
		// for an error to carry — unlike remote_prompt.go's wizard, where the EOF
		// ends a multi-value sequence part way through.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return isAffirmative(answer), nil
}
