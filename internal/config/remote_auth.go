// The third member of the secrets boundary internal/config owns.
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
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
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
// boundary_test.go's confinedFileNames takes this constant directly, so the
// name is spelled once and cannot drift (ADR-0028).
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

// The three ways an endpoint can be one this build refuses to store, and the one
// way a store can be a version it refuses to read.
//
// They are sentinels phrased as the tail of a sentence — the construction
// errNotARegularFile and errPathNotAbsolute already use — so a caller names the
// field's role and this file names the fault, and neither half quotes the value.
// The rejected endpoint is not echoed and url.Parse's own error is never
// wrapped, because it embeds the URL it failed on.
//
// errEndpointCleartext names the remedy as well as the rule, because a refusal a
// user cannot act on is a refusal that gets worked around. The command it names
// is a string and not a call: internal/cli depends on this package and never the
// reverse (ADR-0001).
var (
	errEndpointNotHTTP        = errors.New("is not an absolute http:// or https:// URL")
	errEndpointCleartext      = errors.New(`must be an https:// URL unless its host is a loopback address, because http:// to any other host puts the credential on the network in the clear; run "wake remote set" with an https:// URL`)
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
	// (ADR-0027). Absolute; https://, or http:// only to a loopback host,
	// validated on the way in and again on the way out — validateRemoteAuth is
	// the one spelling of that rule, so a value this build never wrote cannot
	// arrive through the file.
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
// see and the list `init` discloses under ADR-0010, and a file the user has not
// created — a fresh install has no remote-auth store at all — is not a path to
// disclose.
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
// It is what every consumer of the credential calls. The write paths in this
// file deliberately do not: see storedRemoteAuth.
func LoadRemoteAuth(p Paths) (RemoteAuth, error) {
	auth, err := storedRemoteAuth(p)
	if err != nil {
		return RemoteAuth{}, err
	}
	return withEnvCredential(auth), nil
}

// storedRemoteAuth is what the file holds, before EnvRemoteAuthorization is
// applied.
//
// Every write path reads through it rather than through LoadRemoteAuth, because
// writing back a value LoadRemoteAuth produced would persist the environment's
// credential to disk — the one thing ADR-0028 says that override must never do,
// and a leak that would leave a store looking entirely ordinary.
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
// The config root is checked before the file is touched, for the reason
// OpenRepos gives: the mode of a file is only as strong as the directory holding
// it. checkSensitiveFile tests type and mode and never ownership, so in a
// directory anyone else can write, another local user can rename their own 0600
// file over this one — their credential, their endpoint — and every file-level
// check would still pass. A directory that does not exist yet is not a fault.
//
// The endpoint is validated here as well as in SetRemoteAuth. The write path
// only governs stores this build wrote; a store it did not write is the one worth
// checking, and ADR-0027 makes OTLP/HTTP the only surface a credential may be
// posted to. The scheme-and-host rule is part of what is re-checked on the way
// out, so a store holding a cleartext endpoint to a remote host is refused here
// rather than delivered to.
//
// No refusal names the file, the endpoint or the credential. The role is a fixed
// literal here, the fault is a sentinel carrying no value, and a decode failure
// goes through parseFailure — the decoder's own message embeds the offending
// bytes, and the bytes here are the credential (plan §4.2).
//
// Everything above the endpoint rule is decodeRemoteAuth's, which exists for one
// caller; this is still the entry point every reader and every write path but
// SetRemoteEndpoint goes through, and the one that applies the rule.
func storedRemoteAuth(p Paths) (RemoteAuth, error) {
	auth, err := decodeRemoteAuth(p)
	if err != nil {
		return RemoteAuth{}, err
	}
	// Validated here rather than at the caller, because the override carries no
	// endpoint: what is checked is what the file says, which is the whole of what
	// decides where a credential goes.
	if err := validateRemoteAuth(auth); err != nil {
		return RemoteAuth{}, fmt.Errorf("the remote endpoint in the credential store %w", err)
	}
	return auth, nil
}

// decodeRemoteAuth is storedRemoteAuth without the endpoint rule: every refusal
// about the file itself — the config root, the type and mode, the bytes, the
// version — and none about where the endpoint points.
//
// The two are separate for exactly one caller. SetRemoteEndpoint's whole job is
// to replace the endpoint, so a stored endpoint the rule refuses is what it is
// being run to fix rather than a reason to refuse it; every other caller reads
// through storedRemoteAuth and gets the rule. The split is deliberately at the
// endpoint and nowhere else: a store whose version, mode or bytes this build
// cannot trust is never silently overwritten, because those bytes are the only
// copy of the credential there is.
func decodeRemoteAuth(p Paths) (RemoteAuth, error) {
	path := remoteAuthPath(p)

	if err := checkStateDir(p.ConfigDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return RemoteAuth{}, fmt.Errorf("the configuration directory %w", err)
	}

	if err := checkSensitiveFile(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return RemoteAuth{}, fmt.Errorf("the remote credential store %w", err)
		}
		return RemoteAuth{}, nil
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// The race between the check and the read: the store was removed in
		// between, which is the same answer as never having had one.
		return RemoteAuth{}, nil
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

	return RemoteAuth{
		Endpoint:   stored.Endpoint,
		Enabled:    stored.Enabled,
		Credential: stored.Credential,
	}, nil
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
// Validation happens before anything is written, so a rejected endpoint leaves
// no file behind and no half-configured state to explain. The rule itself lives
// in validateRemoteAuth, which the read path applies too. ADR-0027 makes
// OTLP/HTTP JSON the only integration surface, so an endpoint that is not
// absolute http:// or https:// is a credential posted somewhere no decision
// permits — and an http:// endpoint to anything but a loopback host is that
// credential posted in the clear, on every batch, since it rides in the
// Authorization header. The check is deliberately scheme and host only: no
// reachability probe, no assertion about the OTLP path, nothing that reads the
// network — the loopback test reads the literal host as written and consults no
// resolver.
//
// The config root is checked too, for the reason OpenRepos gives: publishing a
// credential at 0600 into a directory anyone else can write into is publishing a
// file they can replace, and the mode says nothing about that. A directory that
// does not exist yet is not a fault — atomicfile creates it at 0700.
//
// One publication for all three fields, through atomicfile: a reader sees the
// old store or the complete new one, the file is chmodded before the rename so
// it is never briefly readable by anyone else, and a successful return means the
// bytes are durable. No second permission check is written here —
// checkSensitiveFile owns that on the way back in.
func SetRemoteAuth(p Paths, a RemoteAuth) error {
	if err := validateRemoteAuth(a); err != nil {
		return fmt.Errorf("the remote endpoint %w", err)
	}
	if err := checkStateDir(p.ConfigDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the configuration directory %w", err)
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

// RemoteEndpointHost reports the host and port of the configured endpoint and
// nothing else about it — no scheme, no path, no query, no userinfo, and never
// the credential. An empty result means no endpoint is configured.
//
// It is what `remote status` prints, and ADR-0029 is the decision that lets it:
// ADR-0028's "never echo what was read" is narrowed by a carve-out ADR-0031
// revises to exactly two consumers, of which this is one — a bare host, on
// stdout, in answer to a command a human just typed. The host is
// the part of a URL that is not a secret; the path, the query and the userinfo
// are where one hides, which is why url.Host is the whole of what this returns.
//
// It is a function here rather than a field on remote.Status deliberately. That
// struct is what `doctor` renders and people paste into issues, so it carries
// presence only — and keeping the value on a separate entry point is what stops
// the pasteable surface from growing it by accident (ADR-0029, ADR-0007).
func RemoteEndpointHost(p Paths) (string, error) {
	auth, err := storedRemoteAuth(p)
	if err != nil {
		return "", err
	}
	return EndpointHost(auth.Endpoint), nil
}

// EndpointHost reports the host and port of an endpoint and nothing else about
// it — no scheme, no path, no query, no userinfo, and never a credential. An
// empty result means there is no endpoint, or that the value is not one this
// build would store.
//
// It is the one derivation of a printable host in this project. ADR-0029 carved
// a bare host out of ADR-0028's never-echo rule for `remote status`; ADR-0031
// revises that carve-out to exactly two consumers — `remote status`, reading a
// stored endpoint through RemoteEndpointHost, and `remote set`'s interactive
// confirmation, showing a value in flight before it is written. Both call this,
// so the rule about what a host may carry is spelled once and cannot drift into
// a third shape.
//
// The endpoint rule is applied here as well as in validateRemoteAuth — the same
// predicate, so the two cannot drift — and that is what makes an empty result a
// usable refusal: a caller can decline a typed value before anything reaches the
// store, rather than confirming a destination the write path will reject.
// url.Parse's error is discarded for the reason endpointFault discards it — it
// embeds the URL it failed on.
func EndpointHost(raw string) string {
	if !isStorableEndpoint(raw) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// SetRemoteEndpoint writes a destination and its credential as one unit,
// preserving whatever on/off state the store already held.
//
// Rotating a credential on a paused endpoint must not silently resume delivery,
// for the same reason `remote off` must not clear the endpoint: the state the
// user put the machine in is theirs, and a command that quietly undoes it is the
// hostility ADR-0018 rejects (ADR-0028).
//
// It reads through decodeRemoteAuth rather than storedRemoteAuth, and it is the
// only caller that does. This is the one command whose whole job is to replace
// the endpoint, so a stored endpoint that fails the endpoint rule — a cleartext
// destination written by an older build, say — is precisely what it is being run
// to fix; refusing it here would leave an affected machine with no in-tool exit
// at all. Every other fault the store can have is still fatal, and the new value
// is validated in full by SetRemoteAuth before anything is written. The
// environment's credential cannot reach the file either, because this path does
// not go through LoadRemoteAuth.
func SetRemoteEndpoint(p Paths, endpoint, credential string) error {
	stored, err := decodeRemoteAuth(p)
	if err != nil {
		return err
	}
	return SetRemoteAuth(p, RemoteAuth{Endpoint: endpoint, Enabled: stored.Enabled, Credential: credential})
}

// SetRemoteEnabled turns delivery on or off without touching the endpoint or the
// credential.
//
// Already-in-that-state is a no-op that writes nothing, so `remote off` on a
// machine with nothing configured creates no file: a store holding a secret's
// shape where there is no secret is the sort of thing a later reader treats as
// evidence.
func SetRemoteEnabled(p Paths, enabled bool) error {
	stored, err := storedRemoteAuth(p)
	if err != nil {
		return err
	}
	if stored.Enabled == enabled {
		return nil
	}
	stored.Enabled = enabled
	return SetRemoteAuth(p, stored)
}

// validateRemoteAuth reports whether a value is one this build will store and
// act on. One function for both directions: the read path and the write path
// enforce the same invariant, and two spellings of it would drift into a store
// that can be read back but never written, or written but never read.
//
// The zero value is valid and is a legitimate store — it is how delivery is
// turned off — but Enabled without an endpoint is not, because that state can
// only fail later, in a background flush nobody is watching. The returned
// sentinel names the fault and never the value; the caller names the role.
func validateRemoteAuth(a RemoteAuth) error {
	if a.Enabled && a.Endpoint == "" {
		return errEndpointRequiredWhenOn
	}
	if a.Endpoint != "" {
		if err := endpointFault(a.Endpoint); err != nil {
			return err
		}
	}
	return nil
}

// endpointFault reports why raw is not a destination this build will store, or
// nil when it is one.
//
// Two faults, in the order they can be decided: not an absolute http:// or
// https:// URL with a host at all, and — for http:// — a host that is not
// loopback, which would put the Authorization header on the network in the clear
// on every batch.
//
// url.Parse's error is discarded rather than returned: it embeds the URL it
// failed on, and the caller reports the fault without the value.
func endpointFault(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errEndpointNotHTTP
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errEndpointNotHTTP
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return errEndpointCleartext
	}
	return nil
}

// isStorableEndpoint is endpointFault as a predicate, for the one caller that
// has no error to return.
func isStorableEndpoint(raw string) bool { return endpointFault(raw) == nil }

// isLoopbackHost reports whether host — the literal host of a URL, with any port
// and any IPv6 brackets already stripped by url.URL.Hostname — is a loopback
// destination.
//
// It is decided from what was written and never from a resolver: a name that
// resolves to loopback today may not tomorrow, and a validator that reads the
// network behaves differently on two machines. That is also why net/netip is
// used rather than net — netip.ParseAddr is a pure value parse with no resolver
// and no I/O, so the property is visible in the import list.
//
// The rule is the literal label "localhost", matched case-insensitively, and any
// address netip calls loopback: all of 127.0.0.0/8, ::1, and the IPv4-in-IPv6
// form. A wider match — foo.localhost, localhost.localdomain, a trailing dot —
// is a wider surface for no benefit, so those are refused.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}
