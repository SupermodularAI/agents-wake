package claudecode

import "github.com/SupermodularAI/agents-wake/internal/record"

// builtinsOmittingOnSuccess is the closed set of Claude Code built-in tools measured
// never to write an explicit is_error: false. Membership is data rather than a branch
// so the set can be asserted in both directions. Go has no immutable map, so "never
// written after initialisation" is an invariant this package keeps rather than one the
// type enforces; the set is unexported and read from exactly one place.
var builtinsOmittingOnSuccess = map[record.Identifier]struct{}{
	"Read":  {},
	"Edit":  {},
	"Write": {},
	"Grep":  {},
	"Glob":  {},
}

// omitsOnSuccess reports whether this tool family was measured never to spell
// is_error: false — in which case an omitted field on its tool_result is the
// family's own success token rather than the source saying nothing.
//
// It is only ever asked about a result line outcomeFor found clean: no denial kind of
// any spelling, no interrupted flag, is_error absent. A denial kind the switch there
// could not name is a failure marker rather than an omission, so it never reaches this
// function and its verdict stays unknown (ADR-0005).
//
// That distinction is the whole of the function. Mapping a harness's vocabulary onto
// the outcome enum is what ADR-0005's consequences require of an adapter; guessing at
// a source that has no way to say is what they forbid. A family that spells failure
// with is_error: true and success with nothing at all has a two-state vocabulary, and
// reading its second state is decoding, not inference (ADR-0008 — a lookup).
//
// The evidence, from 60 real Claude Code transcripts with every tool_result correlated
// back to the tool_use it terminated:
//
//	tool                       is_error:false   absent   is_error:true
//	Bash                                  929        0              29
//	Read                                    0      261              11
//	MCP (two servers)                       0      219               8
//	Task                                    0       44               0
//	Skill                                   0       15               0
//	Edit / Write / Grep / Glob              0      135               3
//
// The set is positive and closed, never a negative exception. A family nobody has
// measured has not been measured never to spell false — a built-in Claude Code ships
// next release, an MCP server nobody has run — so it falls to the default arm, and its
// absences stay null (ADR-0005). Spelling this as "everything except Bash" would hand
// every tool the harness adds later a default ok that nobody measured, which is the
// alternative ADR-0005 rejects.
//
// Bash is outside the set because it is the one family with a three-state vocabulary:
// 929 explicit false and not a single absence, so an absence there is outside what it
// has ever been observed to write and is genuinely unknown. SendFeedback is the only
// other tool measured to write an explicit false, and is likewise outside — it is the
// live demonstration that an allowlist pays for itself.
//
// The default arm is load-bearing beyond the unmeasured built-ins: no kind without a
// completion boundary may acquire an outcome here (ADR-0023 §3, ADR-0036 §2-§3). A
// subagent invocation never reaches this function at all — it is skipped before a call
// is built — and nothing in this switch makes it reachable.
//
// The MCP arm is a falsifiable claim about the format, stated as one: two independent
// servers were measured, 219 absences and zero explicit false between them, and the
// class is treated as one family because a server reports failure through the MCP
// result, which the harness spells as is_error: true. If a server is ever measured to
// spell false, this member is wrong — and the drift will surface as a silent ok rather
// than as a rising null rate, which is precisely why membership is pinned by tests
// rather than carried as a comment (ADR-0005).
func omitsOnSuccess(kind record.Kind, name record.Identifier) bool {
	switch kind {
	case record.KindMCPTool, record.KindSkill:
		return true
	case record.KindBuiltinTool:
		_, measured := builtinsOmittingOnSuccess[name]
		return measured
	}
	return false
}
