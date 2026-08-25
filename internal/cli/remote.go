// The `remote` command surface, and the four rules it is bound by.
//
// It ships in every binary and is off until a user runs `remote set` and
// `remote on`. Until both have happened there is no endpoint, no credential and
// no enabled flag, and nothing is sent — the claim ADR-0030 makes in place of
// ADR-0012's compiled-out one, which this command's own output is how a user
// checks.
//
// It parses and prints only (ADR-0001). No batching, no HTTP client, no gzip and
// no watermark arithmetic happens here: internal/remote owns all four, and this
// file calls Describe, PreviewFlush and FlushReport for answers it must not
// re-derive.
//
// stdout carries the answer and stderr the caveats, so
// `wake remote flush --dry-run > payload.json` yields exactly the bytes that
// would leave and nothing else. Every command still works with stdout redirected
// to a file.
//
// The credential is read from standard input and never reflected (ADR-0028).
// Terminal scrollback outlives the command, and a credential echoed once is a
// credential in a screenshot. The endpoint's bare host is the one value from a
// URL this file may print, and exactly two commands may print it: `status`,
// reading it back from the store, and `set`'s interactive confirmation, showing
// a value in flight before it is written. ADR-0029 carved the first out of
// ADR-0028's never-echo rule and ADR-0031 revises the carve-out to those two and
// no third. Run non-interactively, `set` still confirms without naming the
// destination at all, and neither path ever prints the secret key or the joined
// credential.
package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/remote"
)

func init() { commands = append(commands, newRemoteCmd) }

// maxCredentialBytes bounds what `set` will read from stdin. A credential is a
// colon-joined key pair; anything larger is a mistake — a file redirected by
// accident — and reading it whole would put it in this process's memory for no
// reason.
const maxCredentialBytes = 4 << 10

// The ways `set` refuses what it was given, all carrying the rule and never the
// value (ADR-0028).
//
// errCredentialTooLarge exists because the alternative is silent corruption:
// reading up to the limit and stopping would store the first 4 KiB of whatever
// was redirected as if it were the whole secret, at mode 0600, with the far end
// then rejecting every batch and nothing pointing at the truncation. The message
// names the limit because that is the one number the user needs and the one
// value here that is not a secret.
var (
	errCredentialInput    = errors.New("the credential could not be read from standard input")
	errCredentialTooLarge = fmt.Errorf("the credential on standard input is longer than %d bytes, which is a redirected file rather than a credential", maxCredentialBytes)
	errCredentialInArgv   = errors.New("a credential must be supplied on standard input, not as an argument")

	// A zero-argument `set` with nothing but a pipe on standard input is the one
	// invocation the interactive path could have quietly changed the meaning of.
	// It stays a refusal, and names both ways out: pass the URL, or run it where
	// there is somebody to ask (ADR-0031).
	errEndpointRequired = errors.New("a URL is required: pass it as an argument, or run this at a terminal to be prompted for one")
)

func newRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Configure and run delivery of records to a remote endpoint",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newRemoteStatusCmd(), newRemoteSetCmd(osPrompter), newRemoteOnCmd(), newRemoteOffCmd(), newRemoteFlushCmd())
	return cmd
}

func newRemoteStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report where records are delivered, whether they are, and what is pending",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			status, err := remote.Describe(paths)
			if err != nil {
				return err
			}
			host, err := config.RemoteEndpointHost(paths)
			if err != nil {
				return err
			}
			return writeRemoteStatus(cmd.OutOrStdout(), status, host)
		},
	}
}

// writeRemoteStatus prints the two states ADR-0018 names as corrected by
// ADR-0030 — off and on — in `key: value` lines like doctor's.
//
// The endpoint line carries the host and port and nothing else — no scheme, no
// path, no query, no userinfo (ADR-0029). The credential line carries presence
// and nothing else at all: every byte of a credential is the secret, so it has
// no bare-host analogue. Together with the state line they are the three
// conditions a flush gates on, so a configuration that delivers nothing cannot
// read here as a healthy one — and "configured but paused" stays legible as an
// endpoint present with `state: off`.
func writeRemoteStatus(w io.Writer, status remote.Status, host string) error {
	endpoint := host
	if endpoint == "" {
		endpoint = "not configured"
	}
	credential := "not configured"
	if status.CredentialConfigured {
		credential = "set"
	}
	state := "off"
	if status.Enabled {
		state = "on"
	}
	lastFlush := "never"
	if !status.LastFlush.IsZero() {
		lastFlush = status.LastFlush.UTC().Format(time.RFC3339)
	}
	_, err := fmt.Fprintf(w, "endpoint: %s\ncredential: %s\nstate: %s\nlast flush: %s\ndelivered through: %d\npending: %d\n",
		endpoint, credential, state, lastFlush, status.DeliveredThrough, status.Pending)
	return err
}

func newRemoteSetCmd(newPrompter promptFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "set [url]",
		Short: "Set the delivery endpoint, reading its credential from standard input",
		// A second positional argument is refused with the rule rather than
		// with cobra's arity message, because the mistake it almost always is —
		// the credential typed onto the command line — is the one thing this
		// command exists to prevent.
		//
		// Zero arguments is now admissible because a terminal can be asked for
		// the URL. It is not admissible without one: RunE refuses a
		// zero-argument scripted invocation, so the piped path answers exactly
		// as it did.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return errCredentialInArgv
			}
			return cobra.MaximumNArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}

			// Two sources for the same two values, and one write. Which branch
			// runs is decided by standard input alone: a terminal gets prompts,
			// and a pipe, a file or CI gets the single whole read this command
			// has always done (ADR-0031 §1).
			var endpoint, credential string
			if prompt := newPrompter(cmd); prompt != nil {
				given := ""
				if len(args) == 1 {
					given = args[0]
				}
				if endpoint, credential, err = promptEndpointAndCredential(prompt, cmd.ErrOrStderr(), given); err != nil {
					return err
				}
			} else {
				if len(args) != 1 {
					return errEndpointRequired
				}
				endpoint = args[0]
				if credential, err = readCredential(cmd.InOrStdin()); err != nil {
					return err
				}
			}

			if err = config.SetRemoteEndpoint(paths, endpoint, credential); err != nil {
				return err
			}

			// The destination is not named back, on either path. ADR-0031
			// widens ADR-0029's carve-out to let the interactive confirmation
			// show a bare host *before* the write, which is where it changes an
			// outcome; it does not license naming the destination after the
			// fact, where the only thing left to do about it is read `status`.
			// So this line confirms the write and points at the command allowed
			// to answer "where", and it is identical whether a person was
			// prompted or a script piped.
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), `remote endpoint configured; run "wake remote status" to see it.`); err != nil {
				return err
			}

			status, err := remote.Describe(paths)
			if err != nil {
				return err
			}
			if err = writeMissingCredential(cmd, status); err != nil {
				return err
			}

			// Naming a destination is not the same act as starting to deliver to
			// it, so `set` on a fresh machine leaves delivery off and points at
			// the command that starts it. That is the same non-hostility argument
			// ADR-0018 makes for `off` not clearing the endpoint, in the other
			// direction: the moment records start leaving the machine is a moment
			// the user chose.
			if status.Enabled {
				return nil
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), `remote delivery is off; run "wake remote on" to start delivering.`)
			return err
		},
	}
}

// readCredential takes the credential from standard input, never from argv: an
// argv secret lands in shell history and in `ps` output for every user on the
// machine. Nothing it returns is ever printed, and its refusals name the rule
// rather than the value (ADR-0028).
//
// Empty input is a credential-less configuration rather than a refusal.
// ADR-0028 provides for a machine that keeps no secret on disk at all, and
// before this the only route to that state was to store a placeholder secret and
// shadow it with the environment, which is the opposite of what the override is
// for. The caller says so out loud; nothing about it is silent.
//
// One byte past the limit is refused rather than truncated. io.LimitReader stops
// at its ceiling with a nil error, so reading exactly maxCredentialBytes could
// not tell a whole credential from the first 4 KiB of a redirected file — and
// the first 4 KiB of a file, written to a 0600 store as if it were the secret,
// fails at the far end with nothing pointing back here. Reading one byte past
// the ceiling is what makes the two distinguishable. Trailing whitespace is
// trimmed before the length is judged, so a credential at exactly the limit
// survives the newline a shell adds.
func readCredential(in io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(in, maxCredentialBytes+1))
	if err != nil {
		return "", errCredentialInput
	}
	credential := strings.TrimSpace(string(raw))
	if len(credential) > maxCredentialBytes {
		return "", errCredentialTooLarge
	}
	return credential, nil
}

// writeMissingCredential says, on stderr, that nothing authorises delivery yet.
//
// A configured endpoint that is on with no credential sends nothing, so every
// command that leaves the machine in that state says so at the moment it does,
// rather than leaving it to be discovered by a flush that reports a clean zero.
// It names the environment override because that is the other way to supply one
// and the reason this state is a configuration rather than a mistake (ADR-0028).
func writeMissingCredential(cmd *cobra.Command, status remote.Status) error {
	if status.CredentialConfigured {
		return nil
	}
	_, err := fmt.Fprintf(cmd.ErrOrStderr(),
		"no credential is configured; supply one on standard input or set %s.\n", config.EnvRemoteAuthorization)
	return err
}

func newRemoteOnCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "on",
		Short: "Start delivering records to the configured endpoint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			if err = config.SetRemoteEnabled(paths, true); err != nil {
				return err
			}
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), "remote delivery is on."); err != nil {
				return err
			}

			status, err := remote.Describe(paths)
			if err != nil {
				return err
			}
			return writeMissingCredential(cmd, status)
		},
	}
}

func newRemoteOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Stop delivering records, keeping the configured endpoint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			if err = config.SetRemoteEnabled(paths, false); err != nil {
				return err
			}
			// The observable half of ADR-0018's "unsetting a URL to pause is
			// hostile": the message states that the endpoint survived, so nobody
			// has to re-enter it to find out.
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "remote delivery is off; the endpoint is unchanged.")
			return err
		},
	}
}

func newRemoteFlushCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "flush",
		Short: "Deliver everything pending now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			if dryRun {
				return writeDryRun(cmd, paths)
			}

			// All three conditions a flush gates on, read rather than derived:
			// Describe already computes them, and telling a user who ran `flush`
			// deliberately why nothing left is the difference between a quiet
			// success and a silent one. Checking two of the three would let the
			// third fall through into FlushReport's silent zero return and print
			// a flush that never happened.
			status, err := remote.Describe(paths)
			if err != nil {
				return err
			}
			if !status.EndpointConfigured {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "no remote endpoint is configured; nothing was sent.")
				return err
			}
			if !status.Enabled {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "remote delivery is off; nothing was sent.")
				return err
			}
			if !status.CredentialConfigured {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "no credential is configured; nothing was sent.")
				return err
			}

			report, flushErr := remote.FlushReport(paths)

			// What it sent is printed before why it stopped, and on the failure
			// path too. A flush whose second batch was refused still put the
			// first one on the wire, and "the endpoint could not be reached"
			// with no mention of the 500 records that did leave is the one case
			// where "reports what it sent" would stop being true.
			if err = writeFlushReport(cmd, report, flushErr != nil); err != nil {
				return err
			}

			if isDeliveryFailure(flushErr) {
				// Exit 0 with a plain message. ADR-0018 requires a dead endpoint
				// to be indistinguishable from an absent one from the user's
				// point of view, and a non-zero exit from a command that did
				// everything it could is a failure report about the far end
				// rather than about this machine. The sentinel's own text is
				// printed verbatim: it is valueless on purpose and must not be
				// wrapped into something naming the endpoint (ADR-0028).
				_, err = fmt.Fprintln(cmd.ErrOrStderr(), flushErr)
				return err
			}
			return flushErr
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the exact payload the next flush would send and send nothing")
	return cmd
}

// writeFlushReport prints what the flush put on the wire, and on stderr what the
// encoder refused.
//
// A run that failed before sending anything prints no count: the failure below
// it is the whole of what happened, and "sent 0 records in 0 batches." beside it
// reads as a second, contradictory answer. A run that failed after sending
// something prints both, because that is the case where what was sent is not
// obvious from anywhere else.
//
// A run the minimum interval held back prints neither: it never read the spool,
// so it has no counts to report and the throttle is the whole of what happened.
func writeFlushReport(cmd *cobra.Command, report remote.Report, failed bool) error {
	if report.Suppressed {
		// Printed instead of the counts rather than beside them: "sent 0 records
		// in 0 batches." describes a run that read the spool, and this run never
		// did. The minimum interval is named because it is configuration the
		// reader can change, and it is the only thing said — nothing here
		// reports the far end's state (ADR-0018, ADR-0028).
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "a flush ran less than remote.min_interval ago; nothing was sent.")
		return err
	}
	if failed && report == (remote.Report{}) {
		return nil
	}
	if report.Dropped > 0 {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s were not sent.\n", quantity(report.Dropped, "record")); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "sent %s in %s.\n", quantity(report.Records, "record"), quantity(report.Batches, "batch"))
	return err
}

// isDeliveryFailure reports whether err is the far end's failure and nothing
// else — the one condition under which `flush` exits 0.
//
// flushLocked joins the delivery error with the delivery-state write's, and
// errors.Is is satisfied by any member of a join. Asking it directly would
// therefore report a watermark that never persisted — a local failure, and one
// that makes the next flush re-send — as a benign far-end problem, and exit 0 on
// it. So every member of a join has to answer, not just one.
func isDeliveryFailure(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		members := joined.Unwrap()
		if len(members) == 0 {
			return false
		}
		for _, member := range members {
			if !isDeliveryFailure(member) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, remote.ErrDeliveryFailed) || errors.Is(err, remote.ErrDeliveryRejected)
}

// writeDryRun prints the payloads a flush would send, one per line, and sends
// nothing.
//
// One line per batch because a flush posts one body per batch: Encode returns
// json.Marshal output, which carries no literal newline, so the framing is
// unambiguous and each line is byte-for-byte the body the corresponding request
// would carry (ADR-0027). Gzip is transport framing applied after encoding, so it
// is absent here — printing compressed bytes would defeat the one purpose the
// flag has.
//
// stdout carries the payload and nothing else, so
// `wake remote flush --dry-run > payload.json` yields exactly what would leave.
// Every caveat goes to stderr.
func writeDryRun(cmd *cobra.Command, paths config.Paths) error {
	preview, err := remote.PreviewFlush(paths)
	if err != nil {
		return err
	}
	for _, payload := range preview.Batches {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", payload); err != nil {
			return err
		}
	}
	if preview.Dropped > 0 {
		if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "%s would not be sent.\n", quantity(preview.Dropped, "record")); err != nil {
			return err
		}
	}
	if len(preview.Batches) == 0 {
		_, err = fmt.Fprintln(cmd.ErrOrStderr(), "nothing to send.")
	}
	return err
}

// quantity renders a count with a noun that agrees with it, for the reason
// terminalEvents in root.go does: "1 records" makes a carefully derived number
// look machine-generated in a tool whose whole claim is that its numbers were
// derived carefully.
func quantity(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if noun == "batch" {
		return fmt.Sprintf("%d batches", n)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
