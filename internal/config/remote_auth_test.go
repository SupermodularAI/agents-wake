package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// remoteTestPaths is testPaths with the credential override cleared, for the
// reason testPaths gives for clearing EnvDataDir: a developer who exports
// WAKE_REMOTE_AUTHORIZATION must not see different results from CI, and every
// case below is about what the store holds or what an explicitly set override
// does to it.
func remoteTestPaths(t *testing.T) Paths {
	t.Helper()
	p := testPaths(t)
	t.Setenv(EnvRemoteAuthorization, "")
	return p
}

// Tokens no message of ours contains for another reason, so a match is a leak
// and never a coincidence — the shape TestNoErrorPathLeaksTheSaltOrARepoPath
// already uses.
const (
	testEndpoint   = "https://remote-auth-unmistakable-host.example/api/public/otel/v1/traces"
	testCredential = "pk-lf-unmistakable-public:sk-lf-unmistakable-secret"
	envCredential  = "pk-lf-from-the-environment:sk-lf-from-the-environment"
)

// writeRemoteAuthRaw puts hand-written bytes where the store belongs. Hand
// written is the point: several shapes below are ones SetRemoteAuth would never
// produce, and they are exactly the ones a reader has to survive.
func writeRemoteAuthRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the remote credential store: %v", err)
	}
}

// storeJSON is what a valid store of this version looks like on disk.
func storeJSON(version int) string {
	return fmt.Sprintf(`{"version":%d,"endpoint":%q,"enabled":true,"credential":%q}`+"\n",
		version, testEndpoint, testCredential)
}

// setRemoteAuthOrFail writes a fully populated store, so a case about a file's
// type, mode or contents fails for that reason and not for a bad write.
func setRemoteAuthOrFail(t *testing.T, p Paths) {
	t.Helper()
	if err := SetRemoteAuth(p, RemoteAuth{Endpoint: testEndpoint, Enabled: true, Credential: testCredential}); err != nil {
		t.Fatalf("SetRemoteAuth() = %v", err)
	}
}

func assertNoStore(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("os.Lstat(store) = %v, want fs.ErrNotExist — a refusal must write nothing", err)
	}
}

// The three fields are one unit with one lifecycle, so they are published
// together and read back together. `remote set` writes all three at once, which
// is why there is one file and one publication rather than three.
func TestRemoteAuthRoundTrips(t *testing.T) {
	p := remoteTestPaths(t)
	want := RemoteAuth{Endpoint: testEndpoint, Enabled: true, Credential: testCredential}

	if err := SetRemoteAuth(p, want); err != nil {
		t.Fatalf("SetRemoteAuth() = %v", err)
	}
	got, err := LoadRemoteAuth(p)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() = %v", err)
	}

	if got != want {
		t.Errorf("LoadRemoteAuth() = %+v, want %+v", got, want)
	}
}

// Acceptance item 1. The store holds a third-party-issued credential, so the
// mode is asserted rather than left to the process umask — and asserted on the
// file this build actually produces.
func TestRemoteAuthStoreIsCreatedAt0600(t *testing.T) {
	p := remoteTestPaths(t)
	setRemoteAuthOrFail(t, p)

	info, err := os.Lstat(filepath.Join(p.ConfigDir, remoteAuthFileName))
	if err != nil {
		t.Fatalf("os.Lstat(store) = %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("the store is %v, want a regular file", info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the store's mode = %#o, want 0600", perm)
	}
}

// A fresh install has no store, and asking whether remote delivery is on must
// not create one — the same rule a missing config.toml and a missing project
// table already follow.
func TestLoadRemoteAuthOnAMissingStoreIsNotAnError(t *testing.T) {
	p := remoteTestPaths(t)

	got, err := LoadRemoteAuth(p)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() on a fresh install = %v, want no error", err)
	}

	if got != (RemoteAuth{}) {
		t.Errorf("LoadRemoteAuth() = %+v, want the zero value", got)
	}
	assertNoStore(t, filepath.Join(p.ConfigDir, remoteAuthFileName))
}

// A store replaced by a symlink is a redirection: resolving it would read — and
// on the next publish, write — wherever it points, while every other check would
// still pass on the target.
func TestLoadRemoteAuthRejectsAStoreThatIsASymlink(t *testing.T) {
	p := remoteTestPaths(t)
	storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)
	elsewhere := filepath.Join(t.TempDir(), "their-remote-auth.json")
	writeRemoteAuthRaw(t, elsewhere, storeJSON(remoteAuthVersion))
	if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
		t.Fatalf("creating the config root: %v", err)
	}
	if err := os.Symlink(elsewhere, storePath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := LoadRemoteAuth(p)

	assertDisclosesNothing(t, err, elsewhere, storePath, testEndpoint, testCredential)
}

func TestLoadRemoteAuthRejectsAStoreThatIsNotARegularFile(t *testing.T) {
	p := remoteTestPaths(t)
	storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)
	if err := os.MkdirAll(storePath, 0o700); err != nil {
		t.Fatalf("creating a directory at the store path: %v", err)
	}

	_, err := LoadRemoteAuth(p)

	assertDisclosesNothing(t, err, storePath, testEndpoint, testCredential)
	// A directory in the way must not become a store beside it: refusing means
	// refusing, not routing around.
	info, statErr := os.Lstat(storePath)
	if statErr != nil || !info.IsDir() {
		t.Errorf("os.Lstat(store) = (%v, %v), want the directory still there", info, statErr)
	}
}

// A credential readable by anyone else on the machine is a credential the far
// end can no longer attribute to this user, and revoking it is the only remedy.
func TestLoadRemoteAuthRejectsAStoreMorePermissiveThan0600(t *testing.T) {
	for _, mode := range []os.FileMode{0o604, 0o640, 0o644, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			p := remoteTestPaths(t)
			storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)
			setRemoteAuthOrFail(t, p)
			if err := os.Chmod(storePath, mode); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}

			_, err := LoadRemoteAuth(p)

			assertDisclosesNothing(t, err, storePath, testEndpoint, testCredential)
		})
	}
}

// The fourth leg of the boundary repo-salt and projects.json already stand
// behind (OpenRepos, identity.go): the mode of a file is only as strong as the
// directory holding it. In a config root anyone else can write, another local
// user can rename their own 0600 file over the store between one read and the
// next — a credential of their choosing posted to a collector of their choosing —
// and checkSensitiveFile would pass it, because it tests type and mode and never
// ownership.
func TestRemoteAuthRefusesAGroupWritableConfigDirectory(t *testing.T) {
	for _, op := range []struct {
		name string
		run  func(p Paths) error
	}{
		{"LoadRemoteAuth", func(p Paths) error { _, err := LoadRemoteAuth(p); return err }},
		{"SetRemoteAuth", func(p Paths) error {
			return SetRemoteAuth(p, RemoteAuth{Endpoint: testEndpoint, Enabled: true, Credential: testCredential})
		}},
	} {
		for _, mode := range []os.FileMode{0o770, 0o707} {
			t.Run(op.name+"/"+mode.String(), func(t *testing.T) {
				p := remoteTestPaths(t)
				storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)
				setRemoteAuthOrFail(t, p)
				before := readFileOrFail(t, storePath)
				if err := os.Chmod(p.ConfigDir, mode); err != nil {
					t.Fatalf("Chmod() error = %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(p.ConfigDir, 0o700) })

				err := op.run(p)

				assertDisclosesNothing(t, err, storePath, p.ConfigDir, testEndpoint, testCredential)
				if after := readFileOrFail(t, storePath); after != before {
					t.Error("the store was republished into a directory this build refuses to write into")
				}
			})
		}
	}
}

// The endpoint is validated on the way in, so it is validated on the way out
// too: a store this build never wrote is exactly the one worth checking. ADR-0027
// makes OTLP/HTTP the only integration surface, and a scheme outside it is a
// credential posted somewhere no decision permits.
func TestLoadRemoteAuthRejectsAStoredEndpointThatIsNotHTTP(t *testing.T) {
	for _, endpoint := range []string{
		"ftp://" + testEndpoint,
		"file:///" + testCredential,
		"//" + testEndpoint,
		"https:///v1/traces",
	} {
		t.Run(endpoint, func(t *testing.T) {
			p := remoteTestPaths(t)
			storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)
			writeRemoteAuthRaw(t, storePath, fmt.Sprintf(
				`{"version":1,"endpoint":%q,"enabled":true,"credential":%q}`+"\n", endpoint, testCredential))

			_, err := LoadRemoteAuth(p)

			if !errors.Is(err, errEndpointNotHTTP) {
				t.Fatalf("LoadRemoteAuth() = %v, want errEndpointNotHTTP", err)
			}
			assertDisclosesNothing(t, err, storePath, endpoint, testCredential)
		})
	}
}

// Enabled without a destination can only fail later, in a background flush
// nobody is watching. SetRemoteAuth refuses to write that state; reading it back
// from a hand-edited store is the same state and gets the same answer.
func TestLoadRemoteAuthRejectsAStoreEnabledWithoutAnEndpoint(t *testing.T) {
	p := remoteTestPaths(t)
	storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)
	writeRemoteAuthRaw(t, storePath, fmt.Sprintf(
		`{"version":1,"endpoint":"","enabled":true,"credential":%q}`+"\n", testCredential))

	_, err := LoadRemoteAuth(p)

	if !errors.Is(err, errEndpointRequiredWhenOn) {
		t.Fatalf("LoadRemoteAuth() = %v, want errEndpointRequiredWhenOn", err)
	}
	assertDisclosesNothing(t, err, storePath, testCredential)
}

// The mirror of the two cases above: a store with delivery off and no endpoint
// is the state `remote off` on a fresh install produces, and refusing it would
// close the check on the correct case.
func TestLoadRemoteAuthAcceptsADisabledStoreWithNoEndpoint(t *testing.T) {
	p := remoteTestPaths(t)
	storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)
	writeRemoteAuthRaw(t, storePath, fmt.Sprintf(
		`{"version":1,"endpoint":"","enabled":false,"credential":%q}`+"\n", testCredential))

	got, err := LoadRemoteAuth(p)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() = %v, want a disabled store with no endpoint to load", err)
	}
	if want := (RemoteAuth{Credential: testCredential}); got != want {
		t.Errorf("LoadRemoteAuth() = %+v, want %+v", got, want)
	}
}

// A store that does not parse is an error, not an empty store: treating it as
// empty would silently stop delivering. And it is never rewritten from a failed
// parse — the bytes that failed are the only copy of the credential there is.
func TestLoadRemoteAuthRejectsAStoreThatDoesNotParse(t *testing.T) {
	p := remoteTestPaths(t)
	storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)
	content := fmt.Sprintf(`{"version":1,"endpoint":%q oops}`+"\n", testEndpoint)
	writeRemoteAuthRaw(t, storePath, content)

	_, err := LoadRemoteAuth(p)

	assertDisclosesNothing(t, err, storePath, testEndpoint, testCredential)
	if after := readFileOrFail(t, storePath); after != content {
		t.Errorf("the store was rewritten from a failed parse:\n got %q\nwant %q", after, content)
	}
}

// A future format read as this one would be silently wrong, with a credential
// posted to whatever the misread endpoint field happened to yield.
func TestLoadRemoteAuthRejectsAnUnknownVersion(t *testing.T) {
	p := remoteTestPaths(t)
	storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)
	writeRemoteAuthRaw(t, storePath, storeJSON(remoteAuthVersion+1))

	_, err := LoadRemoteAuth(p)

	assertDisclosesNothing(t, err, storePath, testEndpoint, testCredential)
	message := err.Error()
	for _, want := range []string{fmt.Sprint(remoteAuthVersion + 1), fmt.Sprint(remoteAuthVersion)} {
		if !strings.Contains(message, want) {
			t.Errorf("error %q does not name version %s; both are what makes the refusal actionable", message, want)
		}
	}
}

// ADR-0028: the variable overrides the stored credential. It exists for CI and
// for anyone who prefers no secret on disk at all.
func TestEnvAuthorizationOverridesTheStoredCredential(t *testing.T) {
	p := remoteTestPaths(t)
	setRemoteAuthOrFail(t, p)
	t.Setenv(EnvRemoteAuthorization, envCredential)

	got, err := LoadRemoteAuth(p)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() = %v", err)
	}

	if got.Credential != envCredential {
		t.Errorf("Credential = %q, want the environment's value", got.Credential)
	}
	// The override is scoped to the credential. A credential with no
	// destination is not a state any decision provides for, so the endpoint and
	// the enabled flag still come from the file.
	if got.Endpoint != testEndpoint {
		t.Errorf("Endpoint = %q, want the stored %q", got.Endpoint, testEndpoint)
	}
	if !got.Enabled {
		t.Error("Enabled = false, want the stored true")
	}
}

// ADR-0028: the stored value is never overwritten by an env-var read. A read
// that wrote back would make the variable a way to silently replace a
// credential on someone's disk.
func TestEnvAuthorizationIsNeverWrittenBackToTheStore(t *testing.T) {
	p := remoteTestPaths(t)
	storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)
	setRemoteAuthOrFail(t, p)
	t.Setenv(EnvRemoteAuthorization, envCredential)

	if _, err := LoadRemoteAuth(p); err != nil {
		t.Fatalf("LoadRemoteAuth() = %v", err)
	}

	raw := readFileOrFail(t, storePath)
	if !strings.Contains(raw, testCredential) {
		t.Error("the stored credential is gone; an env-var read must not rewrite the store")
	}
	if strings.Contains(raw, envCredential) {
		t.Error("the environment's credential was written to disk; the override is read-only")
	}
}

// Removing the variable reverts to whatever was last written to disk, so an
// override is a property of one process and not of the installation.
func TestRemovingEnvAuthorizationRevertsToTheStoredCredential(t *testing.T) {
	// The whitespace case is the rule ResolvePaths applies to WAKE_DIR: unset,
	// "" and a stray newline as `export X=$(cat somefile)` produces all mean the
	// same thing.
	for _, c := range []struct{ name, value string }{
		{"cleared", ""},
		{"whitespace only", "   "},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := remoteTestPaths(t)
			setRemoteAuthOrFail(t, p)
			t.Setenv(EnvRemoteAuthorization, c.value)

			got, err := LoadRemoteAuth(p)
			if err != nil {
				t.Fatalf("LoadRemoteAuth() = %v", err)
			}

			if got.Credential != testCredential {
				t.Errorf("Credential = %q, want the stored %q", got.Credential, testCredential)
			}
		})
	}
}

// ADR-0027 makes OTLP/HTTP JSON the only integration surface, so a file:// or
// ftp:// destination is a credential posted somewhere no decision permits. The
// check is scheme and host only: no reachability probe, no OTLP path assertion,
// nothing that reads the network.
func TestSetRemoteAuthRejectsAnEndpointThatIsNotHTTP(t *testing.T) {
	for _, c := range []struct {
		endpoint string
		// spelledByTheReason marks a value the refusal unavoidably contains,
		// because the reason has to name the schemes it does accept in order to
		// be actionable. Only the bare scheme is such a value: it carries no
		// host, no path and nothing of the user's, so a match against it is a
		// coincidence rather than a disclosure — the assertion that matters is
		// asserted for every case either way.
		spelledByTheReason bool
	}{
		{endpoint: "file:///etc/passwd"},
		{endpoint: "ftp://remote-auth-unmistakable-host.example/x"},
		{endpoint: "not-a-url"},
		{endpoint: "https://", spelledByTheReason: true},
		{endpoint: "/relative/path"},
	} {
		t.Run(c.endpoint, func(t *testing.T) {
			p := remoteTestPaths(t)
			storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)

			err := SetRemoteAuth(p, RemoteAuth{Endpoint: c.endpoint, Enabled: true, Credential: testCredential})

			disclosed := c.endpoint
			if c.spelledByTheReason {
				disclosed = ""
			}
			assertDisclosesNothing(t, err, disclosed, storePath, testCredential, p.ConfigDir)
			assertNoStore(t, storePath)
		})
	}
}

// Enabling delivery with nowhere to deliver to is a state that can only fail
// later, at a point where the failure is a background flush nobody is watching.
func TestSetRemoteAuthRejectsEnablingWithoutAnEndpoint(t *testing.T) {
	p := remoteTestPaths(t)
	storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)

	err := SetRemoteAuth(p, RemoteAuth{Enabled: true, Credential: testCredential})

	assertDisclosesNothing(t, err, storePath, testCredential)
	assertNoStore(t, storePath)
}

// The zero value is a legitimate store: it is how `remote set --off` clears one.
// A validation that refused it would leave no way to turn delivery off except
// deleting a file by hand.
func TestSetRemoteAuthAcceptsDisabledWithNoEndpoint(t *testing.T) {
	p := remoteTestPaths(t)

	if err := SetRemoteAuth(p, RemoteAuth{}); err != nil {
		t.Fatalf("SetRemoteAuth(zero) = %v, want the off state to be writable", err)
	}
	got, err := LoadRemoteAuth(p)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() = %v", err)
	}

	if got != (RemoteAuth{}) {
		t.Errorf("LoadRemoteAuth() = %+v, want the zero value", got)
	}
}

// Acceptance item 2, on the path where this kind of promise usually leaks
// (plan §4.2): every error this file can return is driven, and none of them may
// carry the endpoint, either credential, the store path or the config root.
func TestNoRemoteAuthErrorLeaksTheEndpointOrTheCredential(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(t *testing.T, p Paths, storePath string) error
	}{
		{
			name: "a store that is a symlink",
			run: func(t *testing.T, p Paths, storePath string) error {
				elsewhere := filepath.Join(t.TempDir(), "their-remote-auth.json")
				writeRemoteAuthRaw(t, elsewhere, storeJSON(remoteAuthVersion))
				if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
					t.Fatalf("creating the config root: %v", err)
				}
				if err := os.Symlink(elsewhere, storePath); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				_, err := LoadRemoteAuth(p)
				return err
			},
		},
		{
			name: "a store that is not a regular file",
			run: func(t *testing.T, p Paths, storePath string) error {
				if err := os.MkdirAll(storePath, 0o700); err != nil {
					t.Fatalf("creating a directory at the store path: %v", err)
				}
				_, err := LoadRemoteAuth(p)
				return err
			},
		},
		{
			name: "a store anyone can read",
			run: func(t *testing.T, p Paths, storePath string) error {
				setRemoteAuthOrFail(t, p)
				if err := os.Chmod(storePath, 0o644); err != nil {
					t.Fatalf("Chmod() error = %v", err)
				}
				_, err := LoadRemoteAuth(p)
				return err
			},
		},
		{
			name: "a store that does not parse",
			run: func(t *testing.T, p Paths, storePath string) error {
				writeRemoteAuthRaw(t, storePath, fmt.Sprintf(`{"version":1,"credential":%q oops}`, testCredential))
				_, err := LoadRemoteAuth(p)
				return err
			},
		},
		{
			name: "a store field with the wrong type",
			run: func(t *testing.T, p Paths, storePath string) error {
				writeRemoteAuthRaw(t, storePath, fmt.Sprintf(`{"version":1,"credential":[%q]}`, testCredential))
				_, err := LoadRemoteAuth(p)
				return err
			},
		},
		{
			name: "a store of an unknown version",
			run: func(t *testing.T, p Paths, storePath string) error {
				writeRemoteAuthRaw(t, storePath, storeJSON(remoteAuthVersion+1))
				_, err := LoadRemoteAuth(p)
				return err
			},
		},
		{
			name: "a stored endpoint that is not http",
			run: func(t *testing.T, p Paths, storePath string) error {
				writeRemoteAuthRaw(t, storePath, fmt.Sprintf(
					`{"version":1,"endpoint":%q,"enabled":true,"credential":%q}`, "ftp://"+testEndpoint, testCredential))
				_, err := LoadRemoteAuth(p)
				return err
			},
		},
		{
			name: "a stored enabled flag with no endpoint",
			run: func(t *testing.T, p Paths, storePath string) error {
				writeRemoteAuthRaw(t, storePath, fmt.Sprintf(
					`{"version":1,"endpoint":"","enabled":true,"credential":%q}`, testCredential))
				_, err := LoadRemoteAuth(p)
				return err
			},
		},
		{
			name: "a config root anyone can write into",
			run: func(t *testing.T, p Paths, _ string) error {
				setRemoteAuthOrFail(t, p)
				if err := os.Chmod(p.ConfigDir, 0o777); err != nil {
					t.Fatalf("Chmod() error = %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(p.ConfigDir, 0o700) })
				_, err := LoadRemoteAuth(p)
				return err
			},
		},
		{
			name: "an endpoint that is not http",
			run: func(_ *testing.T, p Paths, _ string) error {
				return SetRemoteAuth(p, RemoteAuth{Endpoint: "ftp://" + testEndpoint, Enabled: true, Credential: testCredential})
			},
		},
		{
			name: "enabling without an endpoint",
			run: func(_ *testing.T, p Paths, _ string) error {
				return SetRemoteAuth(p, RemoteAuth{Enabled: true, Credential: testCredential})
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := remoteTestPaths(t)
			t.Setenv(EnvRemoteAuthorization, envCredential)
			storePath := filepath.Join(p.ConfigDir, remoteAuthFileName)

			err := c.run(t, p, storePath)
			if err == nil {
				t.Fatal("the case produced no error; it proves nothing about error messages")
			}

			assertDisclosesNothing(t, err, testEndpoint, testCredential, envCredential, storePath, p.ConfigDir)
		})
	}
}

// The mechanical half of "never echo what was read": a %v of this struct in any
// error, log line or debug print cannot carry either secret, which is stronger
// than remembering not to write one. Both halves still have to be useful in a
// bug report, so presence is reported even though the values are not.
func TestRemoteAuthRedactsItselfWhenFormatted(t *testing.T) {
	a := RemoteAuth{Endpoint: testEndpoint, Enabled: true, Credential: testCredential}

	// The verb and the subject are loop variables rather than six literal
	// Sprintf calls, because what is under test is fmt's dispatch and not a
	// string conversion: the value and the pointer must both route through
	// String, under every verb that would otherwise print the fields. A future
	// String on a pointer receiver would leak through %v on the value, and this
	// is the test that would catch it.
	for _, verb := range []string{"%v", "%s", "%+v"} {
		for _, subject := range []any{a, &a} {
			rendered := fmt.Sprintf(verb, subject)

			for _, secret := range []string{testEndpoint, testCredential} {
				if strings.Contains(rendered, secret) {
					t.Errorf("%s of %T = %q, which leaks %q", verb, subject, rendered, secret)
				}
			}
			for _, want := range []string{"endpoint", "credential", "enabled"} {
				if !strings.Contains(rendered, want) {
					t.Errorf("%s of %T = %q, which says nothing about %s; redaction still has to be useful in a bug report", verb, subject, rendered, want)
				}
			}
		}
	}
}

// The record type is the allowlist (ADR-0007). RemoteAuth is what every consumer
// of the credential sees, so its field list is the guarantee: a field added
// later has to be justified here.
func TestRemoteAuthCarriesNoPathOrLabelField(t *testing.T) {
	assertFieldsAre(t, reflect.TypeOf(RemoteAuth{}), "Endpoint", "Enabled", "Credential")
}

// ADR-0028: never inside config.toml. That is the file users are asked to paste
// into a bug report (ADR-0019 §4), so the credential may not arrive there by any
// route — not as a value, and not as a key somebody could set.
func TestRemoteAuthNeverReachesConfigToml(t *testing.T) {
	p := remoteTestPaths(t)
	setRemoteAuthOrFail(t, p)
	if _, err := Set(p, "ui.default_window", "7d"); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	written := readFileOrFail(t, p.ConfigFile)
	for _, secret := range []string{testEndpoint, testCredential} {
		if strings.Contains(written, secret) {
			t.Errorf("config.toml holds %q", secret)
		}
	}
	for _, name := range KeyNames() {
		for _, forbidden := range []string{"credential", "endpoint", "authorization"} {
			if strings.Contains(strings.ToLower(name), forbidden) {
				t.Errorf("%q is a registered key; the credential and its destination are never configuration", name)
			}
		}
	}
}

// ADR-0018: turning delivery off must not discard where it was going. Asserted
// at the store rather than only at the rendering, because a `remote off` that
// silently cleared the endpoint would still print the same line.
func TestSetRemoteEnabledPreservesTheEndpointAndCredential(t *testing.T) {
	p := remoteTestPaths(t)
	setRemoteAuthOrFail(t, p)

	if err := SetRemoteEnabled(p, false); err != nil {
		t.Fatalf("SetRemoteEnabled(false) = %v", err)
	}

	got, err := LoadRemoteAuth(p)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() = %v", err)
	}
	if got.Endpoint != testEndpoint {
		t.Errorf("Endpoint = %q, want it unchanged", presence(got.Endpoint))
	}
	if got.Credential != testCredential {
		t.Errorf("Credential = %s, want it unchanged", presence(got.Credential))
	}
	if got.Enabled {
		t.Error("Enabled = true, want false")
	}
}

// The regression this entry point exists to make impossible. A write path that
// read through LoadRemoteAuth would persist the environment's credential to
// disk — the one thing ADR-0028 says that override must never do — and the
// resulting store would look entirely ordinary.
func TestSetRemoteEnabledNeverPersistsTheEnvironmentCredential(t *testing.T) {
	p := remoteTestPaths(t)
	setRemoteAuthOrFail(t, p)
	t.Setenv(EnvRemoteAuthorization, envCredential)

	if err := SetRemoteEnabled(p, false); err != nil {
		t.Fatalf("SetRemoteEnabled(false) = %v", err)
	}

	written := readFileOrFail(t, remoteAuthPath(p))
	if !strings.Contains(written, testCredential) {
		t.Error("the store no longer holds the credential it was written with")
	}
	if strings.Contains(written, envCredential) {
		t.Error("the store holds the environment's credential: an override was written to disk")
	}
}

// Enabled without an endpoint can only fail later, in a background flush nobody
// is watching, so validateRemoteAuth refuses it — and a refusal writes nothing.
func TestSetRemoteEnabledOnIsRefusedWithoutAnEndpoint(t *testing.T) {
	p := remoteTestPaths(t)

	if err := SetRemoteEnabled(p, true); err == nil {
		t.Fatal("SetRemoteEnabled(true) on a fresh store = nil, want a refusal")
	}
	assertNoStore(t, remoteAuthPath(p))
}

// `remote off` on a machine with nothing configured must not create a credential
// store. Writing one would put a file holding a secret's shape where there is no
// secret, which is the sort of thing a later reader treats as evidence.
func TestSetRemoteEnabledIsANoOpWhenAlreadyInThatState(t *testing.T) {
	p := remoteTestPaths(t)

	if err := SetRemoteEnabled(p, false); err != nil {
		t.Fatalf("SetRemoteEnabled(false) on a fresh store = %v, want nil", err)
	}
	assertNoStore(t, remoteAuthPath(p))
}

// Rotating a credential on a paused endpoint must not silently resume delivery,
// for the same reason `remote off` must not clear the endpoint (ADR-0018).
func TestSetRemoteEndpointPreservesTheStoredEnabledState(t *testing.T) {
	const rotatedEndpoint = "https://remote-auth-rotated-host.example/v1/traces"
	const rotatedCredential = "pk-lf-rotated-public:sk-lf-rotated-secret"

	p := remoteTestPaths(t)
	setRemoteAuthOrFail(t, p)

	if err := SetRemoteEndpoint(p, rotatedEndpoint, rotatedCredential); err != nil {
		t.Fatalf("SetRemoteEndpoint() = %v", err)
	}

	got, err := LoadRemoteAuth(p)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() = %v", err)
	}
	if !got.Enabled {
		t.Error("Enabled = false, want the stored state preserved")
	}
	if got.Endpoint != rotatedEndpoint {
		t.Error("Endpoint was not replaced by the one supplied")
	}
	if got.Credential != rotatedCredential {
		t.Error("Credential was not replaced by the one supplied")
	}
}

// An empty credential is a destination configured with no secret on disk, which
// ADR-0028 explicitly provides for: the environment supplies it, or nothing
// does and delivery reports that rather than attempting it.
//
// It clears whatever the store held, and that is the point rather than a cost: a
// destination and its credential are one unit, and carrying the previous
// endpoint's credential over to a new one would post a secret issued by one
// service to a different service entirely.
func TestSetRemoteEndpointAcceptsNoCredential(t *testing.T) {
	const replacementEndpoint = "https://remote-auth-replacement-host.example/v1/traces"

	p := remoteTestPaths(t)
	setRemoteAuthOrFail(t, p)

	if err := SetRemoteEndpoint(p, replacementEndpoint, ""); err != nil {
		t.Fatalf("SetRemoteEndpoint() = %v", err)
	}

	got, err := LoadRemoteAuth(p)
	if err != nil {
		t.Fatalf("LoadRemoteAuth() = %v", err)
	}
	if got.Endpoint != replacementEndpoint {
		t.Error("Endpoint was not replaced by the one supplied")
	}
	if got.Credential != "" {
		t.Error("Credential survived: the previous endpoint's secret would be posted to a new destination")
	}
	if strings.Contains(readFileOrFail(t, remoteAuthPath(p)), testCredential) {
		t.Error("the store still holds the previous credential")
	}
}

// The write path's half of ADR-0028: what lands on disk is the argument's
// credential, never whatever the environment happened to carry.
func TestSetRemoteEndpointNeverPersistsTheEnvironmentCredential(t *testing.T) {
	p := remoteTestPaths(t)
	t.Setenv(EnvRemoteAuthorization, envCredential)

	if err := SetRemoteEndpoint(p, testEndpoint, testCredential); err != nil {
		t.Fatalf("SetRemoteEndpoint() = %v", err)
	}

	written := readFileOrFail(t, remoteAuthPath(p))
	if !strings.Contains(written, testCredential) {
		t.Error("the store does not hold the credential that was supplied")
	}
	if strings.Contains(written, envCredential) {
		t.Error("the store holds the environment's credential: an override was written to disk")
	}
}

// What `remote status` may print about a destination: enough to recognise it,
// nothing that could carry a secret. A path can hold a token, userinfo is a
// credential outright, and a query string is where an API key usually hides.
func TestRemoteEndpointHostReportsHostAndPortOnly(t *testing.T) {
	cases := map[string]struct {
		endpoint string
		want     string
	}{
		"userinfo, port and path": {"https://user:pw@api.example.com:4318/v1/traces", "api.example.com:4318"},
		"loopback with a port":    {"http://127.0.0.1:4318/v1/traces", "127.0.0.1:4318"},
		"no store at all":         {"", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := remoteTestPaths(t)
			if tc.endpoint != "" {
				if err := SetRemoteEndpoint(p, tc.endpoint, testCredential); err != nil {
					t.Fatalf("SetRemoteEndpoint() = %v", err)
				}
			}

			got, err := RemoteEndpointHost(p)
			if err != nil {
				t.Fatalf("RemoteEndpointHost() = %v", err)
			}
			if got != tc.want {
				t.Errorf("RemoteEndpointHost() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "/") {
				t.Errorf("RemoteEndpointHost() = %q, which carries a path separator", got)
			}
			for _, forbidden := range []string{"v1", "pw", "user"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("RemoteEndpointHost() = %q, which carries %q", got, forbidden)
				}
			}
		})
	}
}

// The one derivation of a printable host, shared by `remote status` (a stored
// endpoint) and `remote set`'s interactive confirmation (one in flight) —
// ADR-0031 revises ADR-0029's carve-out to those two consumers and no third, so
// the rule is spelled once. An empty result means "not a destination this build
// stores", which is what lets a caller refuse before anything is written.
func TestEndpointHostIsHostAndPortOrNothing(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want string
	}{
		"userinfo, port and path": {"https://user:pw@api.example.com:4318/v1/traces", "api.example.com:4318"},
		"query string":            {"https://api.example.com/otel?key=sk-lf-secret", "api.example.com"},
		"plain http":              {"http://127.0.0.1:4318/v1/traces", "127.0.0.1:4318"},
		"empty":                   {"", ""},
		"not a URL at all":        {"pk-lf-dg74:sk-lf-dg74", ""},
		"wrong scheme":            {"ftp://api.example.com/v1/traces", ""},
		"scheme with no host":     {"https:///v1/traces", ""},
		"control character":       {"http://a\x7f.example.com", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := EndpointHost(tc.raw)
			if got != tc.want {
				t.Errorf("EndpointHost() = %q, want %q", got, tc.want)
			}
			for _, forbidden := range []string{"/", "pw", "user", "sk-lf-secret", "v1"} {
				if got != "" && strings.Contains(got, forbidden) {
					t.Errorf("EndpointHost() = %q, which carries %q", got, forbidden)
				}
			}
		})
	}
}
