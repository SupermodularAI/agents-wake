// Package config owns every location and every setting this tool has, and it is
// the only package that touches the two sensitive files behind them.
//
// It answers three questions and nothing else:
//
//   - where things live — ResolvePaths, the XDG layout and the one override wake
//     has, and ClaudeCodeDir for the harness's own directory and the one variable
//     the harness relocates it with;
//   - what the settings are — the key registry, with Load, Get and Set over
//     config.toml;
//   - which repository an observed working directory belongs to — OpenRepos and
//     Identify, over a salted hash of a consented root (ADR-0019).
//
// All access to repo-salt and projects.json stays inside this package. Those two
// files are the only ones in the system holding a secret and real repository
// paths, so confining them to one package is what makes the guarantee — the
// label and the path never leave the local store, only the hashed id — checkable
// by reading one directory instead of auditing every call site (ADR-0007,
// plan §3.4). A test walks the module and fails if either name is spelled
// anywhere else.
//
// Nothing here calls a model, and nothing here echoes what it read: no error,
// log line or returned value carries a repository path, a label or the salt.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
)

// The modes the config root and config.toml are created with. No decision pins a
// mode for config.toml — only repo-salt and projects.json are pinned at 0600 —
// but the file can hold scan.repos, which is a list of repositories, so the
// conservative single-user mode is the default here too.
const (
	configFileMode = fs.FileMode(0o600)
	configDirMode  = fs.FileMode(0o700)
)

// Setting is one key as the user's configuration leaves it: what it resolves to,
// what it would have been, and whether anyone has decided.
type Setting struct {
	// Key is the dotted key name.
	Key string
	// Value is the canonical resolved value — the file's if it defined one and
	// it was valid, otherwise Default.
	Value string
	// Default is what this build would use if the file said nothing.
	Default string
	// Overridden reports that the file defined this key with a usable value. A
	// key the file defined badly is not overridden: its value is the default.
	Overridden bool
	// Provisional reports that the value is uncalibrated (ADR-0015). T007
	// renders the label; this package only states the fact.
	Provisional bool
}

// Problem is something wrong with the config file that is not worth failing a
// command over: an unknown key, or a value this build cannot use.
//
// Reason never quotes the offending value. The caller has the key and the user
// has the file, and a Reason that echoed file content would be the one free-text
// field in this package able to carry a repository path out of it (ADR-0007).
type Problem struct {
	// Key is the dotted key the problem is with.
	Key string
	// Reason states what is wrong, without quoting what was found.
	Reason string
}

// Config is the resolved configuration: the file's values where they were
// usable, defaults everywhere else, and the list of what could not be used.
//
// It is a snapshot. Set deliberately does not go through it — see Set.
type Config struct {
	// values holds the canonical string form of every key the file defined
	// with a usable value. A key missing here resolves to its default.
	values map[string]string
	// problems is what the file got wrong, sorted by key.
	problems []Problem
}

// Load reads config.toml and resolves every known key.
//
// A missing file is not an error: it yields the defaults and creates nothing
// (acceptance item 2). A file this build cannot parse *is* an error — unlike an
// unknown key, an unparseable file has no defensible interpretation, and
// carrying on with defaults would silently discard whatever the user wrote. The
// error names the file so there is something to go and fix.
//
// Everything else the file gets wrong is a Problem rather than a failure: an
// unknown key is reported and left alone, and an unusable value falls back to
// its default and is reported. Neither is repaired, because a value quietly
// corrected is a file that means something the user never wrote.
func Load(p Paths) (*Config, error) {
	c := &Config{values: map[string]string{}}

	raw, err := os.ReadFile(p.ConfigFile)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p.ConfigFile, err)
	}

	table := map[string]any{}
	md, err := toml.Decode(string(raw), &table)
	if err != nil {
		// The decoder's message reports the syntax and the line, not the
		// content of a value, so it is safe to pass on.
		return nil, fmt.Errorf("parsing %s: %w", p.ConfigFile, err)
	}

	for _, key := range md.Keys() {
		name := key.String()
		// A table is not a setting. An unknown table with no leaves under it
		// carries no value, so there is nothing to report and nothing to lose.
		if md.Type(key...) == "Hash" {
			continue
		}

		k, known := lookup(name)
		if !known {
			c.problems = append(c.problems, Problem{Key: name, Reason: "not a known key in this build"})
			continue
		}

		value, reason := canonical(k, valueAt(table, key))
		if reason != "" {
			c.problems = append(c.problems, Problem{Key: name, Reason: reason})
			continue
		}
		if err := validate(k, value); err != nil {
			var invalid *InvalidValueError
			if errors.As(err, &invalid) {
				c.problems = append(c.problems, Problem{Key: name, Reason: invalid.Reason})
				continue
			}
			c.problems = append(c.problems, Problem{Key: name, Reason: "cannot be used by this build"})
			continue
		}
		c.values[name] = value
	}

	slices.SortFunc(c.problems, func(a, b Problem) int { return strings.Compare(a.Key, b.Key) })
	return c, nil
}

// Get returns the canonical string form of a key's value: the file's if it
// defined a usable one, otherwise the default. An unknown name is an
// *UnknownKeyError carrying every known key.
//
// A list renders comma-joined so that `wake config get scan.harnesses` composes
// in a shell (T007).
func (c *Config) Get(name string) (string, error) {
	k, known := lookup(name)
	if !known {
		return "", &UnknownKeyError{Key: name, Known: KeyNames()}
	}
	if v, ok := c.values[name]; ok {
		return v, nil
	}
	return k.Default, nil
}

// Duration resolves a duration key.
//
// ok is false when the value is one of the key's sentinel words — `forever` for
// retention, `never` for rollup. A caller has to be able to tell "no limit" from
// "zero", and a sentinel silently rendered as 0 would expire everything
// immediately. An error means the name is unknown or is not a duration key.
func (c *Config) Duration(name string) (time.Duration, bool, error) {
	k, known := lookup(name)
	if !known {
		return 0, false, &UnknownKeyError{Key: name, Known: KeyNames()}
	}
	if k.Kind != KindDuration {
		return 0, false, fmt.Errorf("%s is not a duration setting", name)
	}

	value, err := c.Get(name)
	if err != nil {
		return 0, false, err
	}
	if slices.Contains(k.Sentinels, value) {
		return 0, false, nil
	}

	d, err := parseDuration(value)
	if err != nil {
		// Unreachable through Load or Set, which both validate: every stored
		// value and every default has already parsed. Reported rather than
		// panicked because "unreachable" is a claim about other code.
		return 0, false, &InvalidValueError{Key: name, Reason: durationReason(k, err)}
	}
	return d, true, nil
}

// StringList resolves a list key.
//
// The empty list is empty and means none, not all: ADR-0019 §2 makes an empty
// scan.repos scan nothing, because only consented repositories produce records.
// Blank entries are dropped rather than becoming a member named "" — a trailing
// comma from a shell must not create a repository.
func (c *Config) StringList(name string) ([]string, error) {
	k, known := lookup(name)
	if !known {
		return nil, &UnknownKeyError{Key: name, Known: KeyNames()}
	}
	if k.Kind != KindStringList {
		return nil, fmt.Errorf("%s is not a list setting", name)
	}

	value, err := c.Get(name)
	if err != nil {
		return nil, err
	}
	return splitList(value), nil
}

// List returns every known setting, sorted by key. It is what `wake config list`
// renders, and the only place the provisional fact reaches a caller.
func (c *Config) List() []Setting {
	all := Keys()
	out := make([]Setting, 0, len(all))
	for _, k := range all {
		value, overridden := c.values[k.Name]
		if !overridden {
			value = k.Default
		}
		out = append(out, Setting{
			Key:         k.Name,
			Value:       value,
			Default:     k.Default,
			Overridden:  overridden,
			Provisional: k.Provisional,
		})
	}
	return out
}

// Problems returns what the config file got wrong, sorted by key. Empty is the
// normal case, including for a file that does not exist.
func (c *Config) Problems() []Problem {
	return slices.Clone(c.problems)
}

// Set writes one key into config.toml and returns the path it wrote.
//
// It re-reads the file rather than writing a Config back, and the difference
// matters: a Config is a snapshot, and marshalling one back would erase every
// key written since it was loaded — including keys a newer build knows and this
// one does not. Decoding into a map and re-encoding the same map is what keeps
// an unknown key alive across a Set (T007's acceptance). The cost is that
// comments and key order in the file are not preserved.
//
// A rejected name or value writes nothing: the file is not created, and an
// existing one is not touched.
//
// The read and the write happen inside one hold of the config lock. Without it two
// concurrent Sets of two different keys each republish a file decoded before the
// other's write, so one key is lost — and lost silently, because both calls
// return the path they wrote.
func Set(p Paths, name, value string) (string, error) {
	k, known := lookup(name)
	if !known {
		return "", &UnknownKeyError{Key: name, Known: KeyNames()}
	}
	if err := validate(k, value); err != nil {
		return "", err
	}

	// Validation first, outside the lock: a rejected value must create nothing, and
	// taking the lock would create the lock file.
	var written string
	if err := withConfigLock(p.ConfigFile, func() error {
		var err error
		written, err = setLocked(p, k, value)
		return err
	}); err != nil {
		return "", err
	}
	return written, nil
}

// listItemCommaReason is why a list member may not contain a comma. Both paths
// into a list value reject the same input with the same words: AddToList on the way
// in, canonical on the way back out of a hand-edited file. One string rather than
// two is what keeps them from drifting into two answers about one file (T112).
const listItemCommaReason = "an item may not contain a comma, which separates the list's members"

// AddToList adds one item to a list setting, keeping every item already there,
// and returns the path it wrote. An item already present is not added twice and
// nothing is written.
//
// It exists because reading the list and setting it back are two calls, and two
// processes doing that concurrently — two `wake init` runs in two repositories, or
// one racing a hook-triggered scan — each write a list missing the other's item.
// The read and the write happen inside one hold of the config lock, so the merge
// is against what is on disk rather than against a snapshot.
func AddToList(p Paths, name, item string) (string, error) {
	k, known := lookup(name)
	if !known {
		return "", &UnknownKeyError{Key: name, Known: KeyNames()}
	}
	if k.Kind != KindStringList {
		return "", fmt.Errorf("%s is not a list setting", name)
	}
	// A comma is the separator, so an item carrying one would come back as two
	// members on the next read. The reason names the character and not the value:
	// a repository id is not secret, but this is the type's contract.
	if strings.Contains(item, ",") {
		return "", &InvalidValueError{Key: name, Reason: listItemCommaReason}
	}

	var written string
	if err := withConfigLock(p.ConfigFile, func() error {
		// Load only reads, so calling it inside the lock is safe — and necessary:
		// merging against a list read before the lock is the hole this closes.
		c, err := Load(p)
		if err != nil {
			return err
		}
		list, err := c.StringList(name)
		if err != nil {
			return err
		}
		if slices.Contains(list, item) {
			written = p.ConfigFile
			return nil
		}
		written, err = setLocked(p, k, strings.Join(append(list, item), ","))
		return err
	}); err != nil {
		return "", err
	}
	return written, nil
}

// setLocked is Set's body with the config lock already held. Separate because
// AddToList reads the list and writes it back inside one hold, and flock is per
// open file description: a helper that took the lock itself would deadlock against
// its own caller.
//
// It takes the resolved Key rather than a name so it does not repeat the lookup
// its callers have already done.
func setLocked(p Paths, k Key, value string) (string, error) {
	name := k.Name

	table := map[string]any{}
	raw, readErr := os.ReadFile(p.ConfigFile)
	switch {
	case readErr == nil:
		if _, err := toml.Decode(string(raw), &table); err != nil {
			return "", fmt.Errorf("parsing %s: %w", p.ConfigFile, err)
		}
	case !errors.Is(readErr, fs.ErrNotExist):
		return "", fmt.Errorf("reading %s: %w", p.ConfigFile, readErr)
	}

	if err := setValueAt(table, strings.Split(name, "."), encoded(k, value)); err != nil {
		return "", fmt.Errorf("%w in %s", err, p.ConfigFile)
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(table); err != nil {
		return "", fmt.Errorf("encoding %s: %w", p.ConfigFile, err)
	}

	// Publishing sets the mode as part of the write, so there is no window in which
	// a file that can hold scan.repos is readable by anyone else, and no separate
	// Chmod to forget: a file created by hand does not keep its own mode here.
	if err := atomicfile.Publish(p.ConfigFile, buf.Bytes(), configFileMode); err != nil {
		return "", fmt.Errorf("writing %s: %w", p.ConfigFile, err)
	}
	return p.ConfigFile, nil
}

// canonical converts a decoded TOML value into the canonical string form for its
// key, or returns the reason it cannot. The reason never quotes the value.
func canonical(k Key, v any) (string, string) {
	switch k.Kind {
	case KindDuration:
		s, ok := v.(string)
		if !ok {
			return "", "must be a quoted duration, such as \"30d\""
		}
		return s, ""
	case KindStringList:
		items, ok := v.([]any)
		if !ok {
			return "", "must be an array of strings, such as [\"a\", \"b\"]"
		}
		parts := make([]string, 0, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				return "", "must be an array of strings, such as [\"a\", \"b\"]"
			}
			// The canonical form joins the members with a comma, so an element
			// carrying one is indistinguishable from two members once joined —
			// silently becoming a list the user never wrote, and for scan.repos two
			// consented repositories that do not exist. Rejected rather than
			// repaired, in the same words AddToList uses, so a hand-edited file can
			// only mean what the command could have written (T112).
			if strings.Contains(s, ",") {
				return "", listItemCommaReason
			}
			// Trimmed and dropped per element rather than by re-splitting the joined
			// string: same result for every comma-free value, without a round trip
			// that has to reinterpret content it just produced.
			if s = strings.TrimSpace(s); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ","), ""
	default:
		return "", "this key has no value kind; it is registered wrongly"
	}
}

// encoded converts a canonical string into the value to write into the file. A
// list is a TOML array there, and a comma-separated string only on the command
// line, so that a value containing a comma cannot be ambiguous once written.
func encoded(k Key, value string) any {
	if k.Kind != KindStringList {
		return value
	}
	parts := splitList(value)
	items := make([]any, 0, len(parts))
	for _, part := range parts {
		items = append(items, part)
	}
	return items
}

// splitList parses the comma-separated form, trimming each entry and dropping
// blank ones.
func splitList(value string) []string {
	out := []string{}
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// valueAt walks a decoded table to a dotted key. The key comes from
// MetaData.Keys, so the path exists.
func valueAt(table map[string]any, key toml.Key) any {
	var current any = table
	for _, part := range key {
		sub, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = sub[part]
	}
	return current
}

// setValueAt writes a value at a dotted path, creating the tables on the way.
//
// It refuses to replace a non-table with a table: a user who wrote `ui = 5` has
// a file this package cannot write into, and turning that into a table would
// delete what they wrote. The error names the path, which is a key name rather
// than anything read out of the file.
func setValueAt(table map[string]any, parts []string, value any) error {
	current := table
	for i, part := range parts[:len(parts)-1] {
		switch existing := current[part].(type) {
		case nil:
			sub := map[string]any{}
			current[part] = sub
			current = sub
		case map[string]any:
			current = existing
		default:
			return fmt.Errorf("%s is not a table", strings.Join(parts[:i+1], "."))
		}
	}
	current[parts[len(parts)-1]] = value
	return nil
}
