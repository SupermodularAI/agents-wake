package config

import (
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// packageDir is this package's path relative to the module root. The boundary
// tests below are about what is true outside it.
const packageDir = "internal/config"

// moduleRoot walks up to the directory holding go.mod.
//
// A test that cannot find the module root fails rather than skipping: a skipped
// boundary test is indistinguishable from a passing one in CI output, and this is
// the check that keeps acceptance item 12 true as later tickets add packages.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolving the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory; the boundary test must never be skipped")
		}
		dir = parent
	}
}

// Acceptance item 12, mechanically. The privacy guarantee — the repository label
// and path never leave the local store, only the hashed id — is checkable by
// reading one directory precisely because no other package can name either file.
// A hand review of call sites would hold until the next ticket; this holds after
// it.
func TestProjectsJsonAndSaltAreNamedOnlyInThisPackage(t *testing.T) {
	root := moduleRoot(t)
	confined := []string{projectsFileName, saltFileName}
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing under a dot-directory is built into the binary, and
			// .agent/ holds pipeline artifacts that legitimately discuss these
			// files in prose.
			if name := d.Name(); path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if strings.HasPrefix(relative, packageDir+string(filepath.Separator)) {
			return nil
		}
		scanned++

		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, name := range confined {
			if strings.Contains(string(raw), name) {
				t.Errorf("%s names %q; both files are read and written only inside %s", relative, name, packageDir)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if scanned == 0 {
		t.Fatal("the walk scanned no file outside the package; the check proved nothing")
	}
}

// Acceptance item 13. Identity is the type every consumer of the identity
// function sees, so its field list is the guarantee: a Root or Label added here
// would be a repository path or name handed to a renderer, and the struct is
// where that has to be impossible rather than merely discouraged.
func TestIdentityCarriesNoPathOrLabel(t *testing.T) {
	assertFieldsAre(t, reflect.TypeOf(Identity{}), "ID", "Matched")
}

// The same check over the rest of the exported surface.
//
// Paths is deliberately excluded: its fields are paths, but they are this tool's
// own files — the config root, the data root, and the files inside them — not any
// observed repository's. Nothing in it comes out of projects.json.
func TestExportedTypesCarryNoPathOrLabelField(t *testing.T) {
	assertFieldsAre(t, reflect.TypeOf(Setting{}), "Key", "Value", "Default", "Overridden", "Provisional")
	assertFieldsAre(t, reflect.TypeOf(Problem{}), "Key", "Reason")
	assertFieldsAre(t, reflect.TypeOf(NestedRootError{}), "EnclosingID", "Outer")
	assertFieldsAre(t, reflect.TypeOf(Key{}), "Name", "Kind", "Default", "Sentinels", "Provisional")

	// A second, looser net for a field added under a name the allowlists above
	// would have to be updated to admit: whoever adds it has to justify the name
	// as well as the field.
	for _, typ := range []reflect.Type{
		reflect.TypeOf(Identity{}),
		reflect.TypeOf(Setting{}),
		reflect.TypeOf(Problem{}),
		reflect.TypeOf(NestedRootError{}),
		reflect.TypeOf(Key{}),
	} {
		for i := range typ.NumField() {
			name := strings.ToLower(typ.Field(i).Name)
			for _, forbidden := range []string{"path", "root", "label", "dir", "cwd"} {
				if strings.Contains(name, forbidden) {
					t.Errorf("%s.%s is named after a repository location; only Paths may carry one, and only for this tool's own files", typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}

func assertFieldsAre(t *testing.T, typ reflect.Type, want ...string) {
	t.Helper()
	got := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s has fields %v, want exactly %v", typ.Name(), got, want)
	}
}

// Acceptance item 10. The salt is the one secret in the system, and config.toml is
// the file users are asked to paste into a bug report (ADR-0019 §4), so it may not
// arrive there by any route: not as a key, not as a value, and not through the
// list output that renders every key.
func TestSaltIsNeverAConfigKeyNorInListOutput(t *testing.T) {
	p := testPaths(t)

	for _, name := range KeyNames() {
		if strings.Contains(strings.ToLower(name), "salt") {
			t.Errorf("%q is a registered key; the salt is never configuration", name)
		}
	}
	for _, name := range []string{saltFileName, "repo.salt", "repo_salt", "salt"} {
		if path, err := Set(p, name, "x"); err == nil {
			t.Errorf("Set(%q) wrote %s, want a rejection", name, path)
		}
	}

	// A real salt, then a real write, so the assertions below are about the file
	// this build actually produces.
	r := openRepos(t, p)
	salt, err := os.ReadFile(p.SaltFile)
	if err != nil {
		t.Fatalf("reading the salt: %v", err)
	}
	root := mkdirAll(t, filepath.Join(tempRealDir(t), "repo"))
	mustRegister(t, r, root, "repo")
	if _, err := Set(p, "ui.default_window", "7d"); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	forms := []string{string(salt), hex.EncodeToString(salt), strings.ToUpper(hex.EncodeToString(salt))}
	c := loadOrFail(t, p)
	for _, setting := range c.List() {
		for _, field := range []string{setting.Key, setting.Value, setting.Default} {
			for _, form := range forms {
				if strings.Contains(field, form) {
					t.Errorf("the salt reached config list output in %q", setting.Key)
				}
			}
		}
	}
	written := readFileOrFail(t, p.ConfigFile)
	for _, form := range forms {
		if strings.Contains(written, form) {
			t.Error("the salt was written into config.toml")
		}
	}
}

// Acceptance items 10 and 13, on the path where this kind of promise usually
// leaks (plan §4.2): every error this package can return is driven, and none of
// them may carry the salt, a repository path, an element of one, or a label.
func TestNoErrorPathLeaksTheSaltOrARepoPath(t *testing.T) {
	// Tokens no message of ours contains for another reason, so a match is a leak
	// and never a coincidence.
	const (
		outerName = "boundary-outer-repo"
		innerName = "boundary-inner-repo"
		label     = "boundary-label"
		// Recognisable, and deliberately not saltLen bytes long — a coincidence
		// that would make the case a no-op.
		saltContents = "boundary-unmistakable-salt"
	)

	for _, c := range []struct {
		name string
		// run returns any secret only this case knows, and the error to inspect.
		run func(t *testing.T, p Paths, outer, inner string) ([]string, error)
	}{
		{
			name: "a wrong-length salt file",
			run: func(t *testing.T, p Paths, _, _ string) ([]string, error) {
				if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
					t.Fatalf("creating the config root: %v", err)
				}
				if err := os.WriteFile(p.SaltFile, []byte(saltContents), 0o600); err != nil {
					t.Fatalf("writing a short salt: %v", err)
				}
				_, err := OpenRepos(p)
				return []string{saltContents}, err
			},
		},
		{
			name: "an unreadable salt file",
			run: func(t *testing.T, p Paths, _, _ string) ([]string, error) {
				// A directory where the file belongs, rather than a chmod: the
				// read fails the same way whether or not the test runs as root.
				if err := os.MkdirAll(p.SaltFile, 0o700); err != nil {
					t.Fatalf("replacing the salt with a directory: %v", err)
				}
				_, err := OpenRepos(p)
				return nil, err
			},
		},
		{
			name: "a projects.json that does not parse",
			run: func(t *testing.T, p Paths, outer, _ string) ([]string, error) {
				writeProjectsJSON(t, p, `{"version": 1, "projects": [{"root": "`+outer+`" oops}]}`+"\n")
				_, err := OpenRepos(p)
				return nil, err
			},
		},
		{
			name: "a relative working directory",
			run: func(t *testing.T, p Paths, _, _ string) ([]string, error) {
				_, err := openRepos(t, p).Identify(filepath.Join(outerName, innerName))
				return nil, err
			},
		},
		{
			name: "a relative data root",
			run: func(t *testing.T, _ Paths, _, _ string) ([]string, error) {
				t.Setenv(EnvDataDir, filepath.Join(outerName, innerName))
				_, err := ResolvePaths()
				return nil, err
			},
		},
		{
			name: "an unknown config key",
			// The key name itself is deliberately not built from the secrets
			// below: an unknown-key error quotes the name the caller typed, which
			// acceptance item 4 requires it to do. A config key is not a
			// repository path.
			run: func(t *testing.T, p Paths, _, _ string) ([]string, error) {
				_, err := Set(p, "nosuch.key", "30d")
				return nil, err
			},
		},
		{
			name: "an invalid config value",
			run: func(t *testing.T, p Paths, _, _ string) ([]string, error) {
				_, err := Set(p, "ui.default_window", "not-a-duration")
				return nil, err
			},
		},
		{
			name: "a label holding a path separator",
			run: func(t *testing.T, p Paths, outer, _ string) ([]string, error) {
				_, err := openRepos(t, p).Register(outer, label+string(filepath.Separator)+"x")
				return nil, err
			},
		},
		{
			name: "a root that does not exist",
			run: func(t *testing.T, p Paths, outer, _ string) ([]string, error) {
				_, err := openRepos(t, p).Register(filepath.Join(outer, "gone", innerName), label)
				return nil, err
			},
		},
		{
			name: "a nested root",
			run: func(t *testing.T, p Paths, outer, inner string) ([]string, error) {
				r := openRepos(t, p)
				mustRegister(t, r, outer, label)
				_, err := r.Register(inner, label)
				return nil, err
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := testPaths(t)
			outer := mkdirAll(t, filepath.Join(tempRealDir(t), outerName))
			inner := mkdirAll(t, filepath.Join(outer, innerName))

			extra, err := c.run(t, p, outer, inner)
			if err == nil {
				t.Fatal("the case produced no error; it proves nothing about error messages")
			}

			secrets := append([]string{outer, inner, outerName, innerName, label}, extra...)
			if salt, readErr := os.ReadFile(p.SaltFile); readErr == nil && len(salt) == saltLen {
				secrets = append(secrets, string(salt), hex.EncodeToString(salt))
			}
			message := err.Error()
			for _, secret := range secrets {
				if strings.Contains(message, secret) {
					t.Errorf("the error %q leaks %q", message, secret)
				}
			}
		})
	}
}
