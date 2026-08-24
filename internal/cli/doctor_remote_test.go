//go:build remote

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Acceptance criterion 5, first half: under the remote tag, doctor reports the
// delivery state, and the numbers it prints are the real ones rather than a
// placeholder row.
func TestDoctorReportsTheDeliveryState(t *testing.T) {
	paths := isolateRemote(t)
	_, endpoint := serveRemote(t)
	configureRemote(t, endpoint)
	seedSpool(t, paths, 0, 3)

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}

	for _, want := range []string{
		"remote endpoint: configured",
		"remote credential: set",
		"remote delivery: on",
		"remote last flush: never",
		"remote delivered through: 0",
		"remote pending: 3",
		// The section is an addition, not a replacement: doctor's own last
		// line is still there.
		"integration: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output is missing %q:\n%s", want, out)
		}
	}
}

// Acceptance criterion 5, second half, and ADR-0028/ADR-0029 made mechanical.
// The state asserted against is the one most able to leak: an endpoint that is
// configured and on, with a credential and a non-empty spool.
//
// ADR-0029 carves the bare host out of the never-echo rule for exactly one
// consumer, `wake remote status`. doctor is not that consumer — its output is
// what people paste into issues (ADR-0019 §7) — so the host must not appear
// here even though another command may print it.
func TestDoctorRemoteSectionCarriesNoPathSeparatorOrEndpoint(t *testing.T) {
	paths := isolateRemote(t)
	_, endpoint := serveRemote(t)
	configureRemote(t, endpoint)
	seedSpool(t, paths, 0, 3)

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}

	// Nothing doctor prints has a legitimate slash, so a slash is a path or a
	// URL. The strongest available form of the check, and the same one
	// doctor_test.go makes — restated here against the state that populates
	// the section.
	if strings.Contains(out, "/") {
		t.Errorf("doctor output carries a path separator:\n%s", out)
	}
	for _, secret := range []string{remoteTestPublicKey, remoteTestSecretKey} {
		if strings.Contains(out, secret) {
			t.Errorf("doctor output carries the credential %q:\n%s", secret, out)
		}
	}
	host := strings.Split(strings.TrimPrefix(endpoint, "http://"), "/")[0]
	if strings.Contains(out, host) {
		t.Errorf("doctor output carries the endpoint host %q:\n%s", host, out)
	}
}

// A delivery state that cannot be read is reported as unreadable, never as a row
// of zeros: zeros would read as a healthy delivery path that has sent nothing,
// which is the "collects nothing" / "collects zero" conflation doctor exists to
// prevent (ADR-0010, plan §12).
//
// remote.Describe is made to fail through its credential-store check, using only
// exported surface: config.ResolvePaths creates nothing, so the config directory
// is free to be occupied by a regular file, and checkStateDir then refuses it
// with an error that is not fs.ErrNotExist.
func TestDoctorReportsAnUnreadableDeliveryStateRatherThanZeros(t *testing.T) {
	paths := isolateRemote(t)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigDir), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(paths.ConfigDir, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	// A diagnostic reports its own failure rather than failing — the property
	// doctor already holds for an unreadable counter file.
	if err != nil {
		t.Fatalf("doctor error = %v, want nil: a diagnostic reports its own failure", err)
	}

	if !strings.Contains(out, "remote delivery: unreadable") {
		t.Errorf("doctor output does not report the unreadable delivery state:\n%s", out)
	}
	// The assertion that fails if a later change makes the error path render a
	// zero-valued Status instead.
	for _, unwanted := range []string{"remote pending: 0", "remote delivered through: 0"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("doctor reported %q for a delivery state it could not read:\n%s", unwanted, out)
		}
	}
}
