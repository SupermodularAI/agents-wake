// Package repolabel projects a repository id onto the name a local renderer
// shows for it. It is presentation only: the id stays the key everywhere
// (ADR-0019 §3), and local display of the readable name is the decided purpose
// of the local map (ADR-0014 § Decision).
package repolabel

import "github.com/SupermodularAI/agents-wake/internal/record"

// Labels maps a repository id to the readable label recorded for it on this
// machine. It is resolved by internal/cli through config.ProjectLabels and
// handed in, because a renderer may not read the file it comes from: this
// package imports internal/record and nothing else.
//
// A nil map is a valid value and means no repository has a label.
type Labels map[string]string

// idPrefix is how much of an unlabelled repository's id a column shows. The
// full id stays 32 characters (ADR-0019 §8); 12 is a column width, and enough
// to keep every repository on one machine distinguishable.
const idPrefix = 12

// Display returns what a repository column shows for repo.
//
// Never blank and never invented (ADR-0007; ADR-0033 §3; plan §4.5 as DG-93
// applies it). Three cases, in order:
//
//   - no repository at all — the inventory grain has none (ADR-0002), so the
//     cell is a deliberate dash rather than the empty string.
//   - a recorded label that survives record.BoundedToken unchanged — shown.
//     The check is not redundant with config.readProjects' floor: a label
//     reaches a terminal here, and a control or escape sequence inside one
//     would rewrite the table around it.
//   - anything else — the id, prefixed so it reads as an id and not as a
//     project someone named "0123456789ab".
func (l Labels) Display(repo record.Hash) string {
	if repo == "" {
		return "-"
	}
	if raw, recorded := l[string(repo)]; recorded {
		// BoundedToken trims, so the equality matters: showing the trimmed form
		// would repair a value that failed the rule instead of refusing it. Same
		// reasoning as internal/remote's labelFor.
		if label, err := record.BoundedToken(raw); err == nil && string(label) == raw {
			return raw
		}
	}
	id := string(repo)
	if len(id) > idPrefix {
		id = id[:idPrefix]
	}
	return "repo-" + id
}
