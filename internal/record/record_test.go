package record

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"
)

// hostileIdentifiers is this package's corpus of source values that must never
// reach a persisted field. It is declared here and again in the Claude Code
// adapter's tests: ADR-0007 requires a hostile-payload corpus per adapter, and a
// single shared one would let a new adapter inherit coverage it never ran.
var hostileIdentifiers = []string{
	"/usr/local/bin", "usr/local/bin", "./relative", "../secrets", "a/../b",
	"~/.ssh/id_rsa", `C:\Windows\System32`, "C:temp", `C:/Users/me`, `\\server\share`,
	`back\slash`, "contains space", "tab\there", "new\nline", "trailing/", "/", "..", ".hidden", "",
}

func TestRecordContainsNoPlainStrings(t *testing.T) {
	typeOfRecord := reflect.TypeFor[Record]()
	for i := range typeOfRecord.NumField() {
		field := typeOfRecord.Field(i)
		if field.Type.Kind() == reflect.String && field.Type.Name() == "string" {
			t.Fatalf("%s is an unconstrained string field", field.Name)
		}
	}
}

func TestValidate(t *testing.T) {
	record := validRecord()
	if err := Validate(record); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	record.Name = "contains space"
	if err := Validate(record); err == nil {
		t.Fatal("Validate() accepted a name with whitespace")
	}
}

func TestOutcomeHasNoSuccessZeroValue(t *testing.T) {
	if Outcome("") == OutcomeOK {
		t.Fatal("zero Outcome must not mean success")
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	record := validRecord()
	first, err := Marshal(record)
	if err != nil {
		t.Fatalf("Marshal() first error = %v", err)
	}
	second, err := Marshal(record)
	if err != nil {
		t.Fatalf("Marshal() second error = %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("Marshal() differs: %s != %s", first, second)
	}
}

func TestDeriveEventID(t *testing.T) {
	harness := Identifier("claude-code")
	source := Identifier("source-event-1")
	first := DeriveEventID(harness, source)
	second := DeriveEventID(harness, source)
	if first != second {
		t.Fatal("DeriveEventID() is not deterministic")
	}
}

func validRecord() Record {
	outcome := OutcomeOK
	return Record{
		SchemaVersion: SchemaVersion,
		EventID:       DeriveEventID("claude-code", "source-event-1"),
		Timestamp:     time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC),
		Harness:       "claude-code",
		SessionID:     "session-1",
		Repo:          "0123456789abcdef0123456789abcdef",
		Kind:          KindSkill,
		Name:          "review",
		Invoker:       InvokerModel,
		Outcome:       &outcome,
	}
}

func TestValidateRejectsPathShapedRequiredFields(t *testing.T) {
	setters := map[string]func(*Record, string){
		"Name":      func(r *Record, v string) { r.Name = Identifier(v) },
		"SessionID": func(r *Record, v string) { r.SessionID = Identifier(v) },
		"Harness":   func(r *Record, v string) { r.Harness = Identifier(v) },
	}
	for field, set := range setters {
		for _, value := range hostileIdentifiers {
			candidate := validRecord()
			set(&candidate, value)
			if err := Validate(candidate); err == nil {
				t.Errorf("Validate() accepted %s = %q", field, value)
			}
		}
	}
}

func TestValidateRejectsPathShapedOptionalFields(t *testing.T) {
	setters := map[string]func(*Record, string){
		"Package":        func(r *Record, v string) { r.Package = Identifier(v) },
		"ViaSkill":       func(r *Record, v string) { r.ViaSkill = Identifier(v) },
		"ViaAgent":       func(r *Record, v string) { r.ViaAgent = Identifier(v) },
		"Model":          func(r *Record, v string) { r.Model = Identifier(v) },
		"Effort":         func(r *Record, v string) { r.Effort = Identifier(v) },
		"HarnessVersion": func(r *Record, v string) { r.HarnessVersion = Version(v) },
		"PackageVersion": func(r *Record, v string) { r.PackageVersion = Version(v) },
	}
	for field, set := range setters {
		for _, value := range hostileIdentifiers {
			if value == "" {
				continue
			}
			candidate := validRecord()
			set(&candidate, value)
			if err := Validate(candidate); err == nil {
				t.Errorf("Validate() accepted %s = %q", field, value)
			}
		}
	}
}

func TestValidateAcceptsRealClaudeCodeFieldValues(t *testing.T) {
	candidate := validRecord()
	candidate.Name = "mcp__claude-in-chrome__browser_batch"
	candidate.Package = "atlassian"
	candidate.ViaSkill = "pr-review"
	candidate.ViaAgent = "sdlc-check-architecture"
	candidate.Model = "claude-sonnet-4-5-20250929"
	candidate.Effort = "high"
	candidate.HarnessVersion = "2.1.3-beta+build"
	candidate.SessionID = "019bf1e2-4c7a-7b31-9d55-6f0a1b2c3d4e"
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	candidate.Name = "plugin:plugin-skill"
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate() rejected a plugin-qualified name: %v", err)
	}
}

func TestValidateRejectsANonSlugHarness(t *testing.T) {
	for _, value := range []string{"Claude_Code", "claude code", "claude/code", "claude.code"} {
		candidate := validRecord()
		candidate.Harness = Identifier(value)
		if err := Validate(candidate); err == nil {
			t.Errorf("Validate() accepted Harness = %q", value)
		}
	}
	candidate := validRecord()
	candidate.Harness = "claude-code"
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate() rejected a harness slug: %v", err)
	}
}

func TestBoundedIdentifierRejectsThePathCorpus(t *testing.T) {
	for _, value := range hostileIdentifiers {
		if _, err := BoundedIdentifier(value); err == nil {
			t.Errorf("BoundedIdentifier(%q) returned no error", value)
		}
	}
}

func TestBoundedIdentifierAcceptsRealFormats(t *testing.T) {
	for _, value := range []string{"Bash", "mcp__atlassian__search", "plugin:plugin-skill", "claude-sonnet-4-5-20250929", "pr-review"} {
		got, err := BoundedIdentifier(value)
		if err != nil {
			t.Errorf("BoundedIdentifier(%q) error = %v", value, err)
			continue
		}
		if string(got) != value {
			t.Errorf("BoundedIdentifier(%q) = %q", value, got)
		}
	}
}

func TestBoundedTokenRejectsScopeSyntaxAndPaths(t *testing.T) {
	for _, value := range append([]string{"plugin:skill", "a@b", "a+b"}, hostileIdentifiers...) {
		if _, err := BoundedToken(value); err == nil {
			t.Errorf("BoundedToken(%q) returned no error", value)
		}
	}
	for _, value := range []string{"019bf1e2-4c7a-7b31-9d55-6f0a1b2c3d4e", "session-1"} {
		got, err := BoundedToken(value)
		if err != nil {
			t.Errorf("BoundedToken(%q) error = %v", value, err)
			continue
		}
		if string(got) != value {
			t.Errorf("BoundedToken(%q) = %q", value, got)
		}
	}
}

func TestBoundedVersionAcceptsVersionsAndRejectsPaths(t *testing.T) {
	for _, value := range []string{"1.0.0", "2.1.3-beta+build"} {
		got, err := BoundedVersion(value)
		if err != nil {
			t.Errorf("BoundedVersion(%q) error = %v", value, err)
			continue
		}
		if string(got) != value {
			t.Errorf("BoundedVersion(%q) = %q", value, got)
		}
	}
	for _, value := range []string{"../1.0.0", "1.0/0", "1 0", ""} {
		if _, err := BoundedVersion(value); err == nil {
			t.Errorf("BoundedVersion(%q) returned no error", value)
		}
	}
}

func TestDerivedNameLeavesUnscopedIdentitiesUnchanged(t *testing.T) {
	for _, value := range []string{"pr-review", "plugin:plugin-skill", "atlassian", "mcp__a__b"} {
		got, err := testNamer().DerivedName(value)
		if err != nil {
			t.Errorf("DerivedName(%q) error = %v", value, err)
			continue
		}
		if string(got) != value {
			t.Errorf("DerivedName(%q) = %q", value, got)
		}
	}
}

func TestDerivedNameReplacesAPathScopeWithADigest(t *testing.T) {
	got, err := testNamer().DerivedName("apps/web:deploy")
	if err != nil {
		t.Fatalf("DerivedName() error = %v", err)
	}
	if !ValidName(got) {
		t.Fatalf("DerivedName() produced an unpersistable name: %q", got)
	}
	if !strings.HasPrefix(string(got), "scope-") || !strings.HasSuffix(string(got), ":deploy") {
		t.Fatalf("DerivedName() = %q, want scope-<digest>:deploy", got)
	}
	for _, fragment := range []string{"apps", "web", "/", `\`} {
		if strings.Contains(string(got), fragment) {
			t.Fatalf("DerivedName() = %q, retains %q", got, fragment)
		}
	}
}

func TestDerivedNameIsInjectiveAcrossScopes(t *testing.T) {
	sources := []string{"apps/web:deploy", "other/web:deploy", "apps-web:deploy", "deploy"}
	seen := map[Identifier]string{}
	for _, value := range sources {
		got, err := testNamer().DerivedName(value)
		if err != nil {
			t.Fatalf("DerivedName(%q) error = %v", value, err)
		}
		seen[got] = value
	}
	if len(seen) != len(sources) {
		t.Fatalf("DerivedName() collapsed distinct scopes: %v", seen)
	}
}

func TestDerivedNameRejectsPathsThatAreNotScopedReferences(t *testing.T) {
	for _, value := range []string{
		"usr/local/bin", "/etc/passwd", "/etc/passwd:x", "../x:y", "./x:y",
		"a/../b:y", `C:/Users/me`, "~/x:y", "a:/b", "//x:y", "x/:y", "x/y:z/w",
		"a/b:", "apps/web:",
	} {
		if _, err := testNamer().DerivedName(value); err == nil {
			t.Errorf("DerivedName(%q) returned no error", value)
		}
	}
}

func TestValidNameAndValidHarnessAgreeWithValidate(t *testing.T) {
	if ValidName("usr/local/bin") {
		t.Fatal("ValidName() accepted a path")
	}
	if !ValidName("pr-review") {
		t.Fatal("ValidName() rejected a primitive name")
	}
	if !ValidHarness("claude-code") {
		t.Fatal("ValidHarness() rejected a harness slug")
	}
	if ValidHarness("Claude Code") {
		t.Fatal("ValidHarness() accepted a non-slug harness")
	}
}

func TestScopeDigestIsKeyedRatherThanAPlainHash(t *testing.T) {
	got, err := testNamer().DerivedName("apps/web:deploy")
	if err != nil {
		t.Fatalf("DerivedName() error = %v", err)
	}

	// The digest an unkeyed implementation would have persisted. A scope is a
	// handful of directory names, so a plain hash of one is recoverable from a
	// wordlist — and the spool is what leaves the machine under the remote build
	// tag. This is the value that must never appear.
	sum := sha256.Sum256([]byte("apps/web"))
	unkeyed := Identifier("scope-" + hex.EncodeToString(sum[:])[:scopeDigestLen] + ":deploy")
	if got == unkeyed {
		t.Fatalf("DerivedName() persisted an unkeyed digest of the scope: %q", got)
	}

	other, err := NewNamer([]byte("a different per-machine salt")).DerivedName("apps/web:deploy")
	if err != nil {
		t.Fatalf("DerivedName() under a second key error = %v", err)
	}
	if other == got {
		t.Fatalf("DerivedName() ignored the key: both keys produced %q", got)
	}

	again, err := testNamer().DerivedName("apps/web:deploy")
	if err != nil {
		t.Fatalf("second DerivedName() error = %v", err)
	}
	if again != got {
		t.Fatalf("DerivedName() is not stable under one key: %q then %q", got, again)
	}
}

func TestNamerWithoutAKeyRefusesAScopedReference(t *testing.T) {
	var keyless Namer
	if _, err := keyless.DerivedName("apps/web:deploy"); err == nil {
		t.Fatal("a keyless Namer digested a scope")
	}
	got, err := keyless.DerivedName("pr-review")
	if err != nil || got != "pr-review" {
		t.Fatalf("keyless DerivedName(%q) = %q, %v", "pr-review", got, err)
	}
}

func TestDerivedNameRefusesAVerbatimDigestShape(t *testing.T) {
	scoped, err := testNamer().DerivedName("apps/web:deploy")
	if err != nil {
		t.Fatalf("DerivedName() error = %v", err)
	}
	if _, err := testNamer().DerivedName(string(scoped)); err == nil {
		t.Fatalf("DerivedName() accepted a verbatim value shaped like its own output: %q", scoped)
	}
	// The refusal is the digest shape only, so a real primitive that merely starts
	// with the same word is still collected.
	for _, value := range []string{"scope-web:deploy", "scope-3b4e3efd29cd", "scoped:deploy"} {
		if _, err := testNamer().DerivedName(value); err != nil {
			t.Errorf("DerivedName(%q) error = %v", value, err)
		}
	}
}

// testNamer keys the scope digest for tests. Production keys it with a subkey of
// the per-machine salt (config.Repos.NameKey).
func testNamer() Namer { return NewNamer([]byte("test scope key")) }
