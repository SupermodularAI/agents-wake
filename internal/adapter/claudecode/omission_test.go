package claudecode

import (
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// The set is asserted in both directions. A membership test that only checked the
// members would pass just as well for a negative exception ("everything but Bash"),
// which is the form ADR-0005 rejects — so the non-members are the half of this table
// that carries the guarantee.
func TestOmitsOnSuccessIsAClosedMeasuredSet(t *testing.T) {
	for _, testCase := range []struct {
		label string
		kind  record.Kind
		name  record.Identifier
		want  bool
	}{
		{label: "measured builtin Read", kind: record.KindBuiltinTool, name: "Read", want: true},
		{label: "measured builtin Edit", kind: record.KindBuiltinTool, name: "Edit", want: true},
		{label: "measured builtin Write", kind: record.KindBuiltinTool, name: "Write", want: true},
		{label: "measured builtin Grep", kind: record.KindBuiltinTool, name: "Grep", want: true},
		{label: "measured builtin Glob", kind: record.KindBuiltinTool, name: "Glob", want: true},

		// The three-state family: 929 explicit false, zero absences. An absence is
		// outside its observed vocabulary and stays unknown (ADR-0005).
		{label: "three-state Bash", kind: record.KindBuiltinTool, name: "Bash", want: false},
		// The one other tool measured to spell an explicit false.
		{label: "explicit-false SendFeedback", kind: record.KindBuiltinTool, name: "SendFeedback", want: false},

		// Unmeasured is not measured-never. Each of these would be handed a default ok
		// by a negative exception, which is the failure mode the closed set exists to
		// prevent.
		{label: "unmeasured TodoWrite", kind: record.KindBuiltinTool, name: "TodoWrite", want: false},
		{label: "unmeasured WebFetch", kind: record.KindBuiltinTool, name: "WebFetch", want: false},
		{label: "unmeasured NotebookEdit", kind: record.KindBuiltinTool, name: "NotebookEdit", want: false},
		{label: "unmeasured empty name", kind: record.KindBuiltinTool, name: "", want: false},

		// Task is the one row that is outside the set for a reason other than the
		// measurement: it was measured (44 absences, zero explicit false) and is still
		// excluded, because a subagent invocation is skipped before a call is built and
		// never reaches this function at all. Membership would be inert rather than
		// wrong — the set stays to families this gate can actually decide
		// (ADR-0023 §3, ADR-0036 §2-§3).
		{label: "Task never reaches this function", kind: record.KindBuiltinTool, name: "Task", want: false},

		// MCP membership is by kind, so both the bare and the plugin-scoped spelling of
		// a server's tool are members. Per-server allowlisting could not hold: the names
		// are not stable enough to enumerate, and a server nobody has run yet would be
		// excluded for no measured reason.
		{label: "mcp bare spelling", kind: record.KindMCPTool, name: "mcp__atlassian__search", want: true},
		{label: "mcp plugin-scoped spelling", kind: record.KindMCPTool, name: "mcp__plugin_playwright_playwright__browser_click", want: true},

		// Skill membership is decided by kind, never by name: primitiveName puts the
		// derived skill name on call.name, so "Skill" is not what arrives here and a
		// name-keyed gate would miss the whole family.
		{label: "skill by derived name", kind: record.KindSkill, name: "pr-review", want: true},
		{label: "skill by another derived name", kind: record.KindSkill, name: "some-other-skill", want: true},

		// Nothing without a completion boundary may acquire an outcome here
		// (ADR-0023 §3, ADR-0036 §2-§3). These kinds fall to the default arm, which is
		// what keeps that structural rather than incidental.
		{label: "subagent has no completion boundary", kind: record.KindSubagent, name: "reviewer", want: false},
		{label: "command has no completion boundary", kind: record.KindCommand, name: "/review", want: false},
		{label: "session end has no completion boundary", kind: record.KindSessionEnd, name: "session", want: false},
		{label: "plugin has no completion boundary", kind: record.KindPlugin, name: "superpowers", want: false},
		{label: "unset kind", kind: "", name: "Read", want: false},
	} {
		t.Run(testCase.label, func(t *testing.T) {
			if got := omitsOnSuccess(testCase.kind, testCase.name); got != testCase.want {
				t.Errorf("omitsOnSuccess(%q, %q) = %v, want %v", testCase.kind, testCase.name, got, testCase.want)
			}
		})
	}
}
