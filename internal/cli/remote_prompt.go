// The terminal half of `remote set`, and the seam that makes it testable.
//
// It exists because a credential typed at a prompt must not be echoed and Go's
// standard library cannot turn terminal echo off: golang.org/x/term is the
// mechanism ADR-0031 sanctions for that, in preference to shelling out to `stty`
// or hand-rolling a per-platform ioctl.
//
// Everything here is line prompts on the fd internal/cli already reads from —
// no alternate screen, no cursor addressing, nothing that would make this a TUI
// (ADR-0011). It parses and prints only: the value it produces goes straight to
// config.SetRemoteEndpoint, exactly as the piped path's does (ADR-0001).
//
// It is reached only when standard input is a real terminal. Everything else — a
// pipe, a redirected file, CI, and every test that sets a strings.Reader — takes
// the unchanged path in remote.go and never constructs a prompter at all.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// prompter asks the person at the terminal for one value at a time.
//
// Two methods because the two halves of a credential are not the same kind of
// value: ADR-0028 §Context names only the secret half as the credential, so the
// public key is echoed like any other input and the secret key is not echoed at
// all. Nothing here ever writes back what it read (ADR-0029).
type prompter interface {
	// Line asks for a value the terminal may echo as it is typed.
	Line(prompt string) (string, error)
	// Secret asks for a value the terminal must not echo.
	Secret(prompt string) (string, error)
}

// promptFactory decides, per invocation, whether there is a person to prompt.
//
// A nil return is the non-interactive answer, and it is the whole of the
// branch: `remote set` reaches the wizard only when this yields something. It is
// a parameter of newRemoteSetCmd rather than a package-level hook so a test can
// supply a fake terminal without mutating state another test can see.
type promptFactory func(*cobra.Command) prompter

// osPrompter returns a prompter when standard input is a real terminal, and nil
// when it is not.
//
// The check is the one internal/cli/root.go already applies to stdout — a
// character device, per os.ModeCharDevice — so both ends of this package answer
// "is a human here" the same way (ADR-0031 §1). A bytes.Buffer, a strings.Reader
// and a pipe are all not *os.File or not a character device, which is what keeps
// the piped path byte-for-byte what it was.
//
// The nil is returned as an untyped nil so the interface value is nil too; a
// typed nil *termPrompter would pass a `!= nil` check and take a human's branch
// in a script.
func osPrompter(cmd *cobra.Command) prompter {
	file, ok := cmd.InOrStdin().(*os.File)
	if !ok || !isTerminal(file) {
		return nil
	}
	return &termPrompter{in: file, out: cmd.ErrOrStderr(), lines: bufio.NewReader(file)}
}

// termPrompter is the real terminal.
//
// Prompts go to stderr, not stdout. stdout is the answer stream every command in
// this package keeps redirect-safe, and a prompt written into
// `wake remote set > file` is a prompt the person staring at the terminal never
// sees.
//
// The buffered reader is safe beside term.ReadPassword's direct read of the same
// fd only because this type is constructed for a terminal and nothing else: a
// terminal in canonical mode delivers one line per read, so the buffer never
// holds the line ReadPassword is about to want.
type termPrompter struct {
	in    *os.File
	out   io.Writer
	lines *bufio.Reader
}

func (t *termPrompter) Line(prompt string) (string, error) {
	if _, err := fmt.Fprint(t.out, prompt); err != nil {
		return "", err
	}
	line, err := t.lines.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		// A Ctrl-D on an empty line ends the wizard. The error is returned bare:
		// io.EOF carries no value, and wrapping it with anything typed here
		// would be the one place a prompt could carry input back out (ADR-0028).
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (t *termPrompter) Secret(prompt string) (string, error) {
	if _, err := fmt.Fprint(t.out, prompt); err != nil {
		return "", err
	}
	raw, err := term.ReadPassword(int(t.in.Fd()))
	// The newline the user's Return would have echoed, echoed here instead,
	// because ReadPassword suppressed it — without it the next prompt lands on
	// the same line. Printed before the error is judged, so a failed read leaves
	// the cursor somewhere sane too.
	if _, printErr := fmt.Fprintln(t.out); printErr != nil {
		return "", printErr
	}
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
