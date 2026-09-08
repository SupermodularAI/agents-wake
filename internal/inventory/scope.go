package inventory

// ProjectScope is the consent answer for the working directory a discovery pass
// was asked about. It is an answer, never a path: the working directory itself
// never reaches this type (plan §3.4).
type ProjectScope string

const (
	// ProjectConsented means the directory resolved to a repository the user
	// consented to with wake init (ADR-0010, ADR-0019 §9).
	ProjectConsented ProjectScope = "consented"
	// ProjectUnconsented means it did not. Project-local configuration is not read.
	ProjectUnconsented ProjectScope = "unconsented"
	// ProjectUnresolved means consent could not be resolved at all — an unreadable
	// salt or resolution table. It is treated exactly as unconsented: an error
	// path must never default to scanning (plan §3.4, fail closed).
	ProjectUnresolved ProjectScope = "unresolved"
)

// Scope is which Claude Code discovery paths one invocation may read.
//
// Global discovery always runs. Project-local discovery runs only when Project is
// ProjectConsented, which is why the two are separate call paths rather than one
// function with a flag: the consent boundary has to be visible at the call site
// and assertable in a test with no stdout (ADR-0001, plan §0.2).
type Scope struct {
	// ClaudeDir is the harness's own directory — ~/.claude, or another one for
	// the hook path.
	ClaudeDir string
	// Root is the consented working directory. It is empty unless Project is
	// ProjectConsented, and it is ignored when it is not.
	Root string
	// Project is the consent answer for Root.
	Project ProjectScope
}

// allowsProject reports whether project-local discovery may run.
func (s Scope) allowsProject() bool { return s.Project == ProjectConsented && s.Root != "" }

// Discovery is the result of one discovery pass: what it found, and whether it was
// allowed to look everywhere it would have had to look to be sure.
//
// The second half travels with the first because the snapshot writer needs both: an
// entry missing from a pass that could not read a consented project is out of
// scope, not gone, and dropping it would erase a repository's primitives and their
// counters (Store.available). Pairing them in one value is what stops a caller from
// reporting a partial pass as a complete one.
type Discovery struct {
	// Primitives is everything this pass was allowed to see.
	Primitives []Primitive
	// ProjectScanned reports that project-local discovery ran. False is the
	// conservative answer — the zero Discovery is treated as partial — because
	// wrongly claiming completeness deletes recorded state, while wrongly claiming
	// partialness only keeps a name around one refresh longer.
	ProjectScanned bool
	// canonical is the fold from a spelling discovery found onto the one the harness
	// actually invokes the primitive under — a plugin skill's bare directory name onto
	// "<plugin>:<name>", or a plugin command's bare name onto the namespaced spelling a
	// session listing declared it a skill under. Its keys and its values are inventory
	// identities; derive applies it to both sides of its join.
	//
	// The value carries a kind as well as a name, because a plugin primitive Claude
	// Code lists as a skill is discovered from a commands/ directory under a different
	// kind. Where a session listing declared the kind, that declaration is the kind the
	// folded row records; where nothing declared it, the fold leaves the kind exactly as
	// discovery found it (ADR-0041).
	//
	// It is private, and the two constructors above are the only way to fill it, for
	// the same reason claudecode.NewInstalled keeps its map private: the fold is only
	// sound where discovery proved the two spellings are one primitive, and a caller
	// handed a public map could assert that without holding the proof (ADR-0020).
	//
	// Primitives keeps both spellings on purpose. That slice is what activation hands
	// claudecode.NewInstalled, and folding it would stop wake collecting a person who
	// types the bare form — a wrong report turned into lost collection. One row out,
	// both spellings still admitted in.
	canonical map[identity]identity
}
