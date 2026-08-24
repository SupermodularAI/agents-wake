//go:build remote

// The third member of the secrets boundary internal/config owns, present only
// under //go:build remote.
//
// It holds a third-party-issued credential — the first secret this project
// stores that did not originate on this machine — and that is why it is a file
// of its own rather than a field somewhere existing. It has a lifecycle nothing
// else here has: the far end can revoke it at any time, and the user replaces it
// without any of wake's own state changing. Folding it into repo-salt would tie
// a rotation the far end forces to the one file that must never be regenerated,
// and folding it into projects.json would put it in the data root, which
// ADR-0014 makes safe to delete.
//
// It is never in config.toml. That is the file users are asked to paste into a
// bug report (ADR-0019 §4), so a secret that can reach it is a secret that will
// (ADR-0028).
//
// The package comment in config.go says "the two sensitive files". In the
// default build that is exactly true, and it is a shared untagged file this
// build-tagged change does not edit; the boundary this file joins is stated
// here instead.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
)

// remoteAuthFileName is the store's name under the config root. The extension
// matches the encoding, as every other name in this package does — projects.json
// is JSON and repo-salt is raw bytes.
//
// It is spelled here and once more as a literal in boundary_test.go's
// confinedFileNames, because that test is untagged and cannot see this constant.
// TestRemoteAuthFileNameIsConfined pins the two together so they cannot drift.
const remoteAuthFileName = "remote-auth.json"

// remoteAuthVersion is the format version stamped on every write. A future
// format read as this one would be silently wrong in the worst available way: a
// credential posted to whatever the misread endpoint field happened to yield. An
// unrecognised version stops the read instead — the same rule projects.json
// follows, for the same reason.
const remoteAuthVersion = 1

// remoteAuthFileMode is the mode the store is created with. It holds a secret,
// so the mode is a stated property of this file rather than a property of the
// process umask, and checkSensitiveFile refuses anything looser on the way back
// in.
const remoteAuthFileMode = fs.FileMode(0o600)

// EnvRemoteAuthorization overrides the stored credential — and only the
// credential — whenever it is set. The endpoint and the enabled flag still come
// from the file: a credential with no destination is not a state any decision
// provides for.
//
// It carries the same colon-joined public:secret pair the stored `credential`
// field carries, not a pre-encoded Authorization header value; the HTTP Basic
// encoding happens at delivery, in internal/remote. Naming the variable after
// the header it ends up in is ADR-0028's spelling, not a statement about its
// contents.
//
// The stored value is never overwritten by an env-var read, so removing the
// variable reverts to whatever was last written to disk. It exists for CI and
// for anyone who prefers no secret on disk at all.
//
// The tradeoff, stated here so it is not rediscovered as a bug report
// (ADR-0028 § Consequences): a detached flush child spawned from a trigger does
// not inherit a shell-exported variable, so relying on this for background
// delivery means setting it in the trigger's own environment and not just in the
// user's shell.
const EnvRemoteAuthorization = "WAKE_REMOTE_AUTHORIZATION"

// The two ways an endpoint can be one this build refuses to store, and the one
// way a store can be a version it refuses to read.
//
// They are sentinels phrased as the tail of a sentence — the construction
// errNotARegularFile and errPathNotAbsolute already use — so a caller names the
// field's role and this file names the fault, and neither half quotes the value.
// The rejected endpoint is not echoed and url.Parse's own error is never
// wrapped, because it embeds the URL it failed on.
var (
	errEndpointNotHTTP        = errors.New("is not an absolute http:// or https:// URL")
	errEndpointRequiredWhenOn = errors.New("must be set before remote delivery can be enabled")
	errRemoteAuthWrongVersion = errors.New("is a version this build does not read")
)

// RemoteAuth is where records are delivered, whether they are, and what
// authorises the delivery.
//
// The three are one unit with one lifecycle: `remote set` writes them together,
// and a credential without its destination — or a destination without its
// credential — is not a state worth being able to represent. That is why there
// is one file and one publication rather than three.
//
// String redacts both secrets, so the struct cannot leak through a %v in an
// error, a log line or a debug print. Nothing here is a repository path or a
// label; the field list is asserted by TestRemoteAuthCarriesNoPathOrLabelField
// so a field added later has to be justified there (ADR-0007).
type RemoteAuth struct {
	// Endpoint is the OTLP/HTTP JSON traces endpoint records are posted to
	// (ADR-0027). Absolute, http:// or https://, validated on the way in.
	Endpoint string
	// Enabled is whether delivery happens at all. False with an endpoint set is
	// a legitimate state — it is how delivery is turned off without discarding
	// where it was going.
	Enabled bool
	// Credential is the colon-joined public:secret pair the far end issued. It
	// is the one value in this package that did not originate on this machine.
	Credential string
}

// String reports what the store holds without reporting either secret.
//
// This is the mechanical half of ADR-0028's "never echo what was read". Every
// error path in this file is written not to carry a value, but a %v of the
// struct somewhere else entirely is the leak nobody reviews for — so the type
// itself refuses, and presence is reported instead so the redaction is still
// useful in a bug report.
func (a RemoteAuth) String() string {
	return fmt.Sprintf("RemoteAuth{enabled:%t, endpoint:%s, credential:%s}",
		a.Enabled, presence(a.Endpoint), presence(a.Credential))
}

// presence renders whether a value is there, and nothing about what it is.
func presence(s string) string {
	if s == "" {
		return "unset"
	}
	return "set"
}

// remoteAuthPath is where the store lives: under the config root, beside
// repo-salt, because it is user configuration that survives deleting the data
// root (ADR-0028).
//
// Composed here rather than added to Paths. Paths is the surface other packages
// see and the list `init` discloses under ADR-0010, and a field on it would be a
// build-tagged field on an untagged struct — which is how the default build ends
// up disclosing a file it can never have.
func remoteAuthPath(p Paths) string {
	return filepath.Join(p.ConfigDir, remoteAuthFileName)
}

// remoteAuthFile is the on-disk form.
type remoteAuthFile struct {
	Version    int    `json:"version"`
	Endpoint   string `json:"endpoint"`
	Enabled    bool   `json:"enabled"`
	Credential string `json:"credential"`
}

// LoadRemoteAuth reads the store and applies the environment override.
//
// A missing store is not an error: a fresh install has none, and asking whether
// delivery is on must not write one — the rule a missing config.toml and a
// missing project table already follow. The answer is the zero value, which
// means delivery is off.
//
// Everything else fails closed. The file's type and mode are checked before it
// is opened, because the file holds a credential: a symlink standing in for it
// is a redirection that would read — and on the next publish, write — wherever
// it points, and a mode looser than 0600 means anyone else on the machine
// already has the credential. A file that does not parse is an error rather than
// an empty store, because an empty store silently stops delivering, and it is
// never rewritten from a failed parse: those bytes are the only copy of the
// credential there is.
//
// No refusal names the file, the endpoint or the credential. The role is a fixed
// literal here, the fault is a sentinel carrying no value, and a decode failure
// goes through parseFailure — the decoder's own message embeds the offending
// bytes, and the bytes here are the credential (plan §4.2).
func LoadRemoteAuth(p Paths) (RemoteAuth, error) {
	path := remoteAuthPath(p)

	if err := checkSensitiveFile(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return RemoteAuth{}, fmt.Errorf("the remote credential store %w", err)
		}
		return withEnvCredential(RemoteAuth{}), nil
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// The race between the check and the read: the store was removed in
		// between, which is the same answer as never having had one.
		return withEnvCredential(RemoteAuth{}), nil
	}
	if err != nil {
		// Not wrapped with a message: fs.ErrNotExist is the control-flow signal
		// above, and os.ReadFile's error already names the file for the
		// remaining cases, which never reach a user.
		return RemoteAuth{}, err
	}

	var stored remoteAuthFile
	if err := json.Unmarshal(raw, &stored); err != nil {
		return RemoteAuth{}, fmt.Errorf("the remote credential store holds %s", parseFailure(err))
	}
	if stored.Version != remoteAuthVersion {
		return RemoteAuth{}, fmt.Errorf("the remote credential store is version %d; this build reads version %d: %w",
			stored.Version, remoteAuthVersion, errRemoteAuthWrongVersion)
	}

	return withEnvCredential(RemoteAuth{
		Endpoint:   stored.Endpoint,
		Enabled:    stored.Enabled,
		Credential: stored.Credential,
	}), nil
}

// withEnvCredential applies EnvRemoteAuthorization to a value read from disk.
//
// Set wins whole: the environment's credential replaces the stored one rather
// than being blended with it, nothing else in the struct is affected, and
// nothing is written back. Trimmed for the reason ResolvePaths trims WAKE_DIR —
// unset, "" and a stray newline all mean the same thing — so clearing the
// variable reverts to the file rather than delivering with an empty credential.
func withEnvCredential(a RemoteAuth) RemoteAuth {
	if v := strings.TrimSpace(os.Getenv(EnvRemoteAuthorization)); v != "" {
		a.Credential = v
	}
	return a
}

// SetRemoteAuth validates and publishes the store.
//
// Validation happens before anything touches disk, so a rejected endpoint leaves
// no file behind and no half-configured state to explain. ADR-0027 makes
// OTLP/HTTP JSON the only integration surface, so an endpoint that is not
// absolute http:// or https:// is a credential posted somewhere no decision
// permits. The check is deliberately scheme and host only: no reachability
// probe, no assertion about the OTLP path, nothing that reads the network.
//
// The zero value is accepted and is a legitimate store — it is how delivery is
// turned off — but Enabled without an endpoint is not, because that state can
// only fail later, in a background flush nobody is watching.
//
// One publication for all three fields, through atomicfile: a reader sees the
// old store or the complete new one, the file is chmodded before the rename so
// it is never briefly readable by anyone else, and a successful return means the
// bytes are durable. No second permission check is written here —
// checkSensitiveFile owns that on the way back in.
func SetRemoteAuth(p Paths, a RemoteAuth) error {
	if a.Enabled && a.Endpoint == "" {
		return fmt.Errorf("the remote endpoint %w", errEndpointRequiredWhenOn)
	}
	if a.Endpoint != "" && !isHTTPEndpoint(a.Endpoint) {
		return fmt.Errorf("the remote endpoint %w", errEndpointNotHTTP)
	}

	data, err := json.MarshalIndent(remoteAuthFile{
		Version:    remoteAuthVersion,
		Endpoint:   a.Endpoint,
		Enabled:    a.Enabled,
		Credential: a.Credential,
	}, "", "  ")
	if err != nil {
		// The struct is four scalars, so this cannot fail — and the message
		// still names no field, because a marshal error embeds the value it
		// choked on.
		return fmt.Errorf("encoding the remote credential store: %w", err)
	}
	data = append(data, '\n')

	return atomicfile.Publish(remoteAuthPath(p), data, remoteAuthFileMode)
}

// isHTTPEndpoint reports whether raw is an absolute http:// or https:// URL with
// a host. url.Parse's error is discarded rather than returned: it embeds the URL
// it failed on, and the caller reports the fault without the value.
func isHTTPEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
