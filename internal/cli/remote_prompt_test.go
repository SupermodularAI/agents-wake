package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
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

// forbid fails when text carries any of the values a prompt may never show.
// Written over the transcript and the writer together, because a prompt is
// output even though it is not on stdout.
func forbid(t *testing.T, text string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(text, value) {
			t.Errorf("what the terminal displayed carries %q", value)
		}
	}
}

// What ADR-0031 means by "confirm it": the destination's bare host, never the
// URL. The path can hold a token, the query is where an API key usually hides,
// and the userinfo is a credential outright — so the value that goes to the
// store is the whole URL and the value shown to the person is url.Host.
func TestConfirmingAPromptedURLShowsOnlyItsHost(t *testing.T) {
	const url = "https://user:pw@api.example.com:4318/v1/traces"
	fake := &fakeTerminal{answers: []string{url, "y"}}
	var buf bytes.Buffer

	endpoint, err := confirmEndpoint(fake, &buf, "")
	if err != nil {
		t.Fatalf("confirmEndpoint() = %v", err)
	}
	if endpoint != url {
		t.Errorf("confirmEndpoint() = %q, want the URL byte-for-byte", endpoint)
	}

	// Byte-for-byte rather than a "contains the host" check: the confirmation is
	// a fixed literal with url.Host interpolated into it, and pinning the whole
	// string is what proves nothing else from the URL reached it. A "contains no
	// /" assertion could not say this — the prompt's own [y/N] carries one.
	const want = "Deliver records to api.example.com:4318? [y/N]: "
	if len(fake.shown) != 2 || fake.shown[1] != want {
		t.Errorf("the confirmation = %q, want exactly [%q]", fake.shown, want)
	}
	forbid(t, fake.transcript()+buf.String(), "user", "pw", "v1", "traces", "https://", "4318/")
}

// A URL passed as an argument was typed by the same person at the same terminal,
// and a mistyped host is as easy to introduce that way, so it is confirmed too —
// but not re-asked for.
func TestAnArgumentURLIsConfirmedToo(t *testing.T) {
	const given = "https://api.example.com/v1/traces"
	fake := &fakeTerminal{answers: []string{"y"}}
	var buf bytes.Buffer

	endpoint, err := confirmEndpoint(fake, &buf, given)
	if err != nil {
		t.Fatalf("confirmEndpoint() = %v", err)
	}
	if endpoint != given {
		t.Errorf("confirmEndpoint() = %q, want %q", endpoint, given)
	}
	if len(fake.shown) != 1 {
		t.Fatalf("prompts shown = %q, want only the confirmation", fake.shown)
	}
	if !strings.Contains(fake.shown[0], "api.example.com") {
		t.Errorf("the confirmation = %q, which does not name the host", fake.shown[0])
	}
}

// Declining re-prompts rather than aborting. The whole point of the confirmation
// is catching a mistyped host, and re-prompting is the repair — aborting would
// make the user re-run the command and re-type a secret they had already given.
func TestDecliningTheHostRePromptsForTheURL(t *testing.T) {
	for name, decline := range map[string]string{
		"an explicit no": "n",
		"a bare Return":  "",
		"anything else":  "nope",
	} {
		t.Run(name, func(t *testing.T) {
			const right = "https://right.example.com/v1/traces"
			fake := &fakeTerminal{answers: []string{"https://wrong.example.com/v1/traces", decline, right, "y"}}
			var buf bytes.Buffer

			endpoint, err := confirmEndpoint(fake, &buf, "")
			if err != nil {
				t.Fatalf("confirmEndpoint() = %v", err)
			}
			if endpoint != right {
				t.Errorf("confirmEndpoint() = %q, want the second URL", endpoint)
			}
			displayed := fake.transcript()
			for _, host := range []string{"wrong.example.com", "right.example.com"} {
				if !strings.Contains(displayed, host) {
					t.Errorf("the transcript never named %q: %q", host, displayed)
				}
			}
		})
	}
}

// Declining an argument URL asks for a new one rather than re-offering the
// argument, which would be an unbreakable loop.
func TestDecliningAnArgumentURLRePromptsForOne(t *testing.T) {
	const right = "https://right.example.com/v1/traces"
	fake := &fakeTerminal{answers: []string{"n", right, "y"}}
	var buf bytes.Buffer

	endpoint, err := confirmEndpoint(fake, &buf, "https://wrong.example.com/otel")
	if err != nil {
		t.Fatalf("confirmEndpoint() = %v", err)
	}
	if endpoint != right {
		t.Errorf("confirmEndpoint() = %q, want the prompted URL", endpoint)
	}
}

// A URL the store would reject is refused here, before anything is written, and
// the refusal names the rule rather than the value: a URL is exactly where a
// credential hides, so a rejected one is not quoted back either.
func TestANonHTTPURLIsRefusedAndRePrompted(t *testing.T) {
	const ok = "https://ok.example.com/v1/traces"
	fake := &fakeTerminal{answers: []string{"ftp://api.example.com/v1/traces", ok, "y"}}
	var buf bytes.Buffer

	endpoint, err := confirmEndpoint(fake, &buf, "")
	if err != nil {
		t.Fatalf("confirmEndpoint() = %v", err)
	}
	if endpoint != ok {
		t.Errorf("confirmEndpoint() = %q, want %q", endpoint, ok)
	}
	if !strings.Contains(buf.String(), "not an absolute http:// or https:// URL") {
		t.Errorf("the refusal never named the rule: %q", buf.String())
	}
	forbid(t, fake.transcript()+buf.String(), "ftp://api.example.com")
}

// A Ctrl-D ends the wizard with nothing written. This is also what bounds the
// re-prompt loop.
func TestConfirmationAtEOFReturnsAnError(t *testing.T) {
	fake := &fakeTerminal{}
	var buf bytes.Buffer

	endpoint, err := confirmEndpoint(fake, &buf, "")
	if !errors.Is(err, io.EOF) {
		t.Errorf("confirmEndpoint() error = %v, want io.EOF", err)
	}
	if endpoint != "" {
		t.Errorf("confirmEndpoint() = %q, want no endpoint", endpoint)
	}
}

// ADR-0028 §Context names only the secret half as the credential, so the public
// key is echoed like any other input and the secret key is not echoed at all.
// Neither the secret nor the joined string is ever written back.
func TestTheSecretKeyIsAskedForWithEchoOff(t *testing.T) {
	fake := &fakeTerminal{answers: []string{"pk-lf-dg74", "sk-lf-dg74"}}

	credential, err := promptCredential(fake)
	if err != nil {
		t.Fatalf("promptCredential() = %v", err)
	}
	if credential != "pk-lf-dg74:sk-lf-dg74" {
		t.Errorf("promptCredential() = %q, want the joined pair", credential)
	}
	if !slices.Contains(fake.echoed, "pk-lf-dg74") {
		t.Errorf("the public key was not asked for with echo on: echoed %q", fake.echoed)
	}
	if !slices.Contains(fake.masked, "sk-lf-dg74") {
		t.Errorf("the secret key was not asked for with echo off: masked %q", fake.masked)
	}
	if slices.Contains(fake.masked, "pk-lf-dg74") || slices.Contains(fake.echoed, "sk-lf-dg74") {
		t.Errorf("the two halves were asked for the wrong way round: echoed %q, masked %q", fake.echoed, fake.masked)
	}
	forbid(t, fake.transcript(), "sk-lf-dg74", "pk-lf-dg74:sk-lf-dg74")
}

// Two empty answers are a credential-less configuration, not a credential of
// ":" — the same rule readCredential applies to empty standard input.
func TestBothKeysEmptyIsACredentiallessConfiguration(t *testing.T) {
	fake := &fakeTerminal{answers: []string{"", ""}}

	credential, err := promptCredential(fake)
	if err != nil {
		t.Fatalf("promptCredential() = %v", err)
	}
	if credential != "" {
		t.Errorf("promptCredential() = %q, want no credential", credential)
	}
}

// Judging the shape of what was typed is explicitly out of scope: the join is
// mechanical, and a half-filled pair is the far end's business to reject.
func TestOnlyOneKeyGivenIsStillJoined(t *testing.T) {
	cases := map[string]struct {
		answers []string
		want    string
	}{
		"public only": {[]string{"pk-lf-dg74", ""}, "pk-lf-dg74:"},
		"secret only": {[]string{"", "sk-lf-dg74"}, ":sk-lf-dg74"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeTerminal{answers: tc.answers}

			credential, err := promptCredential(fake)
			if err != nil {
				t.Fatalf("promptCredential() = %v", err)
			}
			if credential != tc.want {
				t.Errorf("promptCredential() = %q, want %q", credential, tc.want)
			}
		})
	}
}

// A typed credential is held to the ceiling a piped one is, so a paste that is
// really a redirected file cannot reach a 0600 store truncated. The refusal
// names the limit and never the value.
func TestAnOversizedTypedCredentialIsRefused(t *testing.T) {
	fake := &fakeTerminal{answers: []string{strings.Repeat("A", maxCredentialBytes), "B"}}

	credential, err := promptCredential(fake)
	if !errors.Is(err, errCredentialTooLarge) {
		t.Fatalf("promptCredential() error = %v, want errCredentialTooLarge", err)
	}
	if credential != "" {
		t.Errorf("promptCredential() = %q, want no credential", credential)
	}
	if strings.Contains(err.Error(), strings.Repeat("A", 64)) {
		t.Error("the refusal quotes back what was typed")
	}
}

// A failed read is returned bare. io.EOF carries no value, and wrapping it with
// anything typed here would be the one place a prompt could carry input back out.
func TestSecretPromptFailureIsNotWrapped(t *testing.T) {
	fake := &fakeTerminal{answers: []string{"pk-lf-dg74"}}

	credential, err := promptCredential(fake)
	if !errors.Is(err, io.EOF) {
		t.Errorf("promptCredential() error = %v, want io.EOF", err)
	}
	if credential != "" {
		t.Errorf("promptCredential() = %q, want no credential", credential)
	}
}
