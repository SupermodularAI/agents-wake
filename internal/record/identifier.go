package record

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/SupermodularAI/agents-wake/internal/keyeddigest"
)

const (
	maxIdentifier  = 128
	maxHarness     = 32
	scopeDigestLen = 12
	scopePrefix    = "scope-"
)

var (
	harnessPattern      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	namePattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]*$`)
	tokenPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	windowsDrivePattern = regexp.MustCompile(`^[A-Za-z]:`)
	// derivedScopePattern is the shape DerivedName produces for a scoped
	// reference. A source value already wearing it is refused, so a transcript
	// cannot hand-craft a name that collides with a real scope digest and merges
	// two distinct primitives into one metric. The pattern is the full digest
	// shape rather than the bare prefix, so a primitive genuinely named
	// "scope-something" is still collected.
	derivedScopePattern = regexp.MustCompile(fmt.Sprintf(`^%s[0-9a-f]{%d}:`, scopePrefix, scopeDigestLen))
)

// errUnsafeIdentifier is deliberately valueless: a rejected value is transcript
// content and must never be quoted into a diagnostic (plan §4.2).
var errUnsafeIdentifier = errors.New("unsafe identifier")

// errNoScopeKey is the refusal a Namer with no key returns for a scoped
// reference. It names no value for the same reason errUnsafeIdentifier does.
var errNoScopeKey = errors.New("no scope key")

// BoundedIdentifier returns an Identifier in the name domain — a primitive name,
// a package, a model, an effort level. It refuses every value that could
// represent a path or free text; a source identity with a wider syntax must go
// through DerivedName instead.
func BoundedIdentifier(value string) (Identifier, error) {
	identifier := Identifier(strings.TrimSpace(value))
	if !ValidName(identifier) {
		return "", errUnsafeIdentifier
	}
	return identifier, nil
}

// BoundedToken returns an Identifier in the opaque-token domain: a session id.
// The domain admits no scope separator, so a token can never be a qualified
// reference and never a path.
func BoundedToken(value string) (Identifier, error) {
	identifier := Identifier(strings.TrimSpace(value))
	if !validToken(identifier) {
		return "", errUnsafeIdentifier
	}
	return identifier, nil
}

// BoundedVersion returns a Version. Split from BoundedIdentifier because a
// version is its own domain: the previous code validated a version against the
// identifier grammar and cast the result, which is the domain confusion this
// change removes.
func BoundedVersion(value string) (Version, error) {
	version := Version(strings.TrimSpace(value))
	if !validVersion(version) {
		return "", errUnsafeIdentifier
	}
	return version, nil
}

// Namer derives the persisted name of a source identity whose syntax is wider
// than the name domain, and holds the key the scope part is digested under.
//
// The key is why this is a type rather than a function. A scoped reference's scope
// is a repository path fragment, and its digest is persisted into the spool — the
// same bytes that leave the machine when remote delivery is enabled (ADR-0027,
// ADR-0030). A plain hash of a path is not one-way: the input space is a handful
// of directory names, so it is recoverable from a wordlist. Keying it is the
// standard this project already applies to the one other path-derived value it
// persists, the repository id (config.Repos.NameKey, ADR-0019 §3, ADR-0020).
//
// The zero Namer has no key and refuses every scoped reference rather than
// falling back to a plain digest: a consent answer that could not be resolved
// must not widen what gets persisted (fail closed, plan §3.4).
type Namer struct{ key []byte }

// NewNamer returns a Namer that digests a scope under key.
func NewNamer(key []byte) Namer { return Namer{key: key} }

// DerivedName returns the persisted name for a source identity whose syntax is
// wider than the name domain — a plugin reference ("plugin:atlassian:cloud") or a
// directory-scoped reference ("apps/web:deploy").
//
// A scope that is a path is replaced by a keyed digest of itself: the scope is
// repository content and must never be persisted (plan §3.4, ADR-0007), while the
// digest keeps two same-named primitives in different scopes from collapsing onto
// one name. Everything else is returned verbatim, and any other value carrying a
// separator is refused. Reading a wider syntax is not persisting it.
func (n Namer) DerivedName(value string) (Identifier, error) {
	value = strings.TrimSpace(value)
	if len(value) > maxIdentifier || derivedScopePattern.MatchString(value) {
		return "", errUnsafeIdentifier
	}
	if !strings.Contains(value, "/") {
		return BoundedIdentifier(value)
	}
	scope, name, found := strings.Cut(value, ":")
	if !found || name == "" || strings.ContainsAny(name, `/\`) || !pathScope(scope) {
		return "", errUnsafeIdentifier
	}
	digest, err := n.scopeDigest(scope)
	if err != nil {
		return "", err
	}
	return BoundedIdentifier(scopePrefix + digest + ":" + name)
}

// ValidName reports whether v may be persisted in a name-domain field.
func ValidName(v Identifier) bool { return safeShape(string(v), namePattern, maxIdentifier) }

// ValidHarness reports whether v is a harness slug.
func ValidHarness(v Identifier) bool { return safeShape(string(v), harnessPattern, maxHarness) }

func validToken(v Identifier) bool { return safeShape(string(v), tokenPattern, maxIdentifier) }

func validVersion(v Version) bool { return safeShape(string(v), versionPattern, maxIdentifier) }

func validOptionalName(v Identifier) bool { return v == "" || ValidName(v) }

func validOptionalVersion(v Version) bool { return v == "" || validVersion(v) }

// safeShape states the empty-value, length and Windows-drive rules once so no
// value domain can drift from the others.
func safeShape(value string, pattern *regexp.Regexp, limit int) bool {
	return value != "" && len(value) <= limit && pattern.MatchString(value) && !windowsDrivePattern.MatchString(value)
}

// pathScope reports whether scope is a plain relative sequence of path elements,
// the only shape whose digest DerivedName is willing to persist.
func pathScope(scope string) bool {
	if scope == "" || strings.Contains(scope, `\`) || strings.HasPrefix(scope, "~") {
		return false
	}
	for _, element := range strings.Split(scope, "/") {
		if element == "" || element == "." || element == ".." {
			return false
		}
	}
	return true
}

// scopeDigest is HMAC-SHA256 of the scope under this Namer's key, hex-encoded and
// truncated to scopeDigestLen characters — the same construction the repository id
// uses (ADR-0019 §3, ADR-0020).
//
// Truncation is safe here in a way it would not be unkeyed: without the key there
// is nothing to enumerate against, so the width only has to make an accidental
// collision between two scopes in one repository implausible, not resist a
// pre-image search.
//
// A missing key is an error rather than a plain digest. The one thing this function
// must never do is fall back to a hash of the path.
func (n Namer) scopeDigest(scope string) (string, error) {
	if len(n.key) == 0 {
		return "", errNoScopeKey
	}
	return hex.EncodeToString(keyeddigest.Sum(n.key, []byte(scope)))[:scopeDigestLen], nil
}
