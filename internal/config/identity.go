package config

import (
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/keyeddigest"
)

// errPathNotAbsolute rejects a path that cannot be matched against a recorded
// root. It is phrased as the tail of a sentence so a caller can name what was
// wrong — "the working directory must be an absolute path" — without quoting the
// path itself, which is repository content (plan §4.2).
var errPathNotAbsolute = errors.New("must be an absolute path")

// errRootNotADirectory is returned when a root offered for registration is not a
// directory that exists. It names no path for the same reason.
var errRootNotADirectory = errors.New("a repository root must be a directory that exists")

// errUnreadableEntry is the fail-closed refusal: an entry readProjects would drop
// is worse than a refusal, because the registration would look successful and the
// repository would collect nothing (plan §3.4).
var errUnreadableEntry = errors.New("refusing to record a repository this build could not read back")

// Identity is which repository an observed working directory belongs to.
//
// It carries an id and nothing else that could identify a repository: no path, no
// label, no root. That is the point — the struct is the boundary, so no caller can
// print a path it never received (ADR-0007, plan §3.4), and a test asserts these
// two fields are the only ones.
type Identity struct {
	// ID is the repository id: idHexLen lowercase hex characters (ADR-0019 §8).
	ID string
	// Matched reports that the working directory resolved to a consented root.
	//
	// False means it did not, and the id is the hash of the directory itself
	// (ADR-0019 §9): the record is permanently directory-grain rather than
	// repo-grain. T103 counts these so the coarseness is visible instead of
	// silently folded into a neighbouring repository (§7).
	Matched bool
}

// Repos resolves working directories to repository identities, and is the only
// type that can add a repository to the local table.
//
// It holds the salt and the resolution table read at open time. Derivation runs
// against that snapshot and touches no filesystem (ADR-0019 §1) — the id must
// depend on the event, not on the state of the disk when the log is scanned, or
// re-scanning the same log would produce different ids and ADR-0004's dedup would
// keep both.
//
// One *Repos is used from one goroutine at a time. Register's lock serialises
// writers of the file, including writers in other processes, but it does not make
// a single Repos value safe to share between goroutines.
type Repos struct {
	paths   Paths
	salt    []byte
	table   projectsFile
	dropped int
	// boundaryRefused reports that a collection boundary is recorded and this build
	// will not honour it. It is separate from dropped because it is a different fact
	// with a different remedy, and because a refused boundary is not a refused entry:
	// nothing was re-attributed, but nothing new is being registered either, and
	// `doctor` is the only surface that can say so (ADR-0032 §7).
	boundaryRefused bool
}

// OpenRepos loads the salt and the resolution table.
//
// The salt is created on first need and never regenerated; the table is read as
// it is. A table that does not parse is an error rather than an empty table,
// because resolving against an empty table would hand every repository a new
// identity on the next scan.
//
// The two directories are checked before either file is touched, because the mode
// of a file is only as strong as the directory holding it: in a directory anyone
// else can write, the salt can be replaced between one read and the next whatever
// its own mode says. Only those two directories, never their ancestors — see
// checkStateDir. Each refusal names the directory's role and not its path.
func OpenRepos(p Paths) (*Repos, error) {
	if err := checkStateDir(p.ConfigDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the configuration directory %w", err)
	}
	if err := checkStateDir(filepath.Dir(p.ProjectsFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the state directory %w", err)
	}

	salt, err := loadOrCreateSalt(p)
	if err != nil {
		return nil, err
	}
	// The salt first, then a *Repos built with it, then the table: verifying an
	// entry's id means deriving it, and that cannot happen before the salt is
	// known.
	r := &Repos{paths: p, salt: salt}
	table, dropped, boundaryRefused, err := r.readTable()
	if err != nil {
		return nil, err
	}
	r.table, r.dropped, r.boundaryRefused = table, dropped, boundaryRefused
	return r, nil
}

// readTable reads the resolution table and keeps only the entries this build is
// willing to resolve against. It returns the count it refused, which is the sum of
// the entries readProjects dropped on shape and the entries this verified against
// the salt and rejected (ADR-0019 §7).
//
// A collection boundary this build derives the digest of is kept in the returned
// table, so Register's republication carries it forward. One it refuses is nilled and
// reported, which matches the posture Register already takes for an entry: a boundary
// this build will not honour is one it will not carry back into the file either, since
// a table that kept asserting it after it had been rejected is a consent claim nothing
// stands behind.
func (r *Repos) readTable() (projectsFile, int, bool, error) {
	table, dropped, err := readProjects(r.paths.ProjectsFile)
	if err != nil {
		return projectsFile{}, 0, false, err
	}
	kept, refused := r.trustworthy(table.Projects)
	table.Projects = kept
	boundary, trusted := r.trustedGlobalRoot(table.GlobalRoot)
	boundaryRefused := table.GlobalRoot != nil && !trusted
	table.GlobalRoot = boundary
	return table, dropped + refused, boundaryRefused, nil
}

// trustworthy keeps the entries this build derives both keyed values of, with every
// ambiguous set removed, and reports how many it refused.
//
// The id is the attribution: an entry whose id is not HMAC-SHA256(salt, root)
// truncated to idHexLen is an entry someone wrote by hand, and honouring it would
// let a hand-edited file decide which repository an event belongs to (ADR-0019 §3,
// §8). The id covers the root alone, so the match digest covers the rest of what
// resolution matches with — every alias and the case-folding flag — for the same
// reason and against the same salt: an alias hand-added beside a legitimate root is
// not a collision, so nothing else here would refuse it, and it would both
// re-attribute a directory and widen what discovery may read. Two entries claiming
// the same id, the same root, or the same recorded spelling are ambiguous, and every
// member of such a set is refused rather than one of them arbitrarily preferred —
// picking would be choosing an attribution.
//
// Refusing never re-derives, reassigns or re-registers anything. `init` is the only
// operation that discovers and records a root, and a registered root is never
// reassigned (ADR-0019 §9); a refused entry simply stops resolving, so the
// directory hashes as itself with Matched false, which is the fail-closed answer.
func (r *Repos) trustworthy(entries []projectEntry) (kept []projectEntry, refused int) {
	// A constant-time compare over an id derived from a secret salt. It costs
	// nothing here and removes the question of whether a timing signal leaks the
	// salt to something that can offer roots and watch how long the refusal takes.
	derived := make([]projectEntry, 0, len(entries))
	for _, entry := range entries {
		idOK := hmac.Equal([]byte(entry.ID), []byte(r.hashRoot(entry.Root)))
		matchOK := hmac.Equal([]byte(entry.MatchMAC), []byte(r.matchMAC(entry)))
		// An unbounded entry may also carry the digest a build from before the
		// collection boundary existed wrote (legacyMatchMAC): the alternative is that
		// adding a field to the digest silently stops resolving every repository
		// already recorded. A bounded entry has exactly one acceptable digest.
		if !matchOK && entry.CollectFrom == "" {
			matchOK = hmac.Equal([]byte(entry.MatchMAC), []byte(r.legacyMatchMAC(entry)))
		}
		if !idOK || !matchOK {
			refused++
			continue
		}
		derived = append(derived, entry)
	}

	// Ambiguity is counted across the entries that survived derivation, not the
	// original set: an entry already refused cannot make another one ambiguous.
	ids := map[string]int{}
	roots := map[string]int{}
	spellings := map[string]int{}
	for _, entry := range derived {
		ids[entry.ID]++
		roots[entry.Root]++
		for _, spelling := range entry.spellings() {
			spellings[spelling]++
		}
	}

	kept = make([]projectEntry, 0, len(derived))
	for _, entry := range derived {
		ambiguous := ids[entry.ID] > 1 || roots[entry.Root] > 1
		for _, spelling := range entry.spellings() {
			ambiguous = ambiguous || spellings[spelling] > 1
		}
		if ambiguous {
			refused++
			continue
		}
		kept = append(kept, entry)
	}
	return kept, refused
}

// DroppedEntries reports how many recorded entries this build refused to trust.
//
// It is a count, never the entries: doctor reports it (ADR-0019 §7) and doctor
// output is what people paste into issues. A table that quietly shrinks is the
// failure this design cannot otherwise see.
func (r *Repos) DroppedEntries() int {
	return r.dropped
}

// ConsentedRoots returns the canonical root of every repository this build
// trusts, in table order. It exists for project-local discovery across a
// machine-wide surface (report, serve): those need every consented repo to
// scan, not only the one the command happens to run in. The caller must never
// print or persist what this returns — only Identity's hashed id may leave this
// package for that (plan §3.4).
func (r *Repos) ConsentedRoots() []string {
	roots := make([]string, 0, len(r.table.Projects))
	for _, entry := range r.table.Projects {
		roots = append(roots, entry.Root)
	}
	return roots
}

// Identify returns the identity of the repository the given working directory
// belongs to.
//
// The answer is the longest recorded root — or recorded alias — that is a path
// prefix of the directory (ADR-0019 §1). Longest wins because that is what makes
// the answer unique when the table happens to hold nested roots, which Register
// refuses but a hand-edited or older file may contain.
//
// This is a pure string operation over the snapshot: no filepath.EvalSymlinks, no
// os.Stat, no git, nothing that reads the disk. Everything disk-dependent — the
// canonical spelling of a root, the aliases it also answers to, whether its
// filesystem folds case — was recorded by Register while the directory was there.
// A directory since moved or deleted therefore still resolves, which acceptance
// item 5 requires and ADR-0004's re-scan safety rests on.
//
// A directory matching no recorded root hashes as itself, with Matched false. It
// is not recorded: discovery is an explicit step, never a side effect of a read
// (ADR-0019 §9).
func (r *Repos) Identify(cwd string) (Identity, error) {
	cleaned, err := lexicalClean(cwd)
	if err != nil {
		return Identity{}, fmt.Errorf("the working directory %w", err)
	}

	_, id := r.match(cleaned)
	if id != "" {
		return Identity{ID: id, Matched: true}, nil
	}
	return Identity{ID: r.hashRoot(cleaned), Matched: false}, nil
}

// ConsentedRoot returns the recorded root the given working directory belongs to,
// or the empty string when it belongs to none.
//
// It answers the question local discovery has to ask — which directory tree may be
// read on behalf of this working directory — against the same snapshot, by the same
// longest-prefix rule, and with the same no-filesystem guarantee as Identify
// (ADR-0019 §1). Discovery needs the root rather than the directory: consent was
// given for a repository, and scanning only the subdirectory a command happens to
// run in collects part of that repository and then reports a complete pass.
//
// This is the one path this package returns, and it is one the caller already held:
// the answer is a prefix of the directory the caller passed in, so it discloses
// nothing new. That is why Identity still carries no path — a record's repository is
// a hashed id and only ever that (plan §3.4) — and why NestedRootError still names
// an enclosing repository by id alone, since that root is one the caller never
// offered. This root chooses which directories to read; it is never persisted and
// never printed.
func (r *Repos) ConsentedRoot(cwd string) (string, error) {
	cleaned, err := lexicalClean(cwd)
	if err != nil {
		return "", fmt.Errorf("the working directory %w", err)
	}
	root, _ := r.match(cleaned)
	return root, nil
}

// CollectsFrom returns the instant collection begins for the repository with this
// id, or the zero time when it records no boundary and every event the harness holds
// for it is therefore in scope (ADR-0025).
//
// It answers against the same snapshot Identify resolves against, touching no
// filesystem, so one scan cannot filter two events by two different boundaries
// (ADR-0019 §1). An id this table does not hold answers zero as well: the only caller
// asks after Identify reported a match, so the id came from this snapshot, and an
// unmatched directory is already outside collection.
//
// The boundary is a filter on what a scan imports, never a cursor: it is recorded
// once, at registration, and no scan advances it (ADR-0015 keeps re-scanning safe by
// deriving every id from the source event).
func (r *Repos) CollectsFrom(id string) time.Time {
	for _, entry := range r.table.Projects {
		if entry.ID != id {
			continue
		}
		// The value parses: an entry whose boundary this build cannot read never
		// reaches the snapshot (projectEntry.valid, fail closed).
		at, _ := parseCollectFrom(entry.CollectFrom)
		return at
	}
	return time.Time{}
}

// match returns the recorded spelling and id of the entry cwd belongs to, or two
// empty strings when no recorded spelling is a prefix of it.
//
// Longest wins because that is what makes the answer unique when the table happens
// to hold nested roots, which Register refuses but a hand-edited or older file may
// contain. cwd is assumed already lexically clean.
func (r *Repos) match(cwd string) (root, id string) {
	for _, entry := range r.table.Projects {
		for _, spelling := range entry.spellings() {
			if !hasPathPrefix(cwd, spelling, entry.CaseInsensitive) {
				continue
			}
			if id != "" && len(spelling) <= len(root) {
				continue
			}
			root, id = spelling, entry.ID
		}
	}
	return root, id
}

// Register records a consented repository root and returns its id.
//
// This is the only place in the package that reads the filesystem to normalize a
// path, and it can be: registration runs interactively inside a live directory
// (ADR-0019 §1, T071). It resolves symlinks and probes case sensitivity here, once,
// and records the answers — so derivation can hold ADR-0019 §5's normalization
// rules without ever looking at a disk that may have changed since.
//
// It is append-only in the strict sense (ADR-0019 §9, constraint 16). An existing
// entry's id, root and label are never modified:
//
//   - the same canonical root returns the id it already has, whatever label is
//     offered — T071's "an already-discovered repository keeps its existing
//     identity";
//   - a new spelling of a recorded root appends an alias, which is additive and
//     leaves the id, the root and the label where they are;
//   - a root — or a new spelling of one — nested inside a recorded root or alias,
//     or containing one, is refused (ADR-0019 §5) in both directions. That is what
//     keeps the recorded spellings mutually non-nested, and therefore
//     longest-prefix resolution unique.
//
// Append-only holds across writers, not only within one: the table is re-read
// under an exclusive lock and the decision is made against what is on disk, never
// against the snapshot this Repos opened with. ADR-0019 §9 makes a second writer
// part of the design, and an entry deleted by one is a consented repository that
// collects nothing with no error. See withProjectsLock.
//
// from is the instant collection begins for a newly recorded repository — ADR-0024's
// forward-only default, made durable by ADR-0025 so the scan a hook fires honours it
// too. The zero time records no boundary, which is what an explicit full import asks
// for: the user wants everything the harness holds, and no instant says that.
//
// The boundary is the one part of an existing entry that may change, and every edit
// narrows what an unattended scan will import or opens it fully at the user's word:
//
//   - a zero from clears a recorded boundary, because a user who has asked for the
//     whole history has asked for it from then on as well;
//   - a non-zero from onto an entry that records none records it. An unbounded entry
//     is one every scan imports everything for, so this narrows collection — and the
//     call that asks for it is the one whose disclosure says collection starts now.
//     The two ways an entry gets into that state are a full import, which clears the
//     boundary, and a table written before the boundary existed;
//   - a non-zero from never *moves* a boundary already recorded: the instant the user
//     consented is the instant the disclosure was about, and moving it forward on a
//     later `init` would silently skip everything collected in between.
//
// Nothing is lost by recording one: the history stays exactly as reachable as it was,
// through `wake ingest` or `init --full`, since every id is derived from the source
// event (ADR-0004).
//
// A refused registration writes nothing.
func (r *Repos) Register(root, label string, from time.Time) (string, error) {
	if label == "" || strings.ContainsAny(label, "/"+string(filepath.Separator)) {
		// The label is a repository name, so the reason states the requirement
		// and InvalidValueError.Value is left empty rather than echoing it.
		return "", &InvalidValueError{Key: "label", Reason: "must be a non-empty name containing no path separator"}
	}

	given, err := lexicalClean(root)
	if err != nil {
		return "", fmt.Errorf("a repository root %w", err)
	}
	// Both disk-dependent answers are taken before the lock: they are about the
	// offered directory, not about the table, and holding a lock across them would
	// serialise every registration behind two stats for no gain.
	canonical, err := canonicalRoot(given)
	if err != nil {
		return "", err
	}
	fold, err := caseInsensitive(canonical)
	if err != nil {
		return "", err
	}

	aliases := []string{}
	if given != canonical {
		aliases = append(aliases, given)
	}

	id := ""
	lockErr := withProjectsLock(r.paths.ProjectsFile, func() error {
		// The re-read is the point of the lock. Deciding from r.table — a snapshot
		// taken at OpenRepos — and then republishing the whole file would erase any
		// entry another writer recorded since.
		//
		// Through readTable rather than readProjects, so an entry this build refuses
		// to resolve against is also one it refuses to carry back into the file. A
		// hand-written entry that survived a write would be an attribution the table
		// keeps asserting after it has been rejected.
		table, dropped, boundaryRefused, readErr := r.readTable()
		if readErr != nil {
			return readErr
		}
		updated, entryID, changed, decideErr := r.registration(table, canonical, aliases, label, fold, from)
		if decideErr != nil {
			return decideErr
		}
		if changed {
			if writeErr := writeProjects(r.paths.ProjectsFile, updated); writeErr != nil {
				return writeErr
			}
		}
		// The dropped count never goes down within a session: this write leaves out
		// the entries the re-read refused, and making that shrinkage visible is the
		// count's whole job (ADR-0019 §7). The boundary refusal is ORed for the same
		// reason — the write above dropped the boundary the re-read refused, and a
		// later registration finding nothing there must not report the shrinkage away.
		r.table, r.dropped, id = updated, max(r.dropped, dropped), entryID
		r.boundaryRefused = r.boundaryRefused || boundaryRefused
		return nil
	})
	if lockErr != nil {
		return "", lockErr
	}
	return id, nil
}

// registration decides what the table becomes when a consented root is offered.
//
// It is pure over the table it is handed — every disk-dependent answer, the
// canonical spelling and the case-folding flag, was obtained before the lock was
// taken — so the caller can read the table, decide, and write inside one locked
// section. changed reports whether there is anything to write.
func (r *Repos) registration(table projectsFile, canonical string, aliases []string, label string, fold bool, from time.Time) (updated projectsFile, id string, changed bool, err error) {
	// An exact match is a re-registration, not nesting: the id stands, and the
	// only thing that may change is the set of spellings it answers to, plus a
	// recorded boundary an explicit full import clears.
	if i := slices.IndexFunc(table.Projects, func(e projectEntry) bool { return e.Root == canonical }); i >= 0 {
		existing := table.Projects[i]
		added := []string{}
		for _, alias := range aliases {
			if alias != existing.Root && !slices.Contains(existing.Aliases, alias) {
				added = append(added, alias)
			}
		}
		// The boundary is the one recorded fact this entry allows to change, and only
		// in the two directions Register describes.
		//
		// Clearing is the explicit full import: the user asked for the whole history in
		// a call that found the repository already consented.
		//
		// Recording is a plain init on an entry that has none — after a full import,
		// which clears it, or from a table written before the boundary existed. It is
		// not the forward move Register refuses: there is nothing to move, and an
		// unbounded entry is one every scan would import everything for. Leaving it
		// unbounded here is what made a plain `init` print the forward-only disclosure
		// and then hand the next hook-fired scan the whole history anyway — the
		// disclosure is unconditional, so this is what makes it true.
		clearBoundary := from.IsZero() && existing.CollectFrom != ""
		recordBoundary := !from.IsZero() && existing.CollectFrom == ""
		if len(added) == 0 && !clearBoundary && !recordBoundary {
			return table, existing.ID, false, nil
		}
		if len(added) > 0 {
			// An alias is a recorded spelling, so ADR-0019 §5's refusal applies to it as
			// well: a spelling that sits inside another consented root would attribute
			// that subtree to this repository instead of the one the user consented to
			// for it, and the recorded spellings would stop being mutually non-nested.
			// Only the new spellings are checked — the canonical root is already
			// recorded and any existing alias was checked when it was appended — and this
			// entry is exempt, because a spelling nested inside its own root resolves to
			// the same id either way and there is nothing ambiguous to refuse.
			if nested := nestedWith(table.Projects, added, fold, i); nested != nil {
				return table, "", false, nested
			}
		}
		switch {
		case clearBoundary:
			existing.CollectFrom = ""
		case recordBoundary:
			existing.CollectFrom = formatCollectFrom(from)
		}
		// Cloned before appending: the entry is a copy of the recorded one, but its
		// alias slice still shares the recorded backing array.
		existing.Aliases = append(slices.Clone(existing.Aliases), added...)
		// Re-signed, because the digest covers the spellings and the boundary and this
		// changed one of them. The id is not: this is the same repository, under a new
		// spelling or with the history it declined now asked for.
		existing = r.signed(existing)
		if !existing.valid() {
			return table, "", false, errUnreadableEntry
		}
		table.Projects = slices.Clone(table.Projects)
		table.Projects[i] = existing
		return table, existing.ID, true, nil
	}

	if nested := nestedWith(table.Projects, append([]string{canonical}, aliases...), fold, -1); nested != nil {
		return table, "", false, nested
	}

	entry := r.signed(projectEntry{
		ID:              r.hashRoot(canonical),
		Label:           label,
		Root:            canonical,
		Aliases:         aliases,
		CaseInsensitive: fold,
		CollectFrom:     formatCollectFrom(from),
	})
	if !entry.valid() {
		return table, "", false, errUnreadableEntry
	}

	table.Projects = append(slices.Clone(table.Projects), entry)
	return table, entry.ID, true, nil
}

// nestedWith reports the recorded entry that the offered spellings nest with, in
// either direction, or nil when there is none. The entry at index exempt — -1 for
// none — is skipped.
//
// Case folding here is the union of the recorded flag and the flag just probed for
// the new root: the two may sit on different filesystems, and the conservative
// direction is to refuse. A missed nesting is a table with two overlapping roots,
// where longest-prefix resolution stops being unique; a refusal is visible and the
// user can pick which root they meant. The cost of the union is that nestedWith
// and Identify fold by different rules — per entry there, across both here — so a
// registration can be refused as nested that Identify would not in fact have
// resolved ambiguously.
func nestedWith(entries []projectEntry, offered []string, fold bool, exempt int) *NestedRootError {
	for i, entry := range entries {
		if i == exempt {
			continue
		}
		folded := fold || entry.CaseInsensitive
		for _, recorded := range entry.spellings() {
			for _, candidate := range offered {
				if hasPathPrefix(candidate, recorded, folded) {
					return &NestedRootError{EnclosingID: entry.ID, Outer: false}
				}
				if hasPathPrefix(recorded, candidate, folded) {
					return &NestedRootError{EnclosingID: entry.ID, Outer: true}
				}
			}
		}
	}
	return nil
}

// nameKeyDomain separates the derived-name subkey from every other use of the
// salt. Two keyed values over one key with no domain label are one substitution
// away from being confused for each other; the label costs nothing and the version
// suffix leaves room to re-key names without re-identifying repositories.
const nameKeyDomain = "wake/derived-name/v1"

// NameKey returns the key a persisted scope digest is derived under
// (record.NewNamer, ADR-0020).
//
// It is a subkey of the per-machine salt, by the same construction and for the
// same reason as hashRoot: a scoped primitive reference carries a repository path
// fragment, and an unsalted hash of a path is not one-way, so the digest that
// reaches the spool has to be keyed.
//
// A subkey rather than the salt itself, because the salt is what makes the
// repository id one-way (ADR-0019 §3): a key that leaves this package cannot then
// be used to compute or confirm an id. Deriving it needs no disk — the salt is
// already loaded — so this holds ADR-0019 §1's rule that derivation never touches
// the filesystem.
func (r *Repos) NameKey() []byte {
	return keyeddigest.Sum(r.salt, []byte(nameKeyDomain))
}

// matchMACDomain separates the match digest from every other use of the salt, for
// the reason nameKeyDomain gives: two keyed values over one key with no domain label
// are one substitution away from being confused for each other. The version suffix
// leaves room to change what the digest covers without touching the id.
const matchMACDomain = "wake/match-mac/v1"

// matchMAC is the keyed digest over everything resolution matches an observed
// working directory against — the entry's canonical root, its aliases in recorded
// order, and its case-folding flag — followed by the instant collection begins for
// it.
//
// The boundary is in here because nothing else protects it. The id covers the root
// alone, so a `collect_from` deleted out of a 0600 file would leave an entry that
// still verifies and now imports every event the repository declined: no error, no
// counter, and a disclosure that had already promised otherwise (ADR-0025). Covered,
// the same edit refuses the entry.
//
// Not truncated, unlike the id: this value is never printed and never persisted
// anywhere but the local table, so there is no brevity to trade the margin for.
//
// The parts are NUL-separated, which is injective here because neither a path nor an
// RFC3339 timestamp can contain a NUL byte — so no two different entries encode to
// the same input, and moving an alias's spelling into the root cannot produce the
// same digest as leaving it where it was. The boundary sits last and is always
// terminated, so an entry with no boundary is not the same input as one whose
// boundary was dropped from the encoding.
func (r *Repos) matchMAC(entry projectEntry) string {
	buf := append(matchInput(entry), entry.CollectFrom...)
	return hex.EncodeToString(keyeddigest.Sum(r.salt, append(buf, 0)))
}

// legacyMatchMAC is matchMAC's construction from before the collection boundary
// joined what the digest covers (ADR-0025).
//
// It is accepted on read, and only for an entry that records no boundary — which is
// every entry a build writing this construction could have written. Both halves of
// that sentence are load-bearing. Without the fallback, adding a field to the digest
// would stop resolving every repository already in projects.json, and each would
// hash as itself with Matched false until the user happened to re-run `wake init`.
// Without the restriction, stripping a recorded boundary would produce an entry this
// build still accepts, which is the widening the digest exists to catch: a boundary
// this build wrote can never be edited down to a digest this build honours.
func (r *Repos) legacyMatchMAC(entry projectEntry) string {
	return hex.EncodeToString(keyeddigest.Sum(r.salt, matchInput(entry)))
}

// matchInput is the NUL-separated digest input up to but not including the
// collection boundary. It is split out so the two constructions cannot drift: the
// legacy one is this input exactly, which is what makes accepting it a statement
// about one appended field rather than about a second encoder.
func matchInput(entry projectEntry) []byte {
	buf := append([]byte(matchMACDomain), 0)
	if entry.CaseInsensitive {
		buf = append(buf, 'f')
	}
	buf = append(buf, 0)
	for _, spelling := range entry.spellings() {
		buf = append(buf, spelling...)
		buf = append(buf, 0)
	}
	return buf
}

// signed returns entry with the match digest this build derives for it. Every write
// site goes through it, so a spelling recorded without a digest cannot happen — an
// entry missing one is refused on the next read, which would be a registration that
// looked successful and then collected nothing (errUnreadableEntry's reason).
//
// It deliberately leaves ID alone: an existing entry's id is never recomputed
// (ADR-0019 §9), and the id of a new one is derived where the entry is built, from
// the canonical root the caller resolved.
func (r *Repos) signed(entry projectEntry) projectEntry {
	entry.MatchMAC = r.matchMAC(entry)
	return entry
}

// hashRoot is the repository id: HMAC-SHA256 of the root under the per-machine
// salt, hex-encoded and truncated to idHexLen characters (ADR-0019 §3, §8).
//
// Keyed rather than plain because the input space is tiny — a home directory plus
// a repository name that is often public — so an unsalted hash of a path is not
// one-way, and under the remote build tag it is the id that leaves the machine.
func (r *Repos) hashRoot(root string) string {
	return hex.EncodeToString(keyeddigest.Sum(r.salt, []byte(root)))[:idHexLen]
}

// NestedRootError is returned when a root would nest with one already recorded.
//
// It names the enclosing repository by hashed id only — never its path, any
// element of it, or its label (plan §3.4, constraint 21). T071 resolves a readable
// name from the id when it builds the accessor for that; this package does not
// return one.
type NestedRootError struct {
	// EnclosingID is the id of the already-recorded repository.
	EnclosingID string
	// Outer reports the direction: true when the offered root contains the
	// recorded one, false when it is inside it.
	Outer bool
}

func (e *NestedRootError) Error() string {
	if e.Outer {
		return fmt.Sprintf("this root contains the already-registered repository %s; register the inner root or the outer one, not both", e.EnclosingID)
	}
	return fmt.Sprintf("this root is inside the already-registered repository %s; register the inner root or the outer one, not both", e.EnclosingID)
}

// spellings returns every string this entry answers to: its canonical root first,
// then its aliases. The root is first so a longest-match scan reaches it even when
// an alias is the same length.
func (e projectEntry) spellings() []string {
	return append([]string{e.Root}, e.Aliases...)
}

// lexicalClean normalizes a path without touching the filesystem.
//
// Absolute and clean is all a recorded root or an observed working directory may
// need to be compared: filepath.Abs would resolve against the process working
// directory, which is not where the observed event happened, and EvalSymlinks
// would read a disk that has moved on (ADR-0019 §1). `~` is not expanded — it is
// not an absolute path, so it is rejected rather than guessed at.
//
// The error names the requirement and never the path.
func lexicalClean(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) {
		return "", errPathNotAbsolute
	}
	return filepath.Clean(p), nil
}

// hasPathPrefix reports whether cwd is root or a directory inside it.
//
// It compares path elements, not characters: /a-b and /ab are not inside /a, and
// treating them as inside would attribute one repository's work to another. root
// is assumed clean, so it ends in a separator only when it is the filesystem root.
//
// fold uses strings.ToLower, which is simple case folding rather than the full
// Unicode folding a case-insensitive filesystem performs. That is adequate for the
// ASCII-dominated path elements this compares, and the alternative is an
// x/text dependency for a corner case in a tool whose auditable dependency list is
// itself a feature (AGENTS.md § Stack & commands).
func hasPathPrefix(cwd, root string, fold bool) bool {
	if fold {
		cwd, root = strings.ToLower(cwd), strings.ToLower(root)
	}
	if cwd == root {
		return true
	}
	separator := string(filepath.Separator)
	if !strings.HasSuffix(root, separator) {
		root += separator
	}
	return strings.HasPrefix(cwd, root)
}

// canonicalRoot resolves a root to the spelling every other spelling of it
// resolves to. Registration only (ADR-0019 §1 vs §5).
//
// The error names no path: a root is repository content, and registration's error
// reaches the same terminal as everything else.
func canonicalRoot(given string) (string, error) {
	resolved, err := filepath.EvalSymlinks(given)
	if err != nil {
		return "", errRootNotADirectory
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errRootNotADirectory
	}
	return filepath.Clean(resolved), nil
}

// caseInsensitive reports whether the filesystem holding dir folds case.
//
// This runs at registration only, and that division is the whole design
// (ADR-0019 §1 vs §5). §5 folds case only on a case-insensitive filesystem —
// on ext4, ~/Dev and ~/dev are genuinely different directories and folding would
// merge two repositories — while §1 forbids derivation from touching the
// filesystem at all. The answer is therefore probed here, where the directory is
// present, and recorded in the entry; derivation reads the recorded flag and never
// asks the disk.
//
// The probe re-spells one letter of the path's final element and asks whether it
// names the same file. It writes nothing, and a path with no ASCII letter to flip
// is reported as case-sensitive: the conservative answer, since it keeps two
// spellings apart rather than merging them.
func caseInsensitive(dir string) (bool, error) {
	original, err := os.Lstat(dir)
	if err != nil {
		return false, errRootNotADirectory
	}
	respelled, ok := flipCaseOfLastElement(dir)
	if !ok {
		return false, nil
	}
	// Lstat, not Stat: a symlink whose own name differs from dir's only in case
	// resolves to dir on every filesystem, and following it would report a folding
	// this filesystem does not do — merging two roots §5 keeps apart. dir is
	// canonical, so its final element is never a symlink and the two agree on it.
	//
	// A re-spelling that does not resolve is the answer, not a failure: the
	// filesystem distinguishes the two spellings. It is folded into the result
	// rather than returned, because a probe that found out what it came to find out
	// has not failed.
	other, statErr := os.Lstat(respelled)
	return statErr == nil && os.SameFile(original, other), nil
}

// flipCaseOfLastElement returns the path with the case of one ASCII letter in its
// final element flipped, and whether it found one.
//
// The final element, not the last letter anywhere in the path: the probe is asking
// about the filesystem holding this directory, and a path like /mnt/vol/2024 whose
// last letter sits two elements up would answer for whatever is mounted there
// instead — wrongly in either direction, and a wrong "case-sensitive" costs silent
// under-collection. When the final element has no ASCII letter, the nearest
// ancestor that has one is the closest this probe can get; a path with none at all
// reports no re-spelling, which the caller reads as case-sensitive.
func flipCaseOfLastElement(path string) (string, bool) {
	b := []byte(path)
	for end := len(b); end > 0; {
		start := end
		for start > 0 && b[start-1] != filepath.Separator {
			start--
		}
		for i := end - 1; i >= start; i-- {
			if flipped := flipASCIILetter(b[i]); flipped != b[i] {
				b[i] = flipped
				return string(b), true
			}
		}
		end = start
		for end > 0 && b[end-1] == filepath.Separator {
			end--
		}
	}
	return path, false
}

// flipASCIILetter returns c in the other case, or c unchanged when it is not an
// ASCII letter. Simple ASCII folding, for the reason hasPathPrefix gives.
func flipASCIILetter(c byte) byte {
	switch {
	case c >= 'a' && c <= 'z':
		return c - ('a' - 'A')
	case c >= 'A' && c <= 'Z':
		return c + ('a' - 'A')
	}
	return c
}
