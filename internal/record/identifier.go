package record

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
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
)

// errUnsafeIdentifier is deliberately valueless: a rejected value is transcript
// content and must never be quoted into a diagnostic (plan §4.2).
var errUnsafeIdentifier = errors.New("unsafe identifier")

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

// DerivedName returns the persisted name for a source identity whose syntax is
// wider than the name domain — a plugin reference ("plugin:atlassian:cloud") or a
// directory-scoped reference ("apps/web:deploy").
//
// A scope that is a path is replaced by a short digest of itself: the scope is
// repository content and must never be persisted (plan §3.4, ADR-0007), while the
// digest keeps two same-named primitives in different scopes from collapsing onto
// one name. Everything else is returned verbatim, and any other value carrying a
// separator is refused. Reading a wider syntax is not persisting it.
func DerivedName(value string) (Identifier, error) {
	value = strings.TrimSpace(value)
	if len(value) > maxIdentifier {
		return "", errUnsafeIdentifier
	}
	if !strings.Contains(value, "/") {
		return BoundedIdentifier(value)
	}
	scope, name, found := strings.Cut(value, ":")
	if !found || strings.ContainsAny(name, `/\`) || !pathScope(scope) {
		return "", errUnsafeIdentifier
	}
	return BoundedIdentifier(scopePrefix + scopeDigest(scope) + ":" + name)
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

func scopeDigest(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:])[:scopeDigestLen]
}
