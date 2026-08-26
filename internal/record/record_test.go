package record

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

func TestEntrypointHasNoDefaultMember(t *testing.T) {
	for _, member := range []Entrypoint{EntrypointCLI, EntrypointSDKPython, EntrypointSDKCLI} {
		if Entrypoint("") == member {
			t.Fatalf("zero Entrypoint must not mean %q", member)
		}
	}
}

func TestValidateAcceptsEveryEntrypointMember(t *testing.T) {
	for _, member := range []Entrypoint{EntrypointCLI, EntrypointSDKPython, EntrypointSDKCLI} {
		candidate := validRecord()
		candidate.Entrypoint = member
		if err := Validate(candidate); err != nil {
			t.Errorf("Validate() rejected Entrypoint = %q: %v", member, err)
		}
	}
}

func TestValidateAcceptsAnAbsentEntrypoint(t *testing.T) {
	candidate := validRecord()
	if candidate.Entrypoint != "" {
		t.Fatalf("validRecord() presets Entrypoint = %q", candidate.Entrypoint)
	}
	// Both halves of "absence is omission" (C5), asserted directly rather than
	// through Marshal's internal validation: absence validates, and it leaves no key.
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate() rejected an absent entrypoint: %v", err)
	}
	encoded, err := Marshal(candidate)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), "entrypoint") {
		t.Fatalf("Marshal() emitted an absent entrypoint: %s", encoded)
	}
}

func TestValidateRejectsAnUnknownEntrypoint(t *testing.T) {
	values := []string{"sdk-py", "sdk-cli", "sdk-ts", "CLI", "cli ", "vscode", "sdk_py"}
	for _, value := range hostileIdentifiers {
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	for _, value := range values {
		candidate := validRecord()
		candidate.Entrypoint = Entrypoint(value)
		if err := Validate(candidate); err == nil {
			t.Errorf("Validate() accepted Entrypoint = %q", value)
		}
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

// TestValidateNamesAnUnreadableSchemaVersion pins the one refusal a caller has to
// be able to tell apart. Everything else Validate refuses is a record that was
// always invalid; a foreign version is a record an earlier build wrote correctly,
// and the store rebuilds for it rather than silently reading past it.
func TestValidateNamesAnUnreadableSchemaVersion(t *testing.T) {
	stale := validRecord()
	stale.SchemaVersion = SchemaVersion - 1
	if err := Validate(stale); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Validate() error = %v, want ErrUnsupportedVersion", err)
	}

	future := validRecord()
	future.SchemaVersion = SchemaVersion + 1
	if err := Validate(future); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Validate() on a future version error = %v, want ErrUnsupportedVersion", err)
	}

	// A record of this version that is invalid for any other reason must not be
	// mistaken for one, or a scan would rebuild the whole spool over one bad line.
	broken := validRecord()
	broken.Name = "contains space"
	if err := Validate(broken); err == nil || errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Validate() error = %v, want a refusal that is not ErrUnsupportedVersion", err)
	}
}

// TestDecodeCarriesTheVersionRefusal covers the path that matters: the store reads
// lines through Decode, so the sentinel has to survive its wrapping.
func TestDecodeCarriesTheVersionRefusal(t *testing.T) {
	stale := validRecord()
	stale.SchemaVersion = SchemaVersion - 1
	line, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if _, err := Decode(line); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Decode() error = %v, want ErrUnsupportedVersion", err)
	}
	if _, err := Decode([]byte("not json")); errors.Is(err, ErrUnsupportedVersion) {
		t.Fatal("Decode() reported unreadable JSON as a foreign schema version")
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

// The persisted scope digest, pinned against the construction written out
// longhand. T119 moved the HMAC step into internal/keyeddigest; a digest that
// shifts by one byte renames every scoped primitive already in the spool and
// splits its counters in two (ADR-0020, ADR-0022, ADR-0017). crypto/hmac is
// spelled out rather than calling the shared helper, so this asserts the
// construction and not the helper against itself.
func TestDerivedNameCarriesTheKeyedHMACSHA256DigestOfTheScope(t *testing.T) {
	namer := testNamer()
	mac := hmac.New(sha256.New, namer.key)
	if _, err := mac.Write([]byte("apps/web")); err != nil {
		t.Fatalf("writing to the MAC: %v", err)
	}
	want := Identifier(scopePrefix + hex.EncodeToString(mac.Sum(nil))[:scopeDigestLen] + ":deploy")

	got, err := namer.DerivedName("apps/web:deploy")
	if err != nil {
		t.Fatalf("DerivedName() error = %v", err)
	}
	if got != want {
		t.Fatalf("DerivedName() = %q, want %q", got, want)
	}
}

// testNamer keys the scope digest for tests. Production keys it with a subkey of
// the per-machine salt (config.Repos.NameKey).
func testNamer() Namer { return NewNamer([]byte("test scope key")) }

// ptr is this file's nullable-field helper. The package's older tests take the
// address of a local (&outcome); a table over eight nullable counts needs one
// expression per row rather than one local per row.
func ptr[T any](v T) *T { return &v }

// nullableCounts names every nullable numeric field on Record beside a setter, so
// a new one shows up here as a missing row rather than as untested arithmetic.
func nullableCounts() []struct {
	field string
	set   func(*Record, *int64)
} {
	return []struct {
		field string
		set   func(*Record, *int64)
	}{
		{"duration_ms", func(r *Record, v *int64) { r.DurationMS = v }},
		{"input_tokens", func(r *Record, v *int64) { r.InputTokens = v }},
		{"output_tokens", func(r *Record, v *int64) { r.OutputTokens = v }},
		{"cache_read_tokens", func(r *Record, v *int64) { r.CacheReadTokens = v }},
		{"cache_creation_tokens", func(r *Record, v *int64) { r.CacheCreationTokens = v }},
		{"thinking_tokens", func(r *Record, v *int64) { r.ThinkingTokens = v }},
		{"tool_calls", func(r *Record, v *int64) { r.ToolCalls = v }},
		{"builtin_tool_calls", func(r *Record, v *int64) { r.BuiltinToolCalls = v }},
	}
}

// TestValidateRejectsANegativeSessionTotal is the fail-closed half of the session
// grain's bounded-numeric contract: a count below zero is not a measurement of
// anything, and a record carrying one is dropped rather than written (plan §3.4).
func TestValidateRejectsANegativeSessionTotal(t *testing.T) {
	for _, count := range nullableCounts() {
		t.Run(count.field, func(t *testing.T) {
			negative := validRecord()
			count.set(&negative, ptr(int64(-1)))
			if err := Validate(negative); err == nil {
				t.Fatalf("Validate() accepted %s = -1", count.field)
			}
		})
	}
}

// TestValidateAcceptsAZeroSessionTotal is the other half, and the one that is easy
// to break by reaching for a truthiness check: a measured zero is a value. A
// session that invoked no primitive is the plan §2.7 baseline, so the zero has to
// survive both validation and serialisation.
func TestValidateAcceptsAZeroSessionTotal(t *testing.T) {
	for _, count := range nullableCounts() {
		t.Run(count.field, func(t *testing.T) {
			zero := validRecord()
			count.set(&zero, ptr(int64(0)))
			if err := Validate(zero); err != nil {
				t.Fatalf("Validate() error = %v for %s = 0", err, count.field)
			}
			encoded, err := Marshal(zero)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if want := `"` + count.field + `":0`; !strings.Contains(string(encoded), want) {
				t.Fatalf("Marshal() = %s, want it to contain %s", encoded, want)
			}
		})
	}
}

// TestMarshalRendersAnUnreportedTotalAsNull keeps unreported distinguishable from
// zero on disk. Neither field carries omitempty for this reason: a reader of the
// spool has to be able to tell "the harness said nothing" from "the harness said
// none", and an absent key reads as the latter (ADR-0005 applied to counts).
func TestMarshalRendersAnUnreportedTotalAsNull(t *testing.T) {
	encoded, err := Marshal(validRecord())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, count := range nullableCounts() {
		if want := `"` + count.field + `":null`; !strings.Contains(string(encoded), want) {
			t.Fatalf("Marshal() = %s, want it to contain %s", encoded, want)
		}
	}
}

// TestIsSessionGrain enumerates the whole Kind enum explicitly rather than
// testing the two members that answer true: a Kind added later has to appear here
// as a missing row, because the consequence of getting it wrong is silent — a
// phantom primitive in every report, or a session that stops being counted.
func TestIsSessionGrain(t *testing.T) {
	sessionGrain := []Kind{KindSessionStart, KindSessionEnd}
	invocationGrain := []Kind{
		KindSkill, KindSubagent, KindMCPTool, KindMCPServer,
		KindCommand, KindPlugin, KindBuiltinTool, KindHook,
	}
	for _, kind := range sessionGrain {
		if !IsSessionGrain(kind) {
			t.Fatalf("IsSessionGrain(%q) = false, want true", kind)
		}
	}
	for _, kind := range invocationGrain {
		if IsSessionGrain(kind) {
			t.Fatalf("IsSessionGrain(%q) = true, want false", kind)
		}
	}
	if len(sessionGrain)+len(invocationGrain) != 10 {
		t.Fatal("the Kind enum changed size; add the new member to one of the two lists")
	}
}
