package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/remote"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// The credential every test here configures. Both halves are distinctive
// strings, so a substring search for one of them proves something rather than
// matching some unrelated word.
const (
	remoteTestPublicKey = "pk-lf-dg66"
	remoteTestSecretKey = "sk-lf-dg66"
	remoteTestSecret    = remoteTestPublicKey + ":" + remoteTestSecretKey
)

// runRemote runs the command tree with the three streams separate, because two
// of this file's assertions are about what is *not* on stdout and one is about
// stdout's exact bytes. runSplit sets no stdin, and `remote set` reads the
// credential from it.
func runRemote(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// isolateRemote is isolate with the credential override cleared, for the reason
// internal/remote's testPaths clears it: a developer who exports
// WAKE_REMOTE_AUTHORIZATION must not see different results from CI, and every
// case below is about what the store holds.
func isolateRemote(t *testing.T) config.Paths {
	t.Helper()
	t.Setenv(config.EnvRemoteAuthorization, "")
	return isolate(t)
}

// seedSpool appends n distinct valid records to the spool, deriving each id from
// its own source event so the store's deduplication does not collapse them
// (ADR-0004).
func seedSpool(t *testing.T, p config.Paths, from, n int) {
	t.Helper()
	records := make([]record.Record, 0, n)
	for i := from; i < from+n; i++ {
		records = append(records, record.Record{
			SchemaVersion: record.SchemaVersion,
			EventID:       record.DeriveEventID("claude-code", record.Identifier("dg66-"+strconv.Itoa(i))),
			Timestamp:     time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC),
			Harness:       "claude-code",
			SessionID:     "session-dg66",
			Repo:          "0123456789abcdef0123456789abcdef",
			Kind:          record.KindSkill,
			Name:          "commit-message",
			Invoker:       record.InvokerModel,
		})
	}
	result, err := store.New(filepath.Join(p.DataDir, "events.ndjson")).Append(records)
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if result.Written != n {
		t.Fatalf("Append() wrote %d of %d records (%+v)", result.Written, n, result)
	}
}

// remoteSpy records every request it receives and answers with the status its
// statuses slice names for that request's index, repeating the last one once the
// slice runs out — which is what lets a test say "batch 1 is accepted, batch 2
// is rejected" without knowing when the batches arrive. Mirrored from
// internal/remote's own spy rather than shared, because a test helper in that
// package is not importable from this one.
type remoteSpy struct {
	mu       sync.Mutex
	statuses []int
	bodies   [][]byte
}

func (s *remoteSpy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	index := len(s.bodies)
	s.bodies = append(s.bodies, body)
	status := http.StatusOK
	switch {
	case index < len(s.statuses):
		status = s.statuses[index]
	case len(s.statuses) > 0:
		status = s.statuses[len(s.statuses)-1]
	}
	s.mu.Unlock()
	w.WriteHeader(status)
}

func (s *remoteSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *remoteSpy) body(t *testing.T, index int) []byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.bodies) {
		t.Fatalf("the receiver got %d requests, want at least %d", len(s.bodies), index+1)
	}
	return s.bodies[index]
}

func serveRemote(t *testing.T, statuses ...int) (*remoteSpy, string) {
	t.Helper()
	receiver := &remoteSpy{statuses: statuses}
	server := httptest.NewServer(receiver)
	t.Cleanup(server.Close)
	return receiver, server.URL
}

// configureRemote drives `remote set` and `remote on` through the command tree,
// so the state every test below starts from is state the commands themselves
// produced rather than state a helper reached past them to write.
func configureRemote(t *testing.T, endpoint string) {
	t.Helper()
	if _, _, err := runRemote(t, remoteTestSecret, "remote", "set", endpoint); err != nil {
		t.Fatalf("remote set error = %v", err)
	}
	if _, _, err := runRemote(t, "", "remote", "on"); err != nil {
		t.Fatalf("remote on error = %v", err)
	}
}

func gunzipBody(t *testing.T, body []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the body is not gzipped: %v", err)
	}
	defer reader.Close()
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the gzipped body: %v", err)
	}
	return decompressed
}

// Acceptance criterion 1. ADR-0018: unsetting a URL to pause delivery is
// hostile, so `off` preserves it. Asserted at the store as well as at the
// rendering — an `off` that cleared the endpoint would print the same line.
func TestRemoteOffPreservesTheEndpointAndStatusReportsPaused(t *testing.T) {
	paths := isolateRemote(t)
	configureRemote(t, "https://api.example.com/v1/traces")

	if _, _, err := runRemote(t, "", "remote", "off"); err != nil {
		t.Fatalf("remote off error = %v", err)
	}

	stdout, _, err := runRemote(t, "", "remote", "status")
	if err != nil {
		t.Fatalf("remote status error = %v", err)
	}
	if !strings.Contains(stdout, "endpoint: api.example.com") {
		t.Errorf("status does not report the configured endpoint:\n%s", stdout)
	}
	if !strings.Contains(stdout, "state: off") {
		t.Errorf("status does not report the paused state:\n%s", stdout)
	}

	auth, err := config.LoadRemoteAuth(paths)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() error = %v", err)
	}
	if auth.Endpoint == "" {
		t.Error("the endpoint was cleared: turning delivery off must not discard where it was going")
	}
	if auth.Credential == "" {
		t.Error("the credential was cleared")
	}
	if auth.Enabled {
		t.Error("Enabled = true, want false")
	}
}

// Acceptance criterion 2.
func TestDryRunPrintsAPayloadAndMakesNoRequest(t *testing.T) {
	paths := isolateRemote(t)
	receiver, endpoint := serveRemote(t)
	configureRemote(t, endpoint)
	seedSpool(t, paths, 0, 3)

	stdout, _, err := runRemote(t, "", "remote", "flush", "--dry-run")
	if err != nil {
		t.Fatalf("remote flush --dry-run error = %v", err)
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout is not newline-terminated: %q", stdout)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout holds %d lines, want 1", len(lines))
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("stdout is not valid JSON: %v", err)
	}
	if _, ok := decoded["resourceSpans"]; !ok {
		t.Errorf("the payload has no resourceSpans key: %v", decoded)
	}
	if got := receiver.count(); got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
}

// Acceptance criterion 3. The dry run happens first so the watermark has not
// moved, then the same records are really posted and the two are compared byte
// for byte. Gzip is transport framing applied after encoding, so the posted body
// is gunzipped before the comparison.
func TestDryRunIsByteIdenticalToWhatAFlushSends(t *testing.T) {
	paths := isolateRemote(t)
	receiver, endpoint := serveRemote(t)
	configureRemote(t, endpoint)
	seedSpool(t, paths, 0, 3)

	stdout, _, err := runRemote(t, "", "remote", "flush", "--dry-run")
	if err != nil {
		t.Fatalf("remote flush --dry-run error = %v", err)
	}
	if _, _, err := runRemote(t, "", "remote", "flush"); err != nil {
		t.Fatalf("remote flush error = %v", err)
	}
	if got := receiver.count(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}

	posted := gunzipBody(t, receiver.body(t, 0))
	printed := []byte(strings.TrimSuffix(stdout, "\n"))
	if !bytes.Equal(posted, printed) {
		t.Errorf("the dry run printed\n%s\nbut the flush posted\n%s", printed, posted)
	}
}

// A real flush POSTs one body per batch, so the exact payload the next flush
// would send is plural whenever the record ceiling splits. One payload per line
// is unambiguous framing because Encode returns json.Marshal output, which
// carries no literal newline.
//
// 501 is one more than internal/remote's unexported maxBatchRecords = 500, which
// this package cannot see. internal/remote's own
// TestPreviewFlushSplitsIntoTheSameBatchesAFlushWould uses the constant
// directly, so a change to it fails there rather than only making this vacuous.
func TestDryRunPrintsOnePayloadPerBatch(t *testing.T) {
	paths := isolateRemote(t)
	_, endpoint := serveRemote(t)
	configureRemote(t, endpoint)
	seedSpool(t, paths, 0, 501)

	stdout, _, err := runRemote(t, "", "remote", "flush", "--dry-run")
	if err != nil {
		t.Fatalf("remote flush --dry-run error = %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout holds %d lines, want 2 — one payload per batch", len(lines))
	}
	for i, line := range lines {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i, err)
		}
	}
}

// Acceptance criterion 4, first half. Nothing read from stdin is ever reflected:
// terminal scrollback outlives the command, and a credential echoed once is a
// credential in a screenshot.
func TestSetNeverEchoesTheCredential(t *testing.T) {
	isolateRemote(t)

	stdout, stderr, err := runRemote(t, remoteTestSecret, "remote", "set", "https://api.example.com/v1/traces")
	if err != nil {
		t.Fatalf("remote set error = %v", err)
	}
	for _, secret := range []string{remoteTestSecret, remoteTestSecretKey, remoteTestPublicKey} {
		if strings.Contains(stdout+stderr, secret) {
			t.Errorf("remote set echoed %q:\nstdout: %s\nstderr: %s", secret, stdout, stderr)
		}
	}
	if !strings.Contains(stdout, "remote endpoint configured") {
		t.Errorf("remote set did not confirm what it did:\n%s", stdout)
	}
}

// Acceptance criterion 4, second half. An argv secret lands in shell history and
// in `ps` output for every user on the machine, so a credential in argv is
// refused rather than accepted with a warning — and a refusal writes nothing.
func TestSetRejectsACredentialPassedAsAnArgument(t *testing.T) {
	cases := map[string][]string{
		"as a second argument": {"remote", "set", "https://api.example.com/v1/traces", remoteTestSecret},
		"in place of the URL":  {"remote", "set", remoteTestSecret},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			paths := isolateRemote(t)

			stdout, stderr, err := runRemote(t, remoteTestSecret, args...)
			if err == nil {
				t.Fatal("remote set error = nil, want a refusal")
			}
			if strings.Contains(stdout+stderr+err.Error(), remoteTestSecretKey) {
				t.Errorf("the refusal echoed the credential:\nstdout: %s\nstderr: %s\nerror: %v", stdout, stderr, err)
			}
			auth, loadErr := config.LoadRemoteAuth(paths)
			if loadErr != nil {
				t.Fatalf("LoadRemoteAuth() error = %v", loadErr)
			}
			if auth.Endpoint != "" {
				t.Error("a refused `remote set` wrote an endpoint")
			}
		})
	}
}

// Acceptance criterion 5. A path can hold a token, userinfo is a credential
// outright, and a query string is where an API key usually hides — so `status`
// carries the host and port and nothing else about the destination.
func TestStatusCarriesNoPathSeparatorAndNoCredential(t *testing.T) {
	t.Run("a real endpoint", func(t *testing.T) {
		paths := isolateRemote(t)
		_, endpoint := serveRemote(t)
		configureRemote(t, endpoint)
		seedSpool(t, paths, 0, 3)

		stdout, _, err := runRemote(t, "", "remote", "status")
		if err != nil {
			t.Fatalf("remote status error = %v", err)
		}
		if strings.Contains(stdout, "/") {
			t.Errorf("status output holds a path separator:\n%s", stdout)
		}
		for _, forbidden := range []string{remoteTestSecretKey, remoteTestPublicKey, paths.ConfigDir, paths.DataDir} {
			if strings.Contains(stdout, forbidden) {
				t.Errorf("status output holds %q:\n%s", forbidden, stdout)
			}
		}
	})

	t.Run("an endpoint carrying userinfo and a path", func(t *testing.T) {
		isolateRemote(t)
		configureRemote(t, "https://user:pw@api.example.com:4318/v1/traces")

		stdout, _, err := runRemote(t, "", "remote", "status")
		if err != nil {
			t.Fatalf("remote status error = %v", err)
		}
		if !strings.Contains(stdout, "api.example.com:4318") {
			t.Errorf("status does not name the host and port:\n%s", stdout)
		}
		for _, forbidden := range []string{"pw", "v1", "user"} {
			if strings.Contains(stdout, forbidden) {
				t.Errorf("status output holds %q from the URL:\n%s", forbidden, stdout)
			}
		}
	})
}

// Acceptance criterion 6. ADR-0018 requires a dead endpoint to be
// indistinguishable from an absent one from the user's point of view, and a
// non-zero exit from a command that did everything it could is a failure report
// about the far end rather than about this machine.
func TestFlushAgainstADeadEndpointExitsZeroPromptly(t *testing.T) {
	paths := isolateRemote(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close()

	configureRemote(t, endpoint)
	seedSpool(t, paths, 0, 3)

	startedAt := time.Now()
	stdout, stderr, err := runRemote(t, "", "remote", "flush")
	elapsed := time.Since(startedAt)

	if err != nil {
		t.Fatalf("remote flush error = %v, want nil — a dead endpoint is not this machine's failure", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("remote flush took %v, want under 2s — no command may wait on the network", elapsed)
	}
	if !strings.Contains(stderr, "the remote endpoint could not be reached") {
		t.Errorf("stderr does not carry the plain message:\n%s", stderr)
	}
	if strings.Contains(stdout+stderr, remoteTestSecretKey) {
		t.Errorf("the failure message echoed the credential:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}

// Ticket acceptance, mechanically: adding a command touches no shared file.
// `git diff` is not available inside a unit test, so the property the acceptance
// is about is asserted instead — and unlike a diff assertion, it still holds
// after the next ticket adds a command too.
func TestRegistryAndRootAreUnmodified(t *testing.T) {
	for _, name := range []string{"registry.go", "root.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", name, err)
		}
		if strings.Contains(string(raw), "remote") {
			t.Errorf("%s names the remote command: a subcommand self-registers from its own file", name)
		}
	}
}

// Ticket acceptance: every command works with stdout redirected to a file. The
// dry run comes before the real flush so the spool is still pending when it
// runs, which is what makes its file non-empty.
func TestEveryRemoteCommandWorksWithStdoutRedirectedToAFile(t *testing.T) {
	paths := isolateRemote(t)
	_, endpoint := serveRemote(t)
	seedSpool(t, paths, 0, 3)

	runs := [][]string{
		{"remote", "status"},
		{"remote", "set", endpoint},
		{"remote", "on"},
		{"remote", "off"},
		{"remote", "on"},
		{"remote", "flush", "--dry-run"},
		{"remote", "flush"},
	}
	for i, args := range runs {
		path := filepath.Join(t.TempDir(), "stdout-"+strconv.Itoa(i))
		file, err := os.Create(path)
		if err != nil {
			t.Fatalf("Create(%s) error = %v", path, err)
		}

		cmd := newRootCmd()
		cmd.SetOut(file)
		cmd.SetErr(io.Discard)
		cmd.SetIn(strings.NewReader(remoteTestSecret))
		cmd.SetArgs(args)
		runErr := cmd.Execute()
		closeErr := file.Close()

		if runErr != nil {
			t.Fatalf("%v error = %v", args, runErr)
		}
		if closeErr != nil {
			t.Fatalf("closing %s: %v", path, closeErr)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", path, err)
		}
		if info.Size() == 0 {
			t.Errorf("%v wrote nothing to a redirected stdout", args)
		}
	}
}

// Enabled without an endpoint can only fail later, in a background flush nobody
// is watching, so it is refused at the point somebody is watching. The refusal
// names the rule and no path.
func TestOnWithoutAnEndpointIsRefused(t *testing.T) {
	isolateRemote(t)

	_, _, err := runRemote(t, "", "remote", "on")
	if err == nil {
		t.Fatal("remote on error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "must be set before remote delivery can be enabled") {
		t.Errorf("refusal = %v, want it to name the rule", err)
	}
	if strings.Contains(err.Error(), string(os.PathSeparator)) {
		t.Errorf("refusal = %v, which carries a path", err)
	}
}

// `set` on a fresh machine leaves delivery off and says so, rather than starting
// delivery as a side effect of naming a destination. Two commands, so the moment
// records start leaving the machine is a moment the user chose.
func TestSetOnAFreshStoreLeavesDeliveryOffAndSaysSo(t *testing.T) {
	paths := isolateRemote(t)

	stdout, _, err := runRemote(t, remoteTestSecret, "remote", "set", "https://api.example.com/v1/traces")
	if err != nil {
		t.Fatalf("remote set error = %v", err)
	}
	if !strings.Contains(stdout, "wake remote on") {
		t.Errorf("remote set does not point at the command that starts delivery:\n%s", stdout)
	}

	status, err := remote.Describe(paths)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if status.Enabled {
		t.Error("Enabled = true, want false — naming a destination must not start delivery")
	}
}

func TestFlushReportsWhatItSent(t *testing.T) {
	paths := isolateRemote(t)
	_, endpoint := serveRemote(t)
	configureRemote(t, endpoint)
	seedSpool(t, paths, 0, 3)

	stdout, _, err := runRemote(t, "", "remote", "flush")
	if err != nil {
		t.Fatalf("remote flush error = %v", err)
	}
	if stdout != "sent 3 records in 1 batch.\n" {
		t.Errorf("stdout = %q, want %q", stdout, "sent 3 records in 1 batch.\n")
	}
}

// An oversized standard input is a redirected file, not a credential, and
// truncating it to the limit would write 4 KiB of some other file into a 0600
// store as if it were the secret. The far end would then reject every batch with
// nothing pointing at the truncation, so the refusal happens here, where the
// mistake is still legible.
//
// The limit is a boundary, so both sides of it are asserted: exactly
// maxCredentialBytes is a credential, one byte more is a mistake.
func TestSetRefusesAnOversizedCredential(t *testing.T) {
	const endpoint = "https://api.example.com/v1/traces"

	t.Run("one byte over the limit", func(t *testing.T) {
		paths := isolateRemote(t)
		oversized := strings.Repeat("A", maxCredentialBytes+1)

		stdout, stderr, err := runRemote(t, oversized, "remote", "set", endpoint)
		if err == nil {
			t.Fatal("remote set error = nil, want a refusal — a truncated credential is not a credential")
		}
		if !strings.Contains(err.Error(), strconv.Itoa(maxCredentialBytes)) {
			t.Errorf("refusal = %v, want it to name the limit", err)
		}
		// The refusal names the limit and never the value: what was read is
		// still a secret even when it is the wrong length (ADR-0028).
		if strings.Contains(stdout+stderr+err.Error(), strings.Repeat("A", 64)) {
			t.Errorf("the refusal echoed what was read:\nstdout: %s\nstderr: %s\nerror: %v", stdout, stderr, err)
		}

		auth, loadErr := config.LoadRemoteAuth(paths)
		if loadErr != nil {
			t.Fatalf("LoadRemoteAuth() error = %v", loadErr)
		}
		if auth.Endpoint != "" || auth.Credential != "" {
			t.Error("a refused `remote set` wrote to the store")
		}
	})

	t.Run("exactly the limit", func(t *testing.T) {
		paths := isolateRemote(t)
		atLimit := strings.Repeat("A", maxCredentialBytes)

		if _, _, err := runRemote(t, atLimit, "remote", "set", endpoint); err != nil {
			t.Fatalf("remote set error = %v, want the limit itself to be accepted", err)
		}
		auth, err := config.LoadRemoteAuth(paths)
		if err != nil {
			t.Fatalf("LoadRemoteAuth() error = %v", err)
		}
		if len(auth.Credential) != maxCredentialBytes {
			t.Errorf("the stored credential is %d bytes, want %d", len(auth.Credential), maxCredentialBytes)
		}
	})
}

// ADR-0028 provides for a machine that keeps no secret on disk at all, so naming
// a destination without one is a configuration rather than a mistake. It is
// still a state that delivers nothing until the environment supplies the
// credential, so `set` says so on stderr instead of leaving it to be discovered
// by a flush that reports a clean zero.
func TestSetWithoutACredentialConfiguresTheEndpointOnly(t *testing.T) {
	paths := isolateRemote(t)

	stdout, stderr, err := runRemote(t, "", "remote", "set", "https://api.example.com/v1/traces")
	if err != nil {
		t.Fatalf("remote set error = %v", err)
	}
	if !strings.Contains(stdout, "remote endpoint configured") {
		t.Errorf("remote set did not confirm what it did:\n%s", stdout)
	}
	if strings.Contains(stdout, "api.example.com") {
		t.Errorf("remote set named the endpoint host, which ADR-0029 carves out for `remote status` only:\n%s", stdout)
	}
	if !strings.Contains(stderr, "no credential is configured") {
		t.Errorf("stderr does not say the endpoint has no credential:\n%s", stderr)
	}
	if !strings.Contains(stderr, config.EnvRemoteAuthorization) {
		t.Errorf("stderr does not name the other way to supply one:\n%s", stderr)
	}

	auth, err := config.LoadRemoteAuth(paths)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() error = %v", err)
	}
	if auth.Endpoint == "" {
		t.Error("the endpoint was not written")
	}
	if auth.Credential != "" {
		t.Error("a credential was written from an empty standard input")
	}
}

// An endpoint that is configured and on but has no credential delivers nothing.
// Before the third condition was visible to this package it fell through the two
// checks below into a silent zero return, and `flush` reported a delivery that
// never happened — an unreachable configuration rendered as a healthy one.
func TestFlushWithoutACredentialSaysNothingWasSent(t *testing.T) {
	paths := isolateRemote(t)
	receiver, endpoint := serveRemote(t)

	if _, _, err := runRemote(t, "", "remote", "set", endpoint); err != nil {
		t.Fatalf("remote set error = %v", err)
	}
	if _, _, err := runRemote(t, "", "remote", "on"); err != nil {
		t.Fatalf("remote on error = %v", err)
	}
	seedSpool(t, paths, 0, 3)

	stdout, _, err := runRemote(t, "", "remote", "flush")
	if err != nil {
		t.Fatalf("remote flush error = %v", err)
	}
	if stdout != "no credential is configured; nothing was sent.\n" {
		t.Errorf("stdout = %q, want the missing-credential message", stdout)
	}
	if got := receiver.count(); got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
}

// `status` answers the same three conditions a flush gates on, so a
// configuration that delivers nothing cannot read as a healthy one. Presence
// only: every byte of a credential is the secret.
func TestStatusReportsWhetherACredentialIsConfigured(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		isolateRemote(t)
		configureRemote(t, "https://api.example.com/v1/traces")

		stdout, _, err := runRemote(t, "", "remote", "status")
		if err != nil {
			t.Fatalf("remote status error = %v", err)
		}
		if !strings.Contains(stdout, "credential: set") {
			t.Errorf("status does not report the credential as present:\n%s", stdout)
		}
		if strings.Contains(stdout, remoteTestSecretKey) || strings.Contains(stdout, remoteTestPublicKey) {
			t.Errorf("status carries the credential itself:\n%s", stdout)
		}
	})

	t.Run("absent", func(t *testing.T) {
		isolateRemote(t)
		if _, _, err := runRemote(t, "", "remote", "set", "https://api.example.com/v1/traces"); err != nil {
			t.Fatalf("remote set error = %v", err)
		}

		stdout, _, err := runRemote(t, "", "remote", "status")
		if err != nil {
			t.Fatalf("remote status error = %v", err)
		}
		if !strings.Contains(stdout, "credential: not configured") {
			t.Errorf("status does not report the missing credential:\n%s", stdout)
		}
	})
}

// The no-secret-on-disk path ADR-0028 exists for, end to end: an endpoint
// configured with no credential, the credential supplied by the environment, and
// a flush that actually delivers.
func TestTheEnvironmentCredentialAloneDelivers(t *testing.T) {
	paths := isolateRemote(t)
	receiver, endpoint := serveRemote(t)

	if _, _, err := runRemote(t, "", "remote", "set", endpoint); err != nil {
		t.Fatalf("remote set error = %v", err)
	}
	if _, _, err := runRemote(t, "", "remote", "on"); err != nil {
		t.Fatalf("remote on error = %v", err)
	}
	seedSpool(t, paths, 0, 3)
	t.Setenv(config.EnvRemoteAuthorization, remoteTestSecret)

	stdout, _, err := runRemote(t, "", "remote", "flush")
	if err != nil {
		t.Fatalf("remote flush error = %v", err)
	}
	if stdout != "sent 3 records in 1 batch.\n" {
		t.Errorf("stdout = %q, want the sent line", stdout)
	}
	if got := receiver.count(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}

	// The whole point of the override: nothing was written to the store on the
	// way through. Read back with the variable cleared, so what is asserted is
	// what the file holds.
	t.Setenv(config.EnvRemoteAuthorization, "")
	auth, err := config.LoadRemoteAuth(paths)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() error = %v", err)
	}
	if auth.Credential != "" {
		t.Error("the environment's credential was persisted to disk")
	}
}

// When batch 1 is accepted and batch 2 is rejected, 500 records did leave the
// machine. Reporting only the failure would tell the user the endpoint could not
// be reached while saying nothing about what it already holds — and `flush`'s
// whole job is to report what it sent.
//
// 501 is one more than internal/remote's unexported maxBatchRecords = 500, for
// the reason TestDryRunPrintsOnePayloadPerBatch gives.
func TestPartialFlushReportsWhatItSentAndThatItFailed(t *testing.T) {
	paths := isolateRemote(t)
	receiver, endpoint := serveRemote(t, http.StatusOK, http.StatusInternalServerError)
	configureRemote(t, endpoint)
	seedSpool(t, paths, 0, 501)

	stdout, stderr, err := runRemote(t, "", "remote", "flush")
	if err != nil {
		t.Fatalf("remote flush error = %v, want nil — the far end's refusal is not this machine's failure", err)
	}
	if got := receiver.count(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if stdout != "sent 500 records in 1 batch.\n" {
		t.Errorf("stdout = %q, want what the accepted batch carried", stdout)
	}
	if !strings.Contains(stderr, "the remote endpoint rejected a batch") {
		t.Errorf("stderr does not carry the failure:\n%s", stderr)
	}
}

// A dead endpoint exits 0; a local failure must not. flushLocked joins the
// delivery error with the delivery-state write's, and errors.Is is satisfied by
// either member of a join — so classifying on the sentinel alone would report a
// watermark that never persisted as a benign far-end problem and exit 0.
func TestOnlyAPureDeliveryFailureExitsZero(t *testing.T) {
	writeFailed := errors.New("writing the delivery state failed")
	cases := map[string]struct {
		err  error
		want bool
	}{
		"no failure":                   {nil, false},
		"unreachable":                  {remote.ErrDeliveryFailed, true},
		"rejected, wrapped by status":  {fmt.Errorf("status %d: %w", 500, remote.ErrDeliveryRejected), true},
		"joined with nothing else":     {errors.Join(remote.ErrDeliveryFailed, nil), true},
		"joined with a local failure":  {errors.Join(remote.ErrDeliveryFailed, writeFailed), false},
		"a local failure on its own":   {writeFailed, false},
		"two delivery failures joined": {errors.Join(remote.ErrDeliveryFailed, remote.ErrDeliveryRejected), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isDeliveryFailure(tc.err); got != tc.want {
				t.Errorf("isDeliveryFailure(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// Telling a user who ran `flush` deliberately that delivery is off is the
// difference between a quiet success and a silent one.
func TestFlushSaysNothingWasSentWhenDeliveryIsOff(t *testing.T) {
	paths := isolateRemote(t)
	_, endpoint := serveRemote(t)
	if _, _, err := runRemote(t, remoteTestSecret, "remote", "set", endpoint); err != nil {
		t.Fatalf("remote set error = %v", err)
	}
	seedSpool(t, paths, 0, 3)

	stdout, _, err := runRemote(t, "", "remote", "flush")
	if err != nil {
		t.Fatalf("remote flush error = %v", err)
	}
	if stdout != "remote delivery is off; nothing was sent.\n" {
		t.Errorf("stdout = %q, want the off message", stdout)
	}
}

// DG-72, Issue 1. A flush the minimum interval held back never read the spool,
// so "sent 0 records in 0 batches." reports a run that did not happen — the same
// sentence a flush that ran and found nothing prints. The two must read
// differently, and the throttled one names the key that changes it.
func TestFlushSaysTheMinimumIntervalHeldItBack(t *testing.T) {
	paths := isolateRemote(t)
	receiver, endpoint := serveRemote(t)
	configureRemote(t, endpoint)
	if _, err := config.Set(paths, "remote.min_interval", "15m"); err != nil {
		t.Fatalf("Set(remote.min_interval) error = %v", err)
	}
	seedSpool(t, paths, 0, 3)

	if _, _, err := runRemote(t, "", "remote", "flush"); err != nil {
		t.Fatalf("first remote flush error = %v", err)
	}

	seedSpool(t, paths, 3, 3)
	stdout, stderr, err := runRemote(t, "", "remote", "flush")
	if err != nil {
		t.Fatalf("second remote flush error = %v", err)
	}
	if stdout != "a flush ran less than remote.min_interval ago; nothing was sent.\n" {
		t.Errorf("stdout = %q, want the throttled message", stdout)
	}
	if strings.Contains(stdout+stderr, endpoint) {
		t.Errorf("the throttled message echoed the endpoint:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if got := receiver.count(); got != 1 {
		t.Errorf("requests = %d, want 1 — the second flush must post nothing", got)
	}
}

// DG-72, Issue 2. ADR-0029's carve-out is exactly one command — a bare host, on
// stdout, from `remote status`, typed by the person who configured it. `set`
// confirms what it did and points at that command instead of answering with the
// destination, so the carve-out cannot widen by accident.
func TestSetDoesNotEchoTheEndpoint(t *testing.T) {
	isolateRemote(t)

	stdout, stderr, err := runRemote(t, remoteTestSecret, "remote", "set", "https://user:pw@api.example.com:4318/v1/traces")
	if err != nil {
		t.Fatalf("remote set error = %v", err)
	}
	for _, forbidden := range []string{"api.example.com", "4318", "user", "pw", "v1"} {
		if strings.Contains(stdout+stderr, forbidden) {
			t.Errorf("remote set echoed %q from the endpoint:\nstdout: %s\nstderr: %s", forbidden, stdout, stderr)
		}
	}
	if !strings.Contains(stdout, "remote endpoint configured") {
		t.Errorf("remote set did not confirm what it did:\n%s", stdout)
	}
	if !strings.Contains(stdout, "wake remote status") {
		t.Errorf("remote set does not point at the one command that shows the endpoint:\n%s", stdout)
	}
}
