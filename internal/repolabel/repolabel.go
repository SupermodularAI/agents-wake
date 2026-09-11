// Package repolabel projects a repository id onto the name a local renderer
// shows for it. It is presentation only: the id stays the key everywhere
// (ADR-0019 §3), and local display of the readable name is the decided purpose
// of the local map (ADR-0014 § Decision).
package repolabel

import (
	"strconv"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

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

// Display returns what the PROJECT column shows for repo.
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

// DisplayAll returns what the PROJECT column shows for a row whose invocations
// spanned repos. Three cases, in order:
//
//   - none — Display's deliberate dash: nothing invoked it, so there is no
//     project to name (ADR-0002).
//   - one — Display's answer for it, so a cell that names a project has exactly
//     one definition rather than two.
//   - several — how many, never one of them. Naming one would report it as the
//     only project the primitive was used in, which is the misreport a
//     per-repository row grain used to make (ADR-0042).
//
// The count renders as a single whitespace-free token by construction: the cell
// is read by field position out of a whitespace-separated table, so "4 projects"
// would shift every column after it.
func (l Labels) DisplayAll(repos []record.Hash) string {
	switch len(repos) {
	case 0:
		return l.Display("")
	case 1:
		return l.Display(repos[0])
	default:
		return strconv.Itoa(len(repos)) + "-projects"
	}
}
