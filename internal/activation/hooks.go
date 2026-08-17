package activation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
	"github.com/SupermodularAI/agents-wake/internal/config"
)

// hookEvents are the only events Wake registers. SessionStart ingests anything
// pending; SessionEnd ingests the session that just finished. Never PostToolUse:
// the hook says when, the log says what (ADR-0016), and a per-tool-call hook would
// make the trigger part of the measurement instead of a nudge to go and read it.
var hookEvents = []string{"SessionStart", "SessionEnd"}

// markerKey and hooksKey are the only two keys a Wake-owned hook group has. The
// group's key set is compared against exactly these, so a group a user edited is no
// longer one `remove` will touch.
const (
	markerKey = "wake"
	hooksKey  = "hooks"
)

// settingsFileName is the file inside the Claude Code directory that holds hooks.
const settingsFileName = "settings.json"

// settingsFileMode is what a settings file Wake creates starts at. An existing one
// keeps its own mode: it is a file the harness owns, and re-permissioning it would
// be a side effect ADR-0010 does not license.
const settingsFileMode = fs.FileMode(0o600)

// The shapes a settings file can have that this build refuses to edit. Each says
// what was expected and where, and quotes nothing out of the file: a hook command is
// a command line, and plan §4.2 keeps source content out of every error.
var (
	errSettingsNotAnObject  = errors.New("the Claude Code settings file must hold a JSON object")
	errSettingsUnreadable   = errors.New("the Claude Code settings file is not valid JSON")
	errHooksNotAnObject     = errors.New(`the "hooks" setting must hold a JSON object`)
	errHookEventNotAnArray  = errors.New("a hook event must hold an array of hook groups")
	errSettingsNotARegular  = errors.New("the Claude Code settings file is not a regular file; move whatever is in its place aside and run wake init again")
	errSettingsLinkIsBroken = errors.New("the Claude Code settings file is a symbolic link that resolves to no file; restore what it points at, or remove the link, and run wake init again")
)

// The three ways an installation cannot host a hook command. Each names the
// requirement and never the path it refused (plan §4.2), and each is returned
// before anything has been written.
var (
	errHookCommandNotAbsolute   = errors.New("this build's own path is not absolute, so it cannot be used as a Claude Code hook command; install wake at a fixed absolute path and run wake init again")
	errHookCommandNotExecutable = errors.New("this build's own path is not a file the user can execute, so it cannot be used as a Claude Code hook command; install wake as a regular executable file and run wake init again")
	errHookCommandNotShellSafe  = errors.New("this build's own path holds a character a hook command cannot carry unquoted, so it cannot be used as a Claude Code hook command; install wake at a path of letters, digits and the characters . _ - / + @ : only, then run wake init again")
)

// hookCommandFor returns the hook command a documented installation of executable
// leaves behind, or the reason this installation cannot have one.
//
// An absolute path rather than a bare `wake`, because `make build` leaves the
// binary in dist/ and never on PATH (README § Install), so a PATH-dependent hook
// would be broken for one of the two documented installations. Unquoted, and
// therefore restricted to a character set that survives both readings of a hook
// command string — handed to a shell, or split on spaces — because a harness may do
// either and quoting is only safe under one of them.
//
// The resolved path is what gets validated and written, so a symlinked install
// records the real target: a hook whose command is a symlink stops working the day
// the link moves, and it stops working silently.
func hookCommandFor(executable string) (string, error) {
	if executable == "" || !filepath.IsAbs(executable) {
		return "", errHookCommandNotAbsolute
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", errHookCommandNotExecutable
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errHookCommandNotExecutable
	}
	if !shellSafePath(resolved) {
		return "", errHookCommandNotShellSafe
	}
	return resolved + " ingest --quiet", nil
}

// shellSafePath reports whether every byte of path is one an unquoted hook command
// can carry. The set is deliberately narrow: it is the intersection of what a shell
// leaves alone and what surviving a split on spaces requires.
func shellSafePath(path string) bool {
	for i := range len(path) {
		c := path[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '/', c == '+', c == '@', c == ':':
		default:
			return false
		}
	}
	return true
}

// settingsDoc is settings.json decoded just far enough to edit the hooks and hand
// everything else back unchanged.
//
// Both levels are map[string]json.RawMessage, which is what makes an unknown
// top-level setting and an unknown hook event round-trip untouched — the same
// technique config.Set's doc comment explains for TOML. Anything Wake does not
// write is bytes it carries rather than a shape it understands.
type settingsDoc struct {
	path     string
	settings map[string]json.RawMessage
	hooks    map[string]json.RawMessage
	hadHooks bool
}

// settingsFileFor resolves the file the settings document is read from and written
// back to.
//
// A symlink is resolved to its target and the target is what gets published, because
// publication replaces a path with a regular file: publishing over the link itself
// would delete it, so a user whose ~/.claude/settings.json is symlinked into a
// dotfile store — which is what stow, chezmoi and yadm all leave behind — would keep
// the pre-init content in their store and lose every later change to a file nothing
// reads. It would also publish at the mode Lstat reports for the link, 0755 on darwin
// and 0777 on linux, for a file holding commands the harness executes.
//
// A path that is neither a regular file nor a link to one is refused rather than
// replaced: this build cannot know what somebody put there, and the settings file is
// the harness's, not Wake's. A missing path is not a fault — `init` on a fresh
// machine has no settings file, and creating it is what installing a hook does.
func settingsFileFor(claudeDir string) (string, error) {
	path := filepath.Join(claudeDir, settingsFileName)
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return path, nil
	case err != nil:
		return "", fmt.Errorf("reading the Claude Code settings file: %w", err)
	case info.Mode().IsRegular():
		return path, nil
	case info.Mode()&fs.ModeSymlink == 0:
		return "", errSettingsNotARegular
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errSettingsLinkIsBroken
	}
	// Lstat again on the resolved path: EvalSymlinks answers where the link leads,
	// not what is there, and a link to a directory or a socket is as unpublishable as
	// one at the path itself.
	target, err := os.Lstat(resolved)
	if err != nil {
		return "", errSettingsLinkIsBroken
	}
	if !target.Mode().IsRegular() {
		return "", errSettingsNotARegular
	}
	return resolved, nil
}

// readSettings decodes the settings file, or reports the shape it refuses to edit.
//
// A missing file is an empty document rather than an error: `init` on a fresh
// machine has none, and creating it is what installing a hook does. Everything else
// that is not an object is refused, including the JSON literal null — which is the
// case that used to panic. null unmarshals into a map without error and leaves the
// map nil, and the write into it is the crash.
func readSettings(claudeDir string) (settingsDoc, error) {
	path, err := settingsFileFor(claudeDir)
	if err != nil {
		return settingsDoc{}, err
	}
	doc := settingsDoc{
		path:     path,
		settings: map[string]json.RawMessage{},
		hooks:    map[string]json.RawMessage{},
	}
	raw, err := os.ReadFile(doc.path)
	if errors.Is(err, fs.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return settingsDoc{}, fmt.Errorf("reading the Claude Code settings file: %w", err)
	}

	settings, err := decodeObject(raw)
	if err != nil {
		return settingsDoc{}, err
	}
	doc.settings = settings

	rawHooks, found := doc.settings[hooksKey]
	if !found {
		return doc, nil
	}
	hooks, err := decodeObject(rawHooks)
	if err != nil {
		// The fault is the hooks setting's, not the file's, and the caller can act
		// on the difference: one is a file Wake will not touch at all, the other is
		// one setting inside it.
		return settingsDoc{}, errHooksNotAnObject
	}
	doc.hooks, doc.hadHooks = hooks, true
	return doc, nil
}

// decodeObject decodes raw as a JSON object, refusing null and every other kind.
//
// The pointer target is what distinguishes null from an empty object: unmarshalling
// null into a map leaves it nil and reports no error, whereas unmarshalling into a
// *map leaves the pointer nil, which is visible.
func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	if !json.Valid(raw) {
		return nil, errSettingsUnreadable
	}
	var object *map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errSettingsNotAnObject
	}
	return *object, nil
}

// groups returns the hook groups recorded for one event, or the shape it refuses.
// A missing event is no groups and no error; null and every non-array is refused,
// because Wake has to write an array there and silently replacing what somebody
// else put there is the failure this reports instead.
func (d settingsDoc) groups(event string) ([]json.RawMessage, error) {
	raw, found := d.hooks[event]
	if !found {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, errSettingsUnreadable
	}
	var list *[]json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil || list == nil {
		return nil, errHookEventNotAnArray
	}
	return *list, nil
}

// setGroups records the groups for one event, dropping the event when it has none
// left. An event key holding an empty array would be a hook registration Wake left
// behind after removing the only thing in it.
func (d *settingsDoc) setGroups(event string, groups []json.RawMessage) error {
	if len(groups) == 0 {
		delete(d.hooks, event)
		return nil
	}
	encoded, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("encoding the %s hooks: %w", event, err)
	}
	d.hooks[event] = encoded
	return nil
}

// publish writes the document back, preserving the mode of a file it did not
// create.
//
// The hooks setting is removed entirely when nothing is left in it, rather than
// left as an empty object: `remove` has to be able to leave the file as it found
// it, and an empty "hooks": {} is a trace of Wake in a file it was asked to leave.
func (d settingsDoc) publish() error {
	switch {
	case len(d.hooks) > 0:
		encoded, err := json.Marshal(d.hooks)
		if err != nil {
			return fmt.Errorf("encoding the Claude Code hooks: %w", err)
		}
		d.settings[hooksKey] = encoded
	case d.hadHooks:
		delete(d.settings, hooksKey)
	}

	data, err := json.MarshalIndent(d.settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the Claude Code settings: %w", err)
	}
	// ModeOf, not a fixed mode: settings.json belongs to the harness, and 0600 is
	// only what a file Wake itself creates starts at. d.path is a regular file or
	// nothing — settingsFileFor resolved a link to its target and refused anything
	// else — so this is the mode of the file being replaced, never a link's.
	return atomicfile.Publish(d.path, append(data, '\n'), atomicfile.ModeOf(d.path, settingsFileMode))
}

// checkSettingsShape raises every refusal the settings file's shape decides, and
// keeps nothing it read.
//
// It is installHooks' own read, performed early and thrown away, because every one
// of these refusals needs claudeDir and nothing else: raised from inside the
// settings lock they would be raised after consent and scan.repos had been
// written, and a consent record without a trigger is a repository that from then on
// collects only what a manual `wake ingest` picks up.
//
// Nothing is carried across — not the resolved path, not the decoded document.
// installHooks has to decide what to write from the bytes it read while holding the
// settings lock, so a document read out here would be a stale premise dressed as a
// decision. A settings file that changes shape in between is refused by the second
// read exactly as it would have been without this one; that is a lost race, not a
// half-installed state, because nothing has been written when it happens.
func checkSettingsShape(claudeDir string) error {
	doc, err := readSettings(claudeDir)
	if err != nil {
		return err
	}
	for _, event := range hookEvents {
		if _, groupsErr := doc.groups(event); groupsErr != nil {
			return groupsErr
		}
	}
	return nil
}

// installHooks adds Wake's group to every hookEvent, leaving every other hook and
// every unknown setting exactly as it found them, and returns how many groups it
// wrote or corrected.
//
// The read, the merge and the publication happen inside one hold of the settings
// lock: settings.json is republished whole, so a writer that decided what to write
// from an earlier read would erase whatever another Wake process recorded in
// between.
func installHooks(paths config.Paths, claudeDir, command string) (int, error) {
	desired, err := json.Marshal(map[string]any{
		markerKey: true,
		hooksKey:  []map[string]string{{"type": "command", "command": command}},
	})
	if err != nil {
		return 0, fmt.Errorf("encoding Wake's hook group: %w", err)
	}

	written := 0
	lockErr := config.WithClaudeSettingsLock(paths.ConfigDir, func() error {
		written = 0
		doc, readErr := readSettings(claudeDir)
		if readErr != nil {
			return readErr
		}

		changed := false
		for _, event := range hookEvents {
			current, groupsErr := doc.groups(event)
			if groupsErr != nil {
				return groupsErr
			}
			// Wake's own groups are dropped and one fresh group appended, rather
			// than a second one added beside them: two Wake groups would run two
			// scans per session, and re-running `wake init` after moving the binary
			// is how the command gets corrected (ADR-0010's idempotent re-run).
			next := make([]json.RawMessage, 0, len(current)+1)
			for _, group := range current {
				if owned(group) {
					continue
				}
				next = append(next, group)
			}
			next = append(next, desired)
			if sameGroups(current, next) {
				continue
			}
			if setErr := doc.setGroups(event, next); setErr != nil {
				return setErr
			}
			written++
			changed = true
		}
		if !changed {
			return nil
		}
		return doc.publish()
	})
	if lockErr != nil {
		return 0, lockErr
	}
	return written, nil
}

// removeHooks removes Wake's groups and reports how many it removed and how many
// marked groups it deliberately left in place.
//
// A marked group whose definition no longer matches is kept and counted, not
// removed: the user has edited it, and a group holding a third-party hook next to
// Wake's is a group `remove` must not delete. The count is what makes that visible
// through `doctor` (ADR-0010).
//
// Every event is swept, not only hookEvents: ownership is decided by the marker, so
// a group an earlier build registered somewhere this one does not is still Wake's
// to remove.
func removeHooks(paths config.Paths, claudeDir string) (removed, keptOwned int, err error) {
	lockErr := config.WithClaudeSettingsLock(paths.ConfigDir, func() error {
		removed, keptOwned = 0, 0
		doc, readErr := readSettings(claudeDir)
		if readErr != nil {
			return readErr
		}
		if !doc.hadHooks {
			return nil
		}

		// Sorted, because iteration order over a map is random and a sweep that
		// counted in a different order each run would be needlessly hard to reason
		// about when a count comes out wrong.
		for _, event := range slices.Sorted(maps.Keys(doc.hooks)) {
			current, groupsErr := doc.groups(event)
			if groupsErr != nil {
				return groupsErr
			}
			kept := make([]json.RawMessage, 0, len(current))
			for _, group := range current {
				switch {
				case owned(group):
					removed++
				case marked(group):
					keptOwned++
					kept = append(kept, group)
				default:
					kept = append(kept, group)
				}
			}
			if setErr := doc.setGroups(event, kept); setErr != nil {
				return setErr
			}
		}
		if removed == 0 {
			return nil
		}
		return doc.publish()
	})
	if lockErr != nil {
		return 0, 0, lockErr
	}
	return removed, keptOwned, nil
}

// HookState reports how many Wake-owned hook groups the settings file currently
// holds.
//
// It is doctor's read-only counterpart to installHooks: it takes no lock and
// publishes nothing, because a diagnostic must not modify what it reports
// (ADR-0010). Taking no lock is why it needs no config.Paths, unlike every writer
// here — the read is safe on its own, since the file is published by rename and a
// reader sees the old complete file or the new one.
//
// A settings file it cannot read reports zero and the error, which doctor renders
// rather than fails on.
func HookState(claudeDir string) (int, error) {
	doc, err := readSettings(claudeDir)
	if err != nil {
		return 0, err
	}
	installed := 0
	for _, event := range slices.Sorted(maps.Keys(doc.hooks)) {
		groups, groupsErr := doc.groups(event)
		if groupsErr != nil {
			return 0, groupsErr
		}
		for _, group := range groups {
			if owned(group) {
				installed++
			}
		}
	}
	return installed, nil
}

// owned reports whether a hook group is one Wake wrote and nothing has touched
// since.
//
// Three conditions, all required: the object's key set is exactly {wake, hooks};
// wake is the JSON boolean true, not a string, a number, or false; and hooks is
// exactly one object with the key set {type, command}, type equal to "command", and
// a non-empty command string.
//
// The command string itself is never compared. Command equality is not proof of
// ownership — a user who copied Wake's command into their own group owns that
// group — and an ownership test that depended on this build's command form would
// stop recognising groups an earlier build wrote, which is the other half of the
// same mistake.
//
// A group that is not a JSON object is simply not owned, and therefore preserved.
// Only the containers Wake must write into are shape-checked; what somebody else
// put in the array is theirs.
func owned(raw json.RawMessage) bool {
	group, err := decodeObject(raw)
	if err != nil {
		return false
	}
	if len(group) != 2 {
		return false
	}
	var marker *bool
	if err := json.Unmarshal(group[markerKey], &marker); err != nil || marker == nil || !*marker {
		return false
	}

	var hooks []map[string]json.RawMessage
	if err := json.Unmarshal(group[hooksKey], &hooks); err != nil || len(hooks) != 1 {
		return false
	}
	hook := hooks[0]
	if len(hook) != 2 {
		return false
	}
	var kind, command string
	if err := json.Unmarshal(hook["type"], &kind); err != nil || kind != "command" {
		return false
	}
	if err := json.Unmarshal(hook["command"], &command); err != nil || command == "" {
		return false
	}
	return true
}

// marked reports whether a group carries Wake's marker at all, whatever else has
// happened to it. The difference from owned is the whole point: a marked group that
// is not owned is one somebody edited, and reporting how many of those `remove`
// left behind is what makes a partial state visible rather than mysterious.
func marked(raw json.RawMessage) bool {
	group, err := decodeObject(raw)
	if err != nil {
		return false
	}
	_, found := group[markerKey]
	return found
}

// sameGroups reports whether two group arrays are the same JSON, ignoring
// formatting. It is what makes a re-run report nothing written and leave the file
// byte-identical: the groups read back from a published file carry the indentation
// that publication added, so a byte comparison against freshly built ones would
// report a change that is not one.
func sameGroups(left, right []json.RawMessage) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if compactJSON(left[i]) != compactJSON(right[i]) {
			return false
		}
	}
	return true
}

func compactJSON(raw json.RawMessage) string {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		// Not reachable through the callers: every value compared here came out of
		// a document json.Valid already accepted. Returning the raw bytes keeps the
		// comparison conservative — unequal — rather than claiming equality it
		// could not establish.
		return string(raw)
	}
	return out.String()
}
