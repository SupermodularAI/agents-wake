package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
		{"a list element containing a comma", "[scan]\nharnesses = [\"claude-code\", \"my,harness\"]\n", "scan.harnesses", "claude-code,opencode"},
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
		// only thing the command reads. Compared against the registry rather
		// than a literal pair of names: which keys are provisional is a
		// per-build fact — the tagged build has a third, remote.min_interval —
		// and it is pinned exhaustively by registry_default_test.go and
		// keys_remote_test.go. What is under test here is that List carries the
		// flag through unchanged, and a second closed literal would only make
		// this test fail on a key it says nothing about.
		k, ok := lookup(s.Key)
		if !ok {
			t.Errorf("List() returned %q, which is not a registered key", s.Key)
			continue
		}
		if s.Provisional != k.Provisional {
			t.Errorf("%s provisional = %v, want %v", s.Key, s.Provisional, k.Provisional)
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
	// config.lock as well as config.toml: Set serialises its read-modify-write
	// against a second writer, and the lock is a separate always-empty file
	// because opening config.toml with O_CREATE would leave a zero-length file
	// behind on a crash. Both belong to the config root and neither is in the
	// data root, which is the property this test is about.
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	want := []string{filepath.Base(p.ConfigFile), configLockName}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("the config root holds %v, want %v", names, want)
	}
}

// Acceptance: two concurrent writes of different keys must both survive. Set is a
// read-decode-modify-encode-write, so without serialisation the writer that
// decoded first republishes a table that never had the other's key in it, and the
// loss is silent — both calls return success.
func TestSetPreservesAnUnrelatedKeyWrittenConcurrently(t *testing.T) {
	p := testPaths(t)

	writes := map[string]string{
		"store.retention_raw":     "90d",
		"store.rollup_after":      "7d",
		"ui.default_window":       "14d",
		"session.idle_timeout":    "45m",
		"scan.stale_call_timeout": "12h",
		"scan.harnesses":          "claude-code",
		"scan.repos":              "abc",
	}

	var writers sync.WaitGroup
	failures := make(chan error, len(writes))
	for name, value := range writes {
		writers.Add(1)
		go func() {
			defer writers.Done()
			if _, err := Set(p, name, value); err != nil {
				failures <- fmt.Errorf("Set(%q, %q) = %w", name, value, err)
			}
		}()
	}
	writers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	for name, want := range writes {
		got, err := c.Get(name)
		if err != nil {
			t.Errorf("Get(%q) = %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("Get(%q) = %q, want %q — a concurrent Set erased it", name, got, want)
		}
	}
}

// Acceptance: the same property for a list, where the hole is wider. Reading the
// list and setting it back are two calls, so two `wake init` runs in two
// repositories each write a list missing the other's id.
func TestAddToListKeepsItemsAddedConcurrently(t *testing.T) {
	p := testPaths(t)

	const adders = 8
	ids := make([]string, adders)
	for i := range ids {
		ids[i] = strings.Repeat(fmt.Sprintf("%x", i), idHexLen)
	}

	var writers sync.WaitGroup
	failures := make(chan error, adders)
	for _, id := range ids {
		writers.Add(1)
		go func() {
			defer writers.Done()
			if _, err := AddToList(p, "scan.repos", id); err != nil {
				failures <- fmt.Errorf("AddToList(%q) = %w", id, err)
			}
		}()
	}
	writers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	list, err := c.StringList("scan.repos")
	if err != nil {
		t.Fatalf("StringList() = %v", err)
	}
	for _, id := range ids {
		if !slices.Contains(list, id) {
			t.Errorf("scan.repos = %v, missing %q — a concurrent add erased it", list, id)
		}
	}
}

// Re-running `wake init` in a repository already consented to must not record it
// twice: the list is what the scan filter iterates, and a duplicate would be a
// second pass over the same repository.
func TestAddToListIsIdempotent(t *testing.T) {
	p := testPaths(t)

	for attempt := range 2 {
		if _, err := AddToList(p, "scan.repos", "alpha"); err != nil {
			t.Fatalf("AddToList() attempt %d = %v", attempt, err)
		}
	}

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	list, err := c.StringList("scan.repos")
	if err != nil {
		t.Fatalf("StringList() = %v", err)
	}
	if len(list) != 1 || list[0] != "alpha" {
		t.Errorf("scan.repos = %v, want exactly one \"alpha\"", list)
	}
}

func TestAddToListRejectsANonListKey(t *testing.T) {
	p := testPaths(t)

	if _, err := AddToList(p, "ui.default_window", "7d"); err == nil {
		t.Fatal("AddToList() error = nil, want a rejection for a non-list key")
	}

	assertConfigNotCreated(t, p)
}

// A comma is the list separator, so an item containing one would come back as two
// members on the next read — a repository id that resolves to nothing, twice.
func TestAddToListRejectsAnItemContainingAComma(t *testing.T) {
	p := testPaths(t)

	if _, err := AddToList(p, "scan.repos", "alpha,beta"); err == nil {
		t.Fatal("AddToList() error = nil, want a rejection for an item containing a comma")
	}

	assertConfigNotCreated(t, p)
}

// tomlAssignment renders `key = value` inside the table a dotted key names, so a
// test can define any registered key without hard-coding its group.
func tomlAssignment(name, value string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return fmt.Sprintf("[%s]\n%s = %s\n", name[:i], name[i+1:], value)
	}
	return fmt.Sprintf("%s = %s\n", name, value)
}

// A comma inside one array element of a hand-edited config.toml is valid TOML and
// is not two members. Splitting it would make the file mean something the user
// never wrote, and for scan.repos it would mean consenting to two repositories
// that do not exist. Driven off the kind, not off today's two key names, so a list
// key added later cannot arrive without the guard (T112).
func TestAListElementContainingACommaIsRejectedNotSplit(t *testing.T) {
	const offending = "my,harness"

	exercised := 0
	for _, k := range Keys() {
		if k.Kind != KindStringList {
			continue
		}
		exercised++
		t.Run(k.Name, func(t *testing.T) {
			p := testPaths(t)
			writeConfig(t, p, tomlAssignment(k.Name, `["alpha", "`+offending+`"]`))

			cfg := loadOrFail(t, p)

			if got := getOrFail(t, cfg, k.Name); got != k.Default {
				t.Errorf("Get(%s) = %q, want the default %q", k.Name, got, k.Default)
			}
			list, err := cfg.StringList(k.Name)
			if err != nil {
				t.Fatalf("StringList(%s) = %v", k.Name, err)
			}
			for _, member := range list {
				if member == "my" || member == "harness" || member == offending {
					t.Errorf("StringList(%s) = %v; the element was split into extra entries", k.Name, list)
				}
			}
			problem, ok := problemFor(cfg, k.Name)
			if !ok {
				t.Fatalf("Problems() does not report %s; got %v", k.Name, cfg.Problems())
			}
			// The reason names the key and the character, never the value: this file
			// is what a user pastes into a bug report (ADR-0019 §4, plan §7.1).
			if strings.Contains(problem.Reason, offending) || strings.Contains(problem.Reason, "alpha") {
				t.Errorf("the problem for %s quotes the file's value: %q", k.Name, problem.Reason)
			}
		})
	}
	if exercised == 0 {
		t.Fatal("no KindStringList key was exercised; the registry has none and this test proves nothing")
	}
}

// Acceptance item 3: the load path and the write path answer the same input the
// same way. AddToList's rejection is the reference behaviour (T112 out-of-scope),
// so canonical is held to its exact reason rather than to a lookalike.
func TestLoadAndAddToListRejectACommaTheSameWay(t *testing.T) {
	const item = "alpha,beta"

	_, writeErr := AddToList(testPaths(t), "scan.repos", item)
	var invalid *InvalidValueError
	if !errors.As(writeErr, &invalid) {
		t.Fatalf("AddToList() = %v, want an *InvalidValueError", writeErr)
	}

	p := testPaths(t)
	writeConfig(t, p, "[scan]\nrepos = [\""+item+"\"]\n")
	problem, ok := problemFor(loadOrFail(t, p), "scan.repos")
	if !ok {
		t.Fatal("Problems() does not report scan.repos for a comma inside an element")
	}

	if problem.Reason != invalid.Reason {
		t.Errorf("the load path says %q and the write path says %q; one file, two answers", problem.Reason, invalid.Reason)
	}
	if problem.Reason != listItemCommaReason {
		t.Errorf("reason = %q, want %q", problem.Reason, listItemCommaReason)
	}
}

// canonical is the one place a decoded TOML array becomes the canonical
// comma-joined string. Asserted directly because Get, StringList, AddToList and
// encoded all consume that string as comma-separated, so its exact shape is the
// contract and not an implementation detail.
func TestCanonicalListJoinsTrimsAndDropsBlanks(t *testing.T) {
	k, known := lookup("scan.harnesses")
	if !known {
		t.Fatal("scan.harnesses is not registered; the fixture is wrong")
	}

	for _, c := range []struct {
		name       string
		items      []any
		want       string
		wantReason string
	}{
		{name: "two members", items: []any{"claude-code", "opencode"}, want: "claude-code,opencode"},
		{name: "one member", items: []any{"claude-code"}, want: "claude-code"},
		{name: "spaces and blanks", items: []any{" a ", "", "b"}, want: "a,b"},
		{name: "the empty array", items: []any{}, want: ""},
		{name: "a comma inside an element", items: []any{"claude-code", "my,harness"}, wantReason: listItemCommaReason},
		{name: "a comma as the whole element", items: []any{","}, wantReason: listItemCommaReason},
		{name: "a non-string member", items: []any{1}, wantReason: "must be an array of strings, such as [\"a\", \"b\"]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// c.items is passed as-is: an explicit any(...) conversion here would be
			// flagged by unconvert, which .golangci.yml enables and does not exclude
			// for test files.
			got, reason := canonical(k, c.items)
			if reason != c.wantReason {
				t.Errorf("canonical() reason = %q, want %q", reason, c.wantReason)
			}
			if c.wantReason != "" {
				if got != "" {
					t.Errorf("canonical() = %q with a reason set, want the empty string", got)
				}
				return
			}
			if got != c.want {
				t.Errorf("canonical() = %q, want %q", got, c.want)
			}
		})
	}
}

// Three state files, three locks, three distinct paths. A shared lock would make
// a hook-triggered scan wait on a `wake init` writing config.toml, which is the
// serialisation the split exists to avoid.
func TestConfigLockIsNotTheProjectsLock(t *testing.T) {
	p := testPaths(t)

	locks := []string{
		filepath.Join(p.ConfigDir, configLockName),
		filepath.Join(p.ConfigDir, claudeSettingsLockName),
		filepath.Join(filepath.Dir(p.ProjectsFile), projectsLockName),
	}
	for i, left := range locks {
		for _, right := range locks[i+1:] {
			if left == right {
				t.Errorf("two locks resolve to the same file %q; a writer of one would block a writer of the other", left)
			}
		}
	}
}

// assertConfigNotCreated fails when a rejected write left a config file behind. A
// rejected name or value writes nothing at all.
func assertConfigNotCreated(t *testing.T, p Paths) {
	t.Helper()

	if _, err := os.Lstat(p.ConfigFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Lstat(%q) = %v, want ErrNotExist — a rejected write must create nothing", p.ConfigFile, err)
	}
}
