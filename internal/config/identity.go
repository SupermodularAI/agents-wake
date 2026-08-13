package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// errPathNotAbsolute rejects a path that cannot be matched against a recorded
// root. It is phrased as the tail of a sentence so a caller can name what was
// wrong — "the working directory must be an absolute path" — without quoting the
// path itself, which is repository content (plan §4.2).
var errPathNotAbsolute = errors.New("must be an absolute path")

// errRootNotADirectory is returned when a root offered for registration is not a
// directory that exists. It names no path for the same reason.
var errRootNotADirectory = errors.New("a repository root must be a directory that exists")

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
type Repos struct {
	paths   Paths
	salt    []byte
	table   projectsFile
	dropped int
}

// OpenRepos loads the salt and the resolution table.
//
// The salt is created on first need and never regenerated; the table is read as
// it is. A table that does not parse is an error rather than an empty table,
// because resolving against an empty table would hand every repository a new
// identity on the next scan.
func OpenRepos(p Paths) (*Repos, error) {
	salt, err := loadOrCreateSalt(p)
	if err != nil {
		return nil, err
	}
	table, dropped, err := readProjects(p.ProjectsFile)
	if err != nil {
		return nil, err
	}
	return &Repos{paths: p, salt: salt, table: table, dropped: dropped}, nil
}

// DroppedEntries reports how many recorded entries this build refused to trust.
//
// It is a count, never the entries: doctor reports it (ADR-0019 §7) and doctor
// output is what people paste into issues. A table that quietly shrinks is the
// failure this design cannot otherwise see.
func (r *Repos) DroppedEntries() int {
	return r.dropped
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

	best := ""
	id := ""
	for _, entry := range r.table.Projects {
		for _, spelling := range entry.spellings() {
			if !hasPathPrefix(cleaned, spelling, entry.CaseInsensitive) {
				continue
			}
			if id != "" && len(spelling) <= len(best) {
				continue
			}
			best, id = spelling, entry.ID
		}
	}
	if id != "" {
		return Identity{ID: id, Matched: true}, nil
	}
	return Identity{ID: r.hashRoot(cleaned), Matched: false}, nil
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
//   - a root nested inside a recorded one, or containing one, is refused
//     (ADR-0019 §5) — that is what keeps the recorded roots mutually non-nested,
//     and therefore longest-prefix resolution unique.
//
// A refused registration writes nothing.
func (r *Repos) Register(root, label string) (string, error) {
	if label == "" || strings.ContainsAny(label, "/"+string(filepath.Separator)) {
		// The label is a repository name, so the reason states the requirement
		// and InvalidValueError.Value is left empty rather than echoing it.
		return "", &InvalidValueError{Key: "label", Reason: "must be a non-empty name containing no path separator"}
	}

	given, err := lexicalClean(root)
	if err != nil {
		return "", fmt.Errorf("a repository root %w", err)
	}
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

	// An exact match is a re-registration, not nesting: the id stands, and the
	// only thing that may change is the set of spellings it answers to.
	if i := slices.IndexFunc(r.table.Projects, func(e projectEntry) bool { return e.Root == canonical }); i >= 0 {
		existing := r.table.Projects[i]
		added := false
		for _, alias := range aliases {
			if alias != existing.Root && !slices.Contains(existing.Aliases, alias) {
				existing.Aliases = append(existing.Aliases, alias)
				added = true
			}
		}
		if !added {
			return existing.ID, nil
		}
		updated := r.table
		updated.Projects = slices.Clone(r.table.Projects)
		updated.Projects[i] = existing
		if err := writeProjects(r.paths.ProjectsFile, updated); err != nil {
			return "", err
		}
		r.table = updated
		return existing.ID, nil
	}

	if nested := r.nestedWith(append([]string{canonical}, aliases...), fold); nested != nil {
		return "", nested
	}

	entry := projectEntry{
		ID:              r.hashRoot(canonical),
		Label:           label,
		Root:            canonical,
		Aliases:         aliases,
		CaseInsensitive: fold,
	}
	if !entry.valid() {
		// Fail closed. An entry readProjects would drop is worse than a refusal:
		// the registration would look successful and the repository would collect
		// nothing (plan §3.4).
		return "", errors.New("refusing to record a repository this build could not read back")
	}

	updated := r.table
	updated.Projects = append(slices.Clone(r.table.Projects), entry)
	if err := writeProjects(r.paths.ProjectsFile, updated); err != nil {
		return "", err
	}
	r.table = updated
	return entry.ID, nil
}

// nestedWith reports the recorded entry that the offered spellings nest with, in
// either direction, or nil when there is none.
//
// Case folding here is the union of the recorded flag and the flag just probed for
// the new root: the two may sit on different filesystems, and the conservative
// direction is to refuse. A missed nesting is a table with two overlapping roots,
// where longest-prefix resolution stops being unique; a refusal is visible and the
// user can pick which root they meant.
func (r *Repos) nestedWith(offered []string, fold bool) *NestedRootError {
	for _, entry := range r.table.Projects {
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

// hashRoot is the repository id: HMAC-SHA256 of the root under the per-machine
// salt, hex-encoded and truncated to idHexLen characters (ADR-0019 §3, §8).
//
// Keyed rather than plain because the input space is tiny — a home directory plus
// a repository name that is often public — so an unsalted hash of a path is not
// one-way, and under the remote build tag it is the id that leaves the machine.
func (r *Repos) hashRoot(root string) string {
	mac := hmac.New(sha256.New, r.salt)
	// hash.Hash's contract is that Write never returns an error. It is checked
	// anyway — errcheck runs with check-blank, and there is no honest way to
	// discard it — and a violation would produce an id over a prefix of the root,
	// which is the one outcome worth stopping the process for: every record and
	// every table entry written afterwards would be keyed to a hash nothing else
	// agrees with.
	if _, err := mac.Write([]byte(root)); err != nil {
		panic("hashing a repository root: " + err.Error())
	}
	return hex.EncodeToString(mac.Sum(nil))[:idHexLen]
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
// The probe re-spells one letter of the path and asks whether it names the same
// file. It writes nothing, and a path with no ASCII letter to flip is reported as
// case-sensitive: the conservative answer, since it keeps two spellings apart
// rather than merging them.
func caseInsensitive(dir string) (bool, error) {
	original, err := os.Stat(dir)
	if err != nil {
		return false, errRootNotADirectory
	}
	flipped, ok := flipLastASCIILetter(dir)
	if !ok {
		return false, nil
	}
	// A re-spelling that does not resolve is the answer, not a failure: the
	// filesystem distinguishes the two spellings. It is folded into the result
	// rather than returned, because a probe that found out what it came to find out
	// has not failed.
	other, statErr := os.Stat(flipped)
	return statErr == nil && os.SameFile(original, other), nil
}

// flipLastASCIILetter returns the path with the case of its last ASCII letter
// flipped, and whether there was one. The last letter is used because it is
// almost always in the final element, which is the directory whose filesystem is
// being asked about.
func flipLastASCIILetter(path string) (string, bool) {
	b := []byte(path)
	for i := len(b) - 1; i >= 0; i-- {
		switch {
		case b[i] >= 'a' && b[i] <= 'z':
			b[i] -= 'a' - 'A'
		case b[i] >= 'A' && b[i] <= 'Z':
			b[i] += 'a' - 'A'
		default:
			continue
		}
		return string(b), true
	}
	return path, false
}
