package config

import (
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/keyeddigest"
)

// globalRootMACDomain separates the boundary digest from every other use of the salt,
// for the reason nameKeyDomain gives: two keyed values over one key with no domain
// label are one substitution away from being confused for each other (ADR-0020).
//
// Here the substitution is the whole attack. A boundary consents everything under it,
// so a recorded entry's digest pasted into global_root — over a root that entry
// legitimately carries — would verify, and every repository under that root would be
// consented by an edit that added no entry and broke no other rule.
const globalRootMACDomain = "wake/global-root/v1"

// ErrOutsideGlobalRoot refuses a registration whose discovered root left the bound the
// admission test rests on: the boundary, for a directory the boundary strictly
// encloses, and the worktree the probe named, for a linked worktree of a consented
// repository (ADR-0044 §1). Every directory on a machine with no boundary recorded is
// outside it too. It names the requirement and never the directory, which is
// repository content (plan §4.2).
//
// It is not the answer for a directory the boundary does not enclose. Since ADR-0044
// widened the candidate set to every unmatched working directory, that question is the
// second arm's and its answer is ErrNotAnAdmittedWorktree — a scan counts the two
// differently, because only one of them describes collection that was lost.
var ErrOutsideGlobalRoot = errors.New("a discovered repository root outside what the recorded consent covers is never registered")

// ErrNotAnAdmittedWorktree refuses a directory outside the recorded boundary that the
// admission test's second arm does not admit: one that is not a linked worktree at
// all, or one whose repository this machine has not consented (ADR-0044 §1). It names
// the requirement and never the directory (plan §4.2).
//
// It is separate from ErrOutsideGlobalRoot because the two are different facts and a
// scan counts them differently. This one describes a directory nobody consented, and
// since ADR-0044 made every unmatched working directory a candidate it is the ordinary
// answer for the many that are not worktrees — counting it as a refusal would report
// lost collection on every machine that has ever run a session outside its boundary.
// ErrOutsideGlobalRoot describes a directory the recorded consent does cover, and that
// one is lost collection (health.Scan.BoundaryRefused, plan §3.3, §12).
var ErrNotAnAdmittedWorktree = errors.New("a directory outside the collection boundary is registered only as a linked worktree of a consented repository")

// ErrDiscoveredDirectoryGone refuses a discovered directory that is no longer there.
// It is separate from ErrOutsideGlobalRoot because the two are different facts and a
// scan counts them separately: this one is an honest zero — there is nothing left
// there to read — and the other is a refusal.
var ErrDiscoveredDirectoryGone = errors.New("a discovered directory that no longer exists is never given a root")

// globalRootEntry is the machine-wide collection boundary as it is recorded.
//
// It is not a projectEntry and deliberately carries none of one's fields: it has no
// id, because a boundary is never an identity and nothing resolves to it, and no
// label, because nothing displays it. What it does carry is the same two facts
// derivation cannot re-derive without touching the disk — the canonical spelling and
// whether the filesystem under it folds case — plus the digest that makes editing it
// a refusal rather than a widening.
type globalRootEntry struct {
	// Root is the canonical boundary — absolute, clean, and with symlinks already
	// resolved when it was recorded.
	Root string `json:"root"`
	// CaseInsensitive records what the filesystem under Root did at the time, for the
	// reason projectEntry.CaseInsensitive gives: ADR-0019 §5 folds case only where the
	// filesystem does, and derivation cannot probe for it.
	CaseInsensitive bool `json:"case_insensitive"`
	// MAC is the keyed digest over Root and the folding flag. Without it a boundary
	// hand-widened to a parent directory would consent every repository under that
	// parent, with no error and no counter — the widening this field exists to catch.
	MAC string `json:"mac"`
}

// valid reports whether the boundary is one this build is willing to honour. Failing
// closed, like projectEntry.valid: a boundary that cannot be trusted is absent, never
// repaired, because a repaired boundary consents a set of repositories nobody chose.
func (g globalRootEntry) valid() bool {
	return validRoot(g.Root) && g.MAC != ""
}

// globalRootInput is the digest input, NUL-separated in the same injective style as
// matchInput: neither a path nor the one-byte folding flag can contain a NUL, so no
// two different boundaries encode to the same input.
func globalRootInput(g globalRootEntry) []byte {
	buf := append([]byte(globalRootMACDomain), 0)
	if g.CaseInsensitive {
		buf = append(buf, 'f')
	}
	buf = append(buf, 0)
	buf = append(buf, g.Root...)
	return append(buf, 0)
}

// globalRootMAC is the keyed digest this build derives for a boundary.
//
// Through keyeddigest.Sum rather than a construction of its own (ADR-0022), and not
// truncated, for matchMAC's stated reason: the value is never printed and never
// persisted anywhere but the local table, so there is no brevity to trade the margin
// for.
func (r *Repos) globalRootMAC(g globalRootEntry) string {
	return hex.EncodeToString(keyeddigest.Sum(r.salt, globalRootInput(g)))
}

// signedGlobalRoot returns the boundary with the digest this build derives for it.
// Every write site goes through it, so a boundary recorded without a digest cannot
// happen — one missing a digest is refused on the next read, which would be a
// `wake init --global` that looked successful and then consented nothing.
func (r *Repos) signedGlobalRoot(g globalRootEntry) globalRootEntry {
	g.MAC = r.globalRootMAC(g)
	return g
}

// trustedGlobalRoot returns the boundary only when this build derives its digest.
//
// A boundary that does not verify is absent, never narrowed to something safer and
// never widened: absent is the fail-closed answer, because every directory is then
// outside it and nothing new is consented. A constant-time compare, for the reason
// trustworthy gives.
func (r *Repos) trustedGlobalRoot(g *globalRootEntry) (*globalRootEntry, bool) {
	if g == nil || !g.valid() {
		return nil, false
	}
	if !hmac.Equal([]byte(g.MAC), []byte(r.globalRootMAC(*g))) {
		return nil, false
	}
	return g, true
}

// GlobalRootState is what `doctor` may say about the boundary: two flags and a count.
//
// No path, not even the one the user typed. `doctor` output is what people paste into
// issues (ADR-0019 §7), and the distinction requirement 8 asks for — "no boundary
// set" versus "a boundary is set and nothing has been discovered yet" — needs a flag
// and a number, never a directory.
type GlobalRootState struct {
	// Set reports that a boundary this build honours is recorded.
	Set bool
	// Refused reports that one is recorded and this build will not honour it, which is
	// the fail-closed state made visible rather than silent: the boundary is treated as
	// absent, and a user whose repositories stopped being registered needs the reason.
	Refused bool
	// Discovered counts the recorded repositories the boundary strictly encloses. Zero
	// with Set true is the honest "nothing yet"; it is not the same answer as Set false.
	Discovered int
}

// GlobalRootState reports on the boundary in the snapshot this Repos opened with.
func (r *Repos) GlobalRootState() GlobalRootState {
	state := GlobalRootState{Refused: r.boundaryRefused}
	boundary := r.table.GlobalRoot
	if boundary == nil {
		return state
	}
	state.Set = true
	for _, entry := range r.table.Projects {
		// The union of the two flags, which is the conservative direction nestedWith
		// takes: the boundary and an entry may sit on different filesystems, and counting
		// a root the boundary encloses under either rule overstates nothing — the count
		// is about the boundary, and both spellings are under it.
		if strictlyEncloses(boundary.Root, entry.Root, boundary.CaseInsensitive || entry.CaseInsensitive) {
			state.Discovered++
		}
	}
	return state
}

// GlobalRootStateFor reads the boundary state without opening a Repos.
//
// It exists because `doctor` is its caller and `doctor` writes nothing: ADR-0010 makes
// `init` the only operation that writes, so this reads the salt rather than creating
// one on first need. A machine with no salt has no boundary either — nothing could
// have signed one — so the zero state is the whole answer.
//
// A salt this build cannot use leaves the boundary unverifiable, which is the refused
// state rather than an error: a recorded boundary that cannot be checked is one this
// build will not honour, and saying so is the point of the flag.
func GlobalRootStateFor(p Paths) (GlobalRootState, error) {
	salt, saltErr := readSalt(p)
	if saltErr != nil {
		table, _, err := readProjects(p.ProjectsFile)
		if err != nil {
			return GlobalRootState{}, err
		}
		// No salt and no boundary is a fresh install, which is the zero state rather
		// than a refusal — the flag falls out of the same expression, since nothing can
		// have signed a boundary on a machine with no salt.
		return GlobalRootState{Refused: table.GlobalRoot != nil}, nil
	}

	r := &Repos{paths: p, salt: salt}
	table, dropped, refused, err := r.readTable()
	if err != nil {
		return GlobalRootState{}, err
	}
	r.table, r.dropped, r.boundaryRefused = table, dropped, refused
	return r.GlobalRootState(), nil
}

// DefaultGlobalRoot returns the boundary `wake init --global` records when the user
// names none: the home directory, which is the invocation the feature exists for.
//
// It lives here rather than in internal/cli for ADR-0001's reason, and the same one
// that keeps os.Getwd inside DiscoverRootForRegistration: which directory gets
// consented is a decision, and internal/cli only parses and prints. Nothing under
// internal/cli resolves the home directory —
// TestInternalCliResolvesNoHomeDirectory is the mechanical guard — so the default has
// to be resolvable by name from up there.
//
// It creates nothing and records nothing. SetGlobalRoot is what records a boundary,
// and the caller has a disclosure to print between the two.
func DefaultGlobalRoot() (string, error) {
	return os.UserHomeDir()
}

// WithinGlobalRoot reports whether the recorded boundary strictly encloses cwd.
//
// A pure string operation over the snapshot: no os.Stat, no EvalSymlinks, no git
// (ADR-0019 §1). It runs on the derivation path — once for every working directory a
// scan finds no recorded entry for — and a check that read the disk would make the
// same scan answer differently depending on what had been deleted since.
//
// Strictly: cwd exactly at the boundary is outside it. The boundary encloses roots and
// is never one (ADR-0032 §1), and registering the directory the user stood in — most
// often the home directory — would enclose every repository the boundary later
// discovers, which ADR-0019 §5's nested-root refusal would then refuse.
//
// With no boundary recorded the answer is always false, so the discovery this gates
// collects nothing and a scan on a machine that never ran `init --global` walks once.
func (r *Repos) WithinGlobalRoot(cwd string) bool {
	boundary := r.table.GlobalRoot
	if boundary == nil {
		return false
	}
	cleaned, err := lexicalClean(cwd)
	if err != nil {
		return false
	}
	return strictlyEncloses(boundary.Root, cleaned, boundary.CaseInsensitive)
}

// OfferableUnderGlobalRoot reports whether a working directory no recorded entry
// matched is one the walk should offer to RegisterUnderGlobalRoot after it finishes.
//
// This is the *candidate* test, not the admission test, and the two are deliberately
// different since ADR-0044. Admission has two arms (ADR-0044 §1): the boundary path
// test, and — only after it fails — a linked worktree whose repository is consented.
// Where a linked worktree lives is a property of whichever tool manages them, so no
// path test can tell one from any other directory; with a boundary recorded every
// unmatched directory is therefore a candidate, and RegisterUnderGlobalRoot decides.
//
// Observing is still not registering (ADR-0032 §5). This is a pure string operation
// over the snapshot — no os.Stat, no EvalSymlinks, no git — because it runs on the
// derivation path, once per working directory a scan finds no recorded entry for
// (ADR-0019 §1, ADR-0044 §4). The git question is asked after the walk, once, in
// RegisterUnderGlobalRoot.
//
// With no boundary recorded the answer is always false, so a scan on a machine that
// never ran `init --global` walks once and pays for nothing.
//
// On a machine that did, the cost of the widening is real and repeated, and it is
// accepted rather than overlooked: a directory this offers that is not a worktree
// records nothing, so every distinct historical working directory outside the boundary
// pays one bounded probe on every scan rather than once. It stays within what
// ADR-0044 §2 allows — at most one probe per directory per attempt — and it is paid in
// the detached child rather than in the hook the user waits on (plan §4.1.1).
// Remembering the refusals to avoid re-probing would mean recording state about
// directories nobody consented, which is the population DG-114 is about and not a
// question this decision settles.
func (r *Repos) OfferableUnderGlobalRoot(cwd string) bool {
	// The boundary arm, kept explicit rather than folded away: it is the arm
	// ADR-0044's Consequences require a future change to name, and it keeps
	// WithinGlobalRoot the one place the enclosure rule is spelled.
	if r.WithinGlobalRoot(cwd) {
		return true
	}
	return r.table.GlobalRoot != nil
}

// SetGlobalRoot records the machine-wide collection boundary, replacing any earlier
// one.
//
// It deliberately does not consult nestedWith. A boundary legitimately encloses many
// consented roots — that is what it is for — and refusing one for enclosing a
// repository the user already consented would refuse the feature outright on any
// machine that has run `wake init` (ADR-0032 §1 against ADR-0019 §5, which is a rule
// about two identities overlapping and not about consent enclosing an identity).
//
// The three disk-dependent answers are taken before the lock, exactly as Register
// takes its two: they are about the offered directory rather than about the table.
// Nothing else about the table changes, so a boundary recorded on a machine with
// repositories already consented leaves every one of their identities where it was.
func (r *Repos) SetGlobalRoot(root string) error {
	given, err := lexicalClean(root)
	if err != nil {
		// The message names the requirement and never the path: a boundary is a
		// directory of the user's and this error reaches the same terminal as
		// everything else (plan §4.2).
		return fmt.Errorf("a collection boundary %w", err)
	}
	canonical, err := canonicalRoot(given)
	if err != nil {
		return err
	}
	fold, err := caseInsensitive(canonical)
	if err != nil {
		return err
	}

	return withProjectsLock(r.paths.ProjectsFile, func() error {
		// The re-read is the point of the lock, for Register's reason: deciding from the
		// snapshot this Repos opened with and then republishing the whole file would erase
		// any entry another writer recorded since.
		table, dropped, _, readErr := r.readTable()
		if readErr != nil {
			return readErr
		}
		entry := r.signedGlobalRoot(globalRootEntry{Root: canonical, CaseInsensitive: fold})
		if !entry.valid() {
			return errUnreadableEntry
		}
		table.GlobalRoot = &entry
		if writeErr := writeProjects(r.paths.ProjectsFile, table); writeErr != nil {
			return writeErr
		}
		// The refusal flag goes to false rather than to what the re-read found: the
		// boundary just written is one this build derived, so whatever was refused a
		// moment ago is no longer in the file.
		r.table, r.dropped, r.boundaryRefused = table, max(r.dropped, dropped), false
		return nil
	})
}

// RegisterUnderGlobalRoot records the repository a discovered working directory
// belongs to, under its own identity.
//
// This is the one caller ADR-0032 §2 adds to root discovery, and it is registration
// rather than derivation: the resolver that observed the directory did not register it
// — observing is not registering — and this runs after the walk, once, per directory
// the walk saw (ADR-0032 §5).
//
// Discovery is bounded by the boundary twice over: it is handed to git as a ceiling,
// and the root git hands back is then checked against it. The check is what makes the
// guarantee independent of git's environment — the ceiling narrows the exposure and
// does not close it (see boundedDiscoveryEnv and the check below) — and the guarantee
// is that the root can never be the boundary or anything above it, because a
// repository enclosing the boundary would otherwise become the recorded root of
// everything inside it.
//
// It does not take the projects lock. Register does, and withProjectsLock is not
// reentrant — taking it here would deadlock against the write this function exists to
// cause.
//
// from is the instant collection begins, and it is the caller's for the whole batch:
// one scan writes one instant, so two repositories discovered by the same walk cannot
// disagree about when they were consented. It is never zeroed to make a full import
// work — the import is a user-asked scan and ignores the boundary already (ADR-0025).
func (r *Repos) RegisterUnderGlobalRoot(dir string, from time.Time) (string, error) {
	boundary := r.table.GlobalRoot
	if boundary == nil {
		return "", ErrOutsideGlobalRoot
	}
	cleaned, err := lexicalClean(dir)
	if err != nil {
		return "", fmt.Errorf("a discovered directory %w", err)
	}
	if !strictlyEncloses(boundary.Root, cleaned, boundary.CaseInsensitive) {
		// The second arm (ADR-0044 §1), reached only after the path test above has
		// failed — so a directory inside the boundary never pays for it, and a
		// directory outside it pays one bounded probe (ADR-0044 §2).
		return r.registerLinkedWorktree(cleaned, boundary, from)
	}

	root, err := DiscoverRootForRegistration(cleaned, boundary.Root)
	if err != nil {
		// The only refusal discovery raises is a directory that is not there, and it is
		// re-spelled here so a scan can count it as the honest zero it is rather than as
		// a registration that failed.
		if errors.Is(err, errRootNotADirectory) {
			return "", ErrDiscoveredDirectoryGone
		}
		return "", err
	}
	discovered, err := lexicalClean(root)
	if err != nil {
		return "", fmt.Errorf("a discovered repository root %w", err)
	}
	// The check the guarantee actually rests on, and it is about the root rather than
	// about the directory that led to it: the root is what goes into the table as a
	// consented repository, so it is the thing consent has to be true of.
	//
	// The ceiling handed to git is a narrowing, not a proof. git documents two ways it
	// does not bound the walk — GIT_CEILING_DIRECTORIES is a colon-separated list, so a
	// boundary whose own path contains a colon splits into entries that are ancestors
	// of nothing, and an entry git cannot resolve is skipped silently — and both make
	// it answer with a directory above the boundary. Registering that would attribute
	// every repository under the boundary's parent to one id, the identity collapse
	// ADR-0019 §5 keeps the root set non-nested to prevent, and this path is
	// unattended: nobody is asked and nothing is printed.
	//
	// Both spellings are checked, because both can leave the boundary. The canonical
	// one is what Register records (ADR-0019 §5 owns symlink resolution), so a
	// directory under the boundary that resolves outside it would be recorded outside
	// it — consent is about where the repository is, not about how a transcript spelled
	// the way to it.
	canonical, err := canonicalRoot(discovered)
	if err != nil {
		// The root discovery named is no longer there. The honest zero again, and the
		// same one the vanished starting directory gets: there is nothing left to read.
		return "", ErrDiscoveredDirectoryGone
	}
	if !strictlyEncloses(boundary.Root, discovered, boundary.CaseInsensitive) ||
		!strictlyEncloses(boundary.Root, canonical, boundary.CaseInsensitive) {
		return "", ErrOutsideGlobalRoot
	}
	// The discovered spelling rather than the canonical one, so Register records the
	// alias it derives from the difference: a working directory spelled through a link
	// inside the boundary is one later scans have to match without discovering it
	// again.
	return r.Register(discovered, filepath.Base(discovered), from)
}

// registerLinkedWorktree is the admission test's second arm: a directory the boundary
// does not enclose is admitted when it is a linked worktree whose repository is itself
// a consented entry or inside the boundary (ADR-0044 §1). It registers under its own
// identity, carrying the belongs_to relation ADR-0040 defined — Register does both, so
// projects.json gains no field and DG-104's machinery is reused unchanged.
//
// What replaces the boundary as the bound is stronger, not weaker: a consent decision
// the user already made. The ceiling narrows what git may answer; the checks after git
// answers are what the guarantee rests on, exactly as on the first arm.
//
// Every failure names the requirement and never the directory (plan §4.2), and there
// are two of them because a scan counts them differently. Until consent is decided the
// answer is ErrNotAnAdmittedWorktree: nothing here was consented, so nothing is lost by
// turning it away, and this is the ordinary answer for the many directories a widened
// candidate set offers that are not worktrees. After consent is decided the answer is
// ErrOutsideGlobalRoot, the same sentinel the first arm's post-discovery check raises
// and for the same reason — the repository is one the user consented, the sessions in
// its worktree were readable, and a refusal from here is collection that was lost.
//
// ErrDiscoveredDirectoryGone is deliberately not raised here. A worktree directory that
// is gone fails the probe before consent is ever decided, since git cannot answer about
// a directory that is not there, so it is turned away as not admitted rather than
// counted as an honest zero — and a directory outside the boundary that cannot be read
// was never going to be collected.
func (r *Repos) registerLinkedWorktree(cleaned string, boundary *globalRootEntry, from time.Time) (string, error) {
	// One bounded probe, and the only one a directory that is not a linked worktree
	// pays for the question (ADR-0044 §2).
	topLevel, parentSpellings := discoverLinkedWorktreeForRegistration(cleaned)
	if topLevel == "" {
		return "", ErrNotAnAdmittedWorktree
	}
	// Checked after git answers, against the recorded table — never trusted from
	// git's environment (ADR-0044 §2). This is where consent is decided, and it is the
	// line the two sentinels fall on: everything above answers about a directory
	// nobody consented, everything below about a repository the user did.
	if !r.consentsRepository(parentSpellings, boundary) {
		return "", ErrNotAnAdmittedWorktree
	}
	// The ceiling ADR-0044 §2 requires: the walk may not go above the worktree's own
	// top level. GIT_CEILING_DIRECTORIES names directories git will not chdir up
	// *into*, so naming the top level itself would stop the walk one directory short
	// and a registration from a subdirectory of the worktree would find nothing; the
	// top level's parent is how "this far and no further" is spelled.
	root, err := DiscoverRootForRegistration(cleaned, filepath.Dir(topLevel))
	if err != nil {
		return "", ErrOutsideGlobalRoot
	}
	discovered, err := lexicalClean(root)
	if err != nil {
		return "", ErrOutsideGlobalRoot
	}
	canonical, err := canonicalRoot(discovered)
	if err != nil {
		return "", ErrOutsideGlobalRoot
	}
	canonicalTop, err := canonicalRoot(topLevel)
	if err != nil {
		return "", ErrOutsideGlobalRoot
	}
	// The check the guarantee rests on, and it is about the root rather than about the
	// probe: what git answered under the ceiling must be the worktree the probe named,
	// whatever either call's environment was persuaded to say. Canonical on both
	// sides, so one comparison covers every spelling (ADR-0019 §5 owns symlink
	// resolution).
	if canonical != canonicalTop {
		return "", ErrOutsideGlobalRoot
	}
	// A root at or above the boundary would become the recorded root of everything
	// inside it — the identity collapse the first arm's check exists to prevent, and
	// what ADR-0044 §2 means by a ceiling that is "not the boundary and not the
	// filesystem root". Both spellings, because both can reach it.
	if hasPathPrefix(boundary.Root, discovered, boundary.CaseInsensitive) ||
		hasPathPrefix(boundary.Root, canonical, boundary.CaseInsensitive) {
		return "", ErrOutsideGlobalRoot
	}
	// The discovered spelling rather than the canonical one, for the reason the first
	// arm gives: Register records the alias it derives from the difference.
	return r.Register(discovered, filepath.Base(discovered), from)
}

// consentsRepository reports whether the repository these spellings name is one this
// machine has consented: an entry already recorded, or a directory the boundary
// encloses — ADR-0044 §1's disjunction.
//
// The two disjuncts weigh the spellings differently, and deliberately. A recorded
// entry matches under any of them, because an entry is a consent decision the user
// made about a repository and the alias is how the same repository is recognised
// through its other spelling (ADR-0019 §5). The boundary is the inferred disjunct, so
// it requires every spelling to be inside: git can name a repository through a path
// lexically inside the boundary while it physically lives outside, and the first arm
// refuses exactly that repository — checking both the discovered and the canonical
// spelling against the boundary — because Register records the canonical root and
// consent is about where the repository is. ADR-0044 §2 replaces the boundary ceiling
// here with a consent decision the user already made and calls the replacement
// stronger, not weaker; admitting a parent the first arm would turn away is the one
// way this arm could be weaker instead. No spellings at all is no consent, rather than
// the vacuous truth the loop alone would answer.
//
// Pure over the snapshot and over the answer git already gave. The snapshot rather
// than a re-read, exactly as the boundary itself is read above; Register re-reads
// under the lock, and ADR-0040 §2 decides what happens if the parent is not an entry
// by then — the relation is refused and the registration is not.
func (r *Repos) consentsRepository(spellings []string, boundary *globalRootEntry) bool {
	if entryIDForRoot(r.table.Projects, spellings) != "" {
		return true
	}
	if len(spellings) == 0 {
		return false
	}
	for _, spelling := range spellings {
		if !strictlyEncloses(boundary.Root, spelling, boundary.CaseInsensitive) {
			return false
		}
	}
	return true
}

// strictlyEncloses reports whether inner is a directory inside outer, and not outer
// itself. Both are assumed lexically clean.
//
// It folds by the same simple ASCII rule as hasPathPrefix, and for the reason given
// there: adequate for the path elements this compares, and the alternative is an
// x/text dependency in a tool whose auditable dependency list is itself a feature.
func strictlyEncloses(outer, inner string, fold bool) bool {
	if !hasPathPrefix(inner, outer, fold) {
		return false
	}
	if fold {
		// Folded in the assignment, exactly as hasPathPrefix folds, and by ToLower
		// rather than EqualFold: the two halves of one predicate have to fold by one
		// rule, and EqualFold folds Unicode where hasPathPrefix folds ASCII. Two rules
		// would let the pair disagree about whether two spellings are one directory.
		inner, outer = strings.ToLower(inner), strings.ToLower(outer)
	}
	return inner != outer
}
