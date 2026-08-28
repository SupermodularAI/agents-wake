package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// keys holds every setting this build knows about. Each group of keys lives in
// its own file and appends itself here from an init():
//
//	// keys.go
//	func init() { register(Key{Name: "ui.default_window", ...}) }
//
// Registration is an append from a separate file for the same mechanical reason
// internal/cli/registry.go uses one for subcommands: a closed literal here would
// make every key group a shared-file edit, so two lanes adding unrelated keys in
// parallel would conflict on the same line for no design reason. keys.go and
// keys_remote.go each own their own group and neither touches this file.
//
// Access is unsynchronised by design: init() runs before any goroutine of ours,
// and nothing appends afterwards.
var keys []Key

// Kind is how a setting's value is spelled, and therefore how it is validated.
// There are two because the eight keys need two: a duration (possibly a sentinel
// word) or a list of strings.
type Kind int

// The kinds a setting can have. The zero value is deliberately not one of them,
// so a Key constructed without a Kind fails validation rather than defaulting to
// a lenient one.
const (
	// KindDuration is a duration in Go's syntax extended with a `d` suffix, or
	// one of the key's Sentinels.
	KindDuration Kind = iota + 1
	// KindStringList is a comma-separated list on the command line and a TOML
	// array of strings in the file.
	KindStringList
)

// Key is one setting: what it is called, how it is spelled, and what it means
// when the user has said nothing.
type Key struct {
	// Name is the dotted key, e.g. "store.retention_raw".
	Name string
	// Kind is how the value is spelled and validated.
	Kind Kind
	// Default is the canonical string form of the value used when the file does
	// not define this key. It must itself be valid under Kind.
	Default string
	// Sentinels are words this key accepts in place of a duration, e.g.
	// "forever". They are per key: `forever` is a retention answer, not a
	// window.
	Sentinels []string
	// Provisional marks a value nobody has calibrated. ADR-0015 requires the two
	// timeouts to be tunable and says their values need real-world calibration,
	// which P3 owns; until then this fact is API so T007 can label it. A
	// provisional value presented as a considered one is worse than none.
	Provisional bool
}

// register adds keys to the registry. It is called from init() only.
func register(k ...Key) {
	keys = append(keys, k...)
}

// Keys returns every known key, sorted by name. The slice and its Key values are
// a fresh copy each call: a caller that mutated the registry would redefine a
// setting for the rest of the process.
func Keys() []Key {
	out := make([]Key, len(keys))
	copy(out, keys)
	for i := range out {
		out[i].Sentinels = slices.Clone(out[i].Sentinels)
	}
	slices.SortFunc(out, func(a, b Key) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// KeyNames returns the name of every known key, sorted. This is what an
// UnknownKeyError carries, so a caller can print the list without walking Keys.
func KeyNames() []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.Name)
	}
	slices.Sort(out)
	return out
}

// lookup finds a key by name.
func lookup(name string) (Key, bool) {
	for _, k := range keys {
		if k.Name == name {
			return Key{
				Name:        k.Name,
				Kind:        k.Kind,
				Default:     k.Default,
				Sentinels:   slices.Clone(k.Sentinels),
				Provisional: k.Provisional,
			}, true
		}
	}
	return Key{}, false
}

// UnknownKeyError is returned when a name is not a setting in this build. It
// carries the full known-key list because acceptance item 4 puts the rejection
// in this package and leaves T007 with nothing to do but print it.
type UnknownKeyError struct {
	// Key is the name that was not recognised.
	Key string
	// Known is every key name this build does recognise, sorted.
	Known []string
}

func (e *UnknownKeyError) Error() string {
	return fmt.Sprintf("unknown key %q; known keys: %s", e.Key, strings.Join(e.Known, ", "))
}

// InvalidValueError is returned when a value does not fit its key.
//
// The value and the reason are separate fields on purpose. The reason never
// quotes the value, so a caller reporting a bad value out of a file can report
// the reason alone — this package does not echo file content, and Reason is the
// one string in it that would otherwise be able to.
type InvalidValueError struct {
	// Key is the setting the value was offered for.
	Key string
	// Value is the offered value. It is empty when the caller supplied
	// something this package must not echo, such as a repository label.
	Value string
	// Reason states what the key requires, without quoting what it was given.
	Reason string
}

func (e *InvalidValueError) Error() string {
	if e.Value == "" {
		return fmt.Sprintf("invalid value for %s: %s", e.Key, e.Reason)
	}
	return fmt.Sprintf("invalid value %q for %s: %s", e.Value, e.Key, e.Reason)
}

// validate checks a canonical string value against its key.
//
// It is the single validation entry point Set and Load both go through, so a
// value rejected on the way in is the same value reported as a problem on the
// way out — a value that only one of the two paths accepts is how a config file
// ends up meaning something the command could never have written.
func validate(k Key, value string) error {
	switch k.Kind {
	case KindDuration:
		if slices.Contains(k.Sentinels, value) {
			return nil
		}
		if _, err := parseDuration(value); err != nil {
			return &InvalidValueError{Key: k.Name, Value: value, Reason: durationReason(k, err)}
		}
		return nil
	case KindStringList:
		// Every string is a comma-separated list, including the empty one,
		// which means none rather than all (ADR-0019 §2). There is nothing to
		// reject here: the membership of the list is a question for whoever
		// consumes it — an unknown harness name is T005's to report, not a
		// syntax error.
		return nil
	default:
		// A Key with no Kind: a registration bug, not a user error. Reported
		// rather than treated as the lenient kind.
		return &InvalidValueError{Key: k.Name, Reason: "this key has no value kind; it is registered wrongly"}
	}
}

// durationReason renders why a duration was rejected, without quoting it. The
// specific failure comes from parseDuration's sentinel errors rather than from
// its message, which embeds the offending value.
func durationReason(k Key, err error) string {
	reason := "must be a duration such as 30m, 24h or 30d"
	switch {
	case errors.Is(err, errNegativeDuration):
		reason = "must not be a negative duration"
	case errors.Is(err, errDurationOutOfRange):
		reason = "is too large to be a duration"
	}
	if len(k.Sentinels) > 0 {
		reason += ", or one of: " + strings.Join(k.Sentinels, ", ")
	}
	return reason
}
