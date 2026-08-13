package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeConfig puts a config.toml under the config root, creating the root the
// way a user's editor would. Every fixture in this package is a literal written
// into a temporary directory: testdata/ is reserved for captured harness
// transcripts and is off-limits to hand-added files.
func writeConfig(t *testing.T, p Paths, content string) {
	t.Helper()
	if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
		t.Fatalf("creating the config root: %v", err)
	}
	if err := os.WriteFile(p.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config.toml: %v", err)
	}
}

func loadOrFail(t *testing.T, p Paths) *Config {
	t.Helper()
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	return c
}

func getOrFail(t *testing.T, c *Config, name string) string {
	t.Helper()
	v, err := c.Get(name)
	if err != nil {
		t.Fatalf("Get(%q) = %v", name, err)
	}
	return v
}

// problemFor returns the reported problem for a key, or false.
func problemFor(c *Config, key string) (Problem, bool) {
	for _, p := range c.Problems() {
		if p.Key == key {
			return p, true
		}
	}
	return Problem{}, false
}

// Acceptance item 2. A tool that wrote a config file just because it was asked
// what a setting is would leave state behind on `wake --version`.
func TestMissingConfigFileYieldsDefaultsAndCreatesNothing(t *testing.T) {
	p := testPaths(t)

	c := loadOrFail(t, p)

	if got := getOrFail(t, c, "ui.default_window"); got != "30d" {
		t.Errorf("Get(ui.default_window) = %q, want the default %q", got, "30d")
	}
	if problems := c.Problems(); len(problems) != 0 {
		t.Errorf("Problems() = %v, want none for a missing file", problems)
	}
	for _, path := range []string{p.ConfigFile, p.ConfigDir, p.DataDir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("os.Lstat(%q) = %v, want ErrNotExist — Load must create nothing", path, err)
		}
	}
}

// Acceptance item 4, the read half: the package rejects the name, and the error
// carries the list so T007 only has to print it.
func TestGetUnknownKeyIsRejectedWithTheKnownKeys(t *testing.T) {
	c := loadOrFail(t, testPaths(t))

	_, err := c.Get("ui.no_such_key")

	var unknown *UnknownKeyError
	if !errors.As(err, &unknown) {
		t.Fatalf("Get(ui.no_such_key) = %v, want *UnknownKeyError", err)
	}
	if !slices.Equal(unknown.Known, KeyNames()) {
		t.Errorf("Known = %v, want every known key %v", unknown.Known, KeyNames())
	}
}

// Acceptance item 4, the write half. Rejection is the package's job, not the
// command's, and nothing may be written on the way to rejecting it.
func TestSetUnknownKeyIsRejectedByThePackage(t *testing.T) {
	p := testPaths(t)

	path, err := Set(p, "store.retention", "forever")

	var unknown *UnknownKeyError
	if !errors.As(err, &unknown) {
		t.Fatalf("Set(store.retention) = (%q, %v), want *UnknownKeyError", path, err)
	}
	if !slices.Equal(unknown.Known, KeyNames()) {
		t.Errorf("Known = %v, want every known key %v", unknown.Known, KeyNames())
	}
	if _, err := os.Lstat(p.ConfigFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Lstat(%q) = %v, want ErrNotExist — a rejected key must write nothing", p.ConfigFile, err)
	}
}

func TestSetRejectsAnInvalidValue(t *testing.T) {
	p := testPaths(t)

	path, err := Set(p, "ui.default_window", "bananas")

	var invalid *InvalidValueError
	if !errors.As(err, &invalid) {
		t.Fatalf("Set(ui.default_window, \"bananas\") = (%q, %v), want *InvalidValueError", path, err)
	}
	if invalid.Key != "ui.default_window" {
		t.Errorf("Key = %q, want %q", invalid.Key, "ui.default_window")
	}
	if _, err := os.Lstat(p.ConfigFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Lstat(%q) = %v, want ErrNotExist — a rejected value must write nothing", p.ConfigFile, err)
	}
}

// config.toml can hold scan.repos, so it is not a public file; and D8 pins the
// mode rather than inheriting whatever umask the user runs with.
func TestSetWritesAndReportsThePath(t *testing.T) {
	p := testPaths(t)

	path, err := Set(p, "ui.default_window", "7d")
	if err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if path != p.ConfigFile {
		t.Errorf("Set() reported %q, want %q", path, p.ConfigFile)
	}

	fi, err := os.Stat(p.ConfigFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.toml mode = %#o, want 0600", perm)
	}
	di, err := os.Stat(p.ConfigDir)
	if err != nil {
		t.Fatalf("stat the config root: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("config root mode = %#o, want 0700", perm)
	}
}

func TestSetThenLoadReturnsTheNewValue(t *testing.T) {
	p := testPaths(t)

	if _, err := Set(p, "session.idle_timeout", "45m"); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	c := loadOrFail(t, p)
	if got := getOrFail(t, c, "session.idle_timeout"); got != "45m" {
		t.Errorf("Get(session.idle_timeout) = %q, want %q", got, "45m")
	}
	d, ok, err := c.Duration("session.idle_timeout")
	if err != nil || !ok || d != 45*time.Minute {
		t.Errorf("Duration(session.idle_timeout) = (%v, %v, %v), want (45m, true, nil)", d, ok, err)
	}
}

// T007's acceptance requires an unknown key to be reported rather than silently
// dropped, which rules out marshalling a struct back over the file: a key this
// build does not know must survive a `set` of a key it does.
func TestSetPreservesUnknownKeysAlreadyInTheFile(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "unknown_top = 1\n\n[zzz]\nfuture_key = \"kept\"\n\n[store]\nretention_raw = \"forever\"\n")

	if _, err := Set(p, "ui.default_window", "7d"); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	raw, err := os.ReadFile(p.ConfigFile)
	if err != nil {
		t.Fatalf("reading config.toml: %v", err)
	}
	for _, want := range []string{"unknown_top", "zzz", "future_key", "kept", "default_window"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config.toml lost %q after Set:\n%s", want, raw)
		}
	}
}

func TestSetOnAListKeyWritesATomlArray(t *testing.T) {
	p := testPaths(t)

	if _, err := Set(p, "scan.harnesses", "claude-code, opencode"); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	raw, err := os.ReadFile(p.ConfigFile)
	if err != nil {
		t.Fatalf("reading config.toml: %v", err)
	}
	if !strings.Contains(string(raw), "harnesses = [") {
		t.Errorf("scan.harnesses was not written as a TOML array:\n%s", raw)
	}

	c := loadOrFail(t, p)
	got, err := c.StringList("scan.harnesses")
	if err != nil {
		t.Fatalf("StringList() = %v", err)
	}
	if want := []string{"claude-code", "opencode"}; !slices.Equal(got, want) {
		t.Errorf("StringList(scan.harnesses) = %v, want %v", got, want)
	}
	// The comma-joined form is what `wake config get` prints, so it has to
	// compose in a shell without the spaces the user happened to type.
	if got := getOrFail(t, c, "scan.harnesses"); got != "claude-code,opencode" {
		t.Errorf("Get(scan.harnesses) = %q, want %q", got, "claude-code,opencode")
	}
}

// A key this build does not know is a key a newer build might: reported, kept,
// and never fatal (T007 acceptance).
func TestUnknownKeyInTheFileIsReportedNotFatal(t *testing.T) {
	p := testPaths(t)
	const content = "oops = 1\n\n[store]\nretention_raw = \"forever\"\nbogus = \"x\"\n"
	writeConfig(t, p, content)

	c := loadOrFail(t, p)

	for _, key := range []string{"oops", "store.bogus"} {
		problem, ok := problemFor(c, key)
		if !ok {
			t.Errorf("Problems() does not report %q; got %v", key, c.Problems())
			continue
		}
		if problem.Reason == "" {
			t.Errorf("the problem for %q has no reason", key)
		}
	}
	if got := getOrFail(t, c, "store.retention_raw"); got != "forever" {
		t.Errorf("Get(store.retention_raw) = %q; an unknown neighbour must not disturb a known key", got)
	}

	raw, err := os.ReadFile(p.ConfigFile)
	if err != nil {
		t.Fatalf("reading config.toml: %v", err)
	}
	if string(raw) != content {
		t.Errorf("Load rewrote config.toml:\ngot:\n%s\nwant:\n%s", raw, content)
	}
}

// Fail closed and stay visible: a value this build cannot use falls back to the
// default and says so. Silently repairing it would make the file mean something
// the user never wrote (constraint 22).
func TestInvalidValueInTheFileFallsBackToTheDefaultAndIsReported(t *testing.T) {
	for _, c := range []struct {
		name    string
		content string
		key     string
		want    string
	}{
		{"a duration that does not parse", "[ui]\ndefault_window = \"bananas\"\n", "ui.default_window", "30d"},
		{"a duration that is not a string", "[ui]\ndefault_window = 7\n", "ui.default_window", "30d"},
		{"a negative duration", "[session]\nidle_timeout = \"-5m\"\n", "session.idle_timeout", "30m"},
		{"a list that is not an array", "[scan]\nharnesses = \"claude-code\"\n", "scan.harnesses", "claude-code,opencode"},
		{"a list holding a non-string", "[scan]\nharnesses = [1, 2]\n", "scan.harnesses", "claude-code,opencode"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := testPaths(t)
			writeConfig(t, p, c.content)

			cfg := loadOrFail(t, p)

			if got := getOrFail(t, cfg, c.key); got != c.want {
				t.Errorf("Get(%s) = %q, want the default %q", c.key, got, c.want)
			}
			problem, ok := problemFor(cfg, c.key)
			if !ok {
				t.Fatalf("Problems() does not report %s; got %v", c.key, cfg.Problems())
			}
			if problem.Reason == "" {
				t.Error("the problem has no reason")
			}
			for _, setting := range cfg.List() {
				if setting.Key == c.key && setting.Overridden {
					t.Errorf("List() marks %s overridden, but its value was rejected", c.key)
				}
			}
		})
	}
}

// An unparseable file has no defensible interpretation, and ignoring it would
// hide the user's edit. This is deliberately unlike an unknown key.
func TestBrokenTomlIsAnError(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "[store\nretention_raw = \"forever\"\n")

	c, err := Load(p)
	if err == nil {
		t.Fatalf("Load() = (%v, nil), want an error", c)
	}
	if !strings.Contains(err.Error(), p.ConfigFile) {
		t.Errorf("error %q does not name the file the user has to fix", err)
	}
}

func TestListDistinguishesDefaultFromOverridden(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "[ui]\ndefault_window = \"7d\"\n")

	c := loadOrFail(t, p)
	settings := c.List()

	if len(settings) != len(Keys()) {
		t.Fatalf("List() returned %d settings, want %d", len(settings), len(Keys()))
	}
	if !slices.IsSortedFunc(settings, func(a, b Setting) int { return strings.Compare(a.Key, b.Key) }) {
		t.Error("List() is not sorted by key; the output has to be deterministic")
	}

	for _, s := range settings {
		switch s.Key {
		case "ui.default_window":
			if !s.Overridden || s.Value != "7d" || s.Default != "30d" {
				t.Errorf("%s = %+v, want value 7d, default 30d, overridden", s.Key, s)
			}
		default:
			if s.Overridden {
				t.Errorf("%s is marked overridden but the file does not define it", s.Key)
			}
			if s.Value != s.Default {
				t.Errorf("%s = %q, want its default %q", s.Key, s.Value, s.Default)
			}
		}
		// The provisional fact has to reach T007 through List, which is the
		// only thing the command reads.
		if want := (s.Key == "session.idle_timeout" || s.Key == "scan.stale_call_timeout"); s.Provisional != want {
			t.Errorf("%s provisional = %v, want %v", s.Key, s.Provisional, want)
		}
	}
}

func TestListRendersListValuesAsCommaSeparated(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "[scan]\nrepos = [\"a\", \"b\"]\n")

	for _, s := range loadOrFail(t, p).List() {
		if s.Key != "scan.repos" {
			continue
		}
		if s.Value != "a,b" {
			t.Errorf("scan.repos value = %q, want %q", s.Value, "a,b")
		}
		if s.Default != "" {
			t.Errorf("scan.repos default = %q, want the empty list", s.Default)
		}
	}
}

// ADR-0015's thresholds have to reach a reader as a time.Duration; a reader that
// re-parsed the string would be the constant this ticket exists to remove.
func TestDurationResolvesTheTwoTimeouts(t *testing.T) {
	c := loadOrFail(t, testPaths(t))

	for _, want := range []struct {
		key string
		d   time.Duration
	}{
		{"session.idle_timeout", 30 * time.Minute},
		{"scan.stale_call_timeout", 24 * time.Hour},
		{"ui.default_window", 720 * time.Hour},
	} {
		d, ok, err := c.Duration(want.key)
		if err != nil {
			t.Errorf("Duration(%s) = %v", want.key, err)
			continue
		}
		if !ok {
			t.Errorf("Duration(%s) reported no duration, want %v", want.key, want.d)
			continue
		}
		if d != want.d {
			t.Errorf("Duration(%s) = %v, want %v", want.key, d, want.d)
		}
	}
}

// `forever` and `never` are answers, not durations. A caller must be able to
// tell "no limit" from "zero", which is why the second return exists.
func TestDurationReportsSentinelValues(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "[store]\nrollup_after = \"90d\"\n")

	c := loadOrFail(t, p)

	d, ok, err := c.Duration("store.retention_raw")
	if err != nil {
		t.Fatalf("Duration(store.retention_raw) = %v", err)
	}
	if ok || d != 0 {
		t.Errorf("Duration(store.retention_raw) = (%v, %v), want (0, false) for the sentinel `forever`", d, ok)
	}

	d, ok, err = c.Duration("store.rollup_after")
	if err != nil {
		t.Fatalf("Duration(store.rollup_after) = %v", err)
	}
	if !ok || d != 2160*time.Hour {
		t.Errorf("Duration(store.rollup_after) = (%v, %v), want (2160h, true)", d, ok)
	}

	if _, _, err := c.Duration("scan.harnesses"); err == nil {
		t.Error("Duration(scan.harnesses) = nil error, want a rejection: it is not a duration key")
	}
	if _, err := c.StringList("ui.default_window"); err == nil {
		t.Error("StringList(ui.default_window) = nil error, want a rejection: it is not a list key")
	}
}

// ADR-0019 §2: an empty active-repo list scans nothing. An empty list read as
// "all repos" is the consent hole the ADR exists to close, so the emptiness is
// asserted rather than assumed.
func TestStringListEmptyMeansNone(t *testing.T) {
	c := loadOrFail(t, testPaths(t))

	got, err := c.StringList("scan.repos")
	if err != nil {
		t.Fatalf("StringList(scan.repos) = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("StringList(scan.repos) = %v, want an empty list", got)
	}

	// Whitespace and stray commas are the shape a shell produces; none of them
	// may become a repo named "".
	p := testPaths(t)
	writeConfig(t, p, "[scan]\nrepos = [\" a \", \"\", \"b\"]\n")
	got, err = loadOrFail(t, p).StringList("scan.repos")
	if err != nil {
		t.Fatalf("StringList(scan.repos) = %v", err)
	}
	if want := []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("StringList(scan.repos) = %v, want %v", got, want)
	}
}

// Set re-reads the file rather than writing an in-memory copy back: two
// processes each holding a Config would otherwise have the second erase the
// first's key.
func TestSetDoesNotEraseAKeyWrittenBySomeoneElse(t *testing.T) {
	p := testPaths(t)

	if _, err := Set(p, "ui.default_window", "7d"); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	stale := loadOrFail(t, p)
	if _, err := Set(p, "session.idle_timeout", "45m"); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	// The stale Config is used only to prove it cannot write itself back.
	if got := getOrFail(t, stale, "session.idle_timeout"); got != "30m" {
		t.Fatalf("the stale Config already sees %q; the fixture is wrong", got)
	}
	if _, err := Set(p, "ui.default_window", "14d"); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	c := loadOrFail(t, p)
	if got := getOrFail(t, c, "session.idle_timeout"); got != "45m" {
		t.Errorf("Get(session.idle_timeout) = %q, want %q — Set erased another writer's key", got, "45m")
	}
	if got := getOrFail(t, c, "ui.default_window"); got != "14d" {
		t.Errorf("Get(ui.default_window) = %q, want %q", got, "14d")
	}
}

// A user who wrote `ui = 5` has a file this package cannot write into. Replacing
// the value with a table would delete what they wrote, so it is an error.
func TestSetRefusesToOverwriteANonTable(t *testing.T) {
	p := testPaths(t)
	const content = "ui = 5\n"
	writeConfig(t, p, content)

	if _, err := Set(p, "ui.default_window", "7d"); err == nil {
		t.Fatal("Set() = nil, want an error: `ui` is not a table")
	}

	raw, err := os.ReadFile(p.ConfigFile)
	if err != nil {
		t.Fatalf("reading config.toml: %v", err)
	}
	if string(raw) != content {
		t.Errorf("config.toml changed:\ngot:\n%s\nwant:\n%s", raw, content)
	}
}

// The data root is not where config lives, and a Set must not create it. The
// two roots are separate so uninstall can remove one and keep the other
// (ADR-0010).
func TestSetTouchesOnlyTheConfigRoot(t *testing.T) {
	p := testPaths(t)

	if _, err := Set(p, "ui.default_window", "7d"); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	if _, err := os.Lstat(p.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Lstat(%q) = %v, want ErrNotExist — Set must not create the data root", p.DataDir, err)
	}
	entries, err := os.ReadDir(p.ConfigDir)
	if err != nil {
		t.Fatalf("reading the config root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(p.ConfigFile) {
		t.Errorf("the config root holds %v, want only config.toml", entries)
	}
}
