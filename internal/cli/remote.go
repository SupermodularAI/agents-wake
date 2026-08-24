//go:build remote

// The `remote` command surface, and the four rules it is bound by.
//
// It is absent from the default build. ADR-0012 compiles remote delivery out
// rather than configuring it off, and a `remote` line in `wake --help` — even one
// that only reported being unsupported — would turn "this binary contains no
// network code" from something a reader can see into something they have to
// verify. remote_absent_test.go asserts that mechanically.
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
// credential in a screenshot.
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

// errCredentialMissing is phrased to carry the rule it enforces, and carries
// nothing that was read (ADR-0028).
var errCredentialMissing = errors.New("a credential must be supplied on standard input, not as an argument")

func newRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Configure and run delivery of records to a remote endpoint",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newRemoteStatusCmd(), newRemoteSetCmd(), newRemoteOnCmd(), newRemoteOffCmd(), newRemoteFlushCmd())
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

// writeRemoteStatus prints the three states ADR-0018 names, in `key: value`
// lines like doctor's. "Not compiled in" is the third state and is expressed by
// this command not existing at all, which is why only two state words appear
// here.
//
// The endpoint line carries the host and port and nothing else — no scheme, no
// path, no query, no userinfo, never the credential (ADR-0028). Together with the
// state line it is what makes "configured but paused" legible: an endpoint
// present with `state: off`.
func writeRemoteStatus(w io.Writer, status remote.Status, host string) error {
	endpoint := host
	if endpoint == "" {
		endpoint = "not configured"
	}
	state := "off"
	if status.Enabled {
		state = "on"
	}
	lastFlush := "never"
	if !status.LastFlush.IsZero() {
		lastFlush = status.LastFlush.UTC().Format(time.RFC3339)
	}
	_, err := fmt.Fprintf(w, "endpoint: %s\nstate: %s\nlast flush: %s\ndelivered through: %d\npending: %d\n",
		endpoint, state, lastFlush, status.DeliveredThrough, status.Pending)
	return err
}

func newRemoteSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <url>",
		Short: "Set the delivery endpoint, reading its credential from standard input",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.ResolvePaths()
			if err != nil {
				return err
			}
			credential, err := readCredential(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if err = config.SetRemoteEndpoint(paths, args[0], credential); err != nil {
				return err
			}

			host, err := config.RemoteEndpointHost(paths)
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "remote endpoint set to %s.\n", host); err != nil {
				return err
			}

			// Naming a destination is not the same act as starting to deliver to
			// it, so `set` on a fresh machine leaves delivery off and points at
			// the command that starts it. That is the same non-hostility argument
			// ADR-0018 makes for `off` not clearing the endpoint, in the other
			// direction: the moment records start leaving the machine is a moment
			// the user chose.
			status, err := remote.Describe(paths)
			if err != nil {
				return err
			}
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
func readCredential(in io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(in, maxCredentialBytes))
	if err != nil {
		return "", errCredentialMissing
	}
	credential := strings.TrimSpace(string(raw))
	if credential == "" {
		return "", errCredentialMissing
	}
	return credential, nil
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
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "remote delivery is on.")
			return err
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

			// The three states, read rather than derived: Describe already
			// computes them, and telling a user who ran `flush` deliberately that
			// delivery is off is the difference between a quiet success and a
			// silent one.
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

			report, flushErr := remote.FlushReport(paths)
			if errors.Is(flushErr, remote.ErrDeliveryFailed) || errors.Is(flushErr, remote.ErrDeliveryRejected) {
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
			if flushErr != nil {
				return flushErr
			}

			if report.Dropped > 0 {
				if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "%s could not be encoded and were not sent.\n", quantity(report.Dropped, "record")); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "sent %s in %s.\n", quantity(report.Records, "record"), quantity(report.Batches, "batch"))
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the exact payload the next flush would send and send nothing")
	return cmd
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
		if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "%s could not be encoded and would not be sent.\n", quantity(preview.Dropped, "record")); err != nil {
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
