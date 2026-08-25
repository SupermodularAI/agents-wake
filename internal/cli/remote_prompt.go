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

	"github.com/SupermodularAI/agents-wake/internal/config"
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
// The check is term.IsTerminal, which performs the ioctl a terminal answers and
// a redirected file does not — deliberately *not* root.go's os.ModeCharDevice
// check on stdout (ADR-0031 §1 Correction). The two fds ask different questions:
// on stdout a wrong answer costs a colour code, while on stdin it decides
// whether the command runs at all, and /dev/null is a character device. Reading
// `wake remote set <url> < /dev/null` — what systemd, cron, nohup and a CI
// `run:` step with nothing to pipe supply — as a human at a keyboard would fail
// it at the first prompt, on an invocation ADR-0031 promises is untouched.
//
// A bytes.Buffer, a strings.Reader, a pipe, a redirected file and /dev/null are
// all either not *os.File or not a terminal, which is what keeps the piped path
// byte-for-byte what it was. It is also what keeps termPrompter's bufio.Reader
// off a stream with no newline in it: /dev/zero is not a terminal either, so the
// wizard is never constructed over one.
//
// The nil is returned as an untyped nil so the interface value is nil too; a
// typed nil *termPrompter would pass a `!= nil` check and take a human's branch
// in a script.
func osPrompter(cmd *cobra.Command) prompter {
	file, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
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

// notAnHTTPEndpoint is what a rejected URL is told, and it is a fixed literal:
// the value that was typed is never quoted back, because a URL is exactly where
// a credential hides (ADR-0029, ADR-0031).
const notAnHTTPEndpoint = "that is not an absolute http:// or https:// URL; try again."

// promptEndpointAndCredential walks a person through `remote set`, in the order
// the two answers are needed: where records go, confirmed, and then what
// authorises the delivery.
//
// The endpoint is settled before either key is asked for, so a mistyped host
// costs a re-typed URL rather than a re-typed secret. given is the URL from
// argv, or "" when none was passed.
func promptEndpointAndCredential(prompt prompter, w io.Writer, given string) (endpoint, credential string, err error) {
	endpoint, err = confirmEndpoint(prompt, w, given)
	if err != nil {
		return "", "", err
	}
	credential, err = promptCredential(prompt)
	if err != nil {
		return "", "", err
	}
	return endpoint, credential, nil
}

// confirmEndpoint asks until the person says yes to a host, and returns the
// whole URL behind it.
//
// It shows config.EndpointHost and never the URL — not the scheme, not the path,
// not the query, and above all not the userinfo, which is a credential outright
// (ADR-0031 §1, which rejects reading the issue's "confirm it" as an echo of the
// full URL). The confirmation happens whether the URL was typed here or passed
// as an argument: both were typed by the same person at the same terminal, and a
// mistyped host is as easy to introduce either way.
//
// A decline clears the endpoint and asks again rather than aborting, so a
// credential typed later is never thrown away by a typo caught here. The loop is
// bounded by the reader: a Ctrl-D returns io.EOF and the command exits having
// written nothing.
//
// An empty host is the refusal that keeps this from confirming a destination the
// store would reject: config.EndpointHost yields one only for an absolute
// http:// or https:// URL.
func confirmEndpoint(prompt prompter, w io.Writer, given string) (string, error) {
	endpoint := strings.TrimSpace(given)
	for {
		if endpoint == "" {
			typed, err := prompt.Line("Endpoint URL: ")
			if err != nil {
				return "", err
			}
			endpoint = strings.TrimSpace(typed)
		}
		host := config.EndpointHost(endpoint)
		if host == "" {
			if _, err := fmt.Fprintln(w, notAnHTTPEndpoint); err != nil {
				return "", err
			}
			endpoint = ""
			continue
		}
		answer, err := prompt.Line(fmt.Sprintf("Deliver records to %s? [y/N]: ", host))
		if err != nil {
			return "", err
		}
		if isAffirmative(answer) {
			return endpoint, nil
		}
		endpoint = ""
	}
}

// isAffirmative reads a yes and treats everything else — including a bare
// Return — as a no. The prompt says [y/N], and the safe default for "start
// sending records somewhere" is not to.
func isAffirmative(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

// promptCredential asks for the two halves and joins them the way the far end
// expects, public:secret.
//
// The public key is echoed and the secret key is not: ADR-0028 §Context names
// only the secret half as the credential. Neither the secret nor the joined
// string is ever written back, in a prompt, a re-prompt or an error (ADR-0031).
//
// Two empty answers are a credential-less configuration rather than a credential
// of ":", which is the same rule readCredential applies to empty standard input —
// ADR-0028 provides for a machine that keeps no secret on disk at all, and the
// caller says so out loud on stderr. Nothing here judges the shape of what was
// typed beyond that; a colon inside a public key is the far end's business.
//
// The joined value is held to the same ceiling a piped one is, so a paste that
// is really a redirected file cannot reach a 0600 store truncated.
func promptCredential(prompt prompter) (string, error) {
	publicKey, err := prompt.Line("Public key: ")
	if err != nil {
		return "", err
	}
	secretKey, err := prompt.Secret("Secret key (not shown): ")
	if err != nil {
		return "", err
	}
	publicKey, secretKey = strings.TrimSpace(publicKey), strings.TrimSpace(secretKey)
	if publicKey == "" && secretKey == "" {
		return "", nil
	}
	credential := publicKey + ":" + secretKey
	if len(credential) > maxCredentialBytes {
		return "", errCredentialTooLarge
	}
	return credential, nil
}
