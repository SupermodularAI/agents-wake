package sqlitex

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// frozenPackageImports is every non-test file in this package and the exact import
// set that file may have. The mechanism is ported from
// internal/adapter/claudecode/imports_test.go, which freezes the reader's imports
// for the same reason: a package whose guarantee is "this code cannot do X" is
// only as good as an assertion that it still cannot.
//
// Per file rather than per package, which is the stricter form: a new capability
// has to be declared against the file that acquires it, and a file absent from
// this map fails outright.
var frozenPackageImports = map[string][]string{
	// The whole file is the driver import. Nothing else may live here, because
	// TestOnlyDriverGoImportsTheSQLiteDriver names this file by hand.
	"driver.go": {"modernc.org/sqlite"},
	// Thresholds only: guards.go holds numbers and no capability at all.
	"guards.go": {},
	// The one write path, and the only file here allowed to open a store without
	// mode=ro. It is tests-only and TestNoCommandPathCreatesAStore proves it.
	"fixture.go": {"database/sql", "errors", "fmt"},
	"sqlitex.go": {
		"context",
		"database/sql",
		"errors",
		"io",
		"io/fs",
		"net/url",
		"os",
		"path/filepath",
		"time",
	},
}

func TestPackageImportsAreFrozen(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}

	scanned := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned = append(scanned, name)

		imports := importsOf(t, name)
		frozen, declared := frozenPackageImports[name]
		if !declared {
			// The tripwire. A new file in this package is a new set of capabilities
			// on the only path to SQLite, and it has to be declared here before it
			// can be compiled past this test.
			t.Errorf("%s has no entry in frozenPackageImports: declare its exact import set", name)
			continue
		}
		if !slices.Equal(imports, frozen) {
			t.Errorf("imports of %s = %v, frozen allowlist = %v", name, imports, frozen)
		}
	}
	if len(scanned) == 0 {
		t.Fatal("found no non-test .go files to scan: the import assertion would be vacuous")
	}
	// The twin guard: a stale entry would leave a deleted file's allowlist
	// lingering, which makes the map's coverage look wider than it is.
	for name := range frozenPackageImports {
		if !slices.Contains(scanned, name) {
			t.Errorf("frozenPackageImports declares %s, which this package does not contain", name)
		}
	}
}

// TestOnlyDriverGoImportsTheSQLiteDriver walks the whole module — test files
// included — and asserts ADR-0009's confinement as a fact rather than a
// convention: the driver is imported by exactly one file, and database/sql is
// imported only inside this package.
//
// Test files are in scope here, and that is the one place this tightens the
// claudecode precedent. fixture.go is what makes tightening it possible: a test
// anywhere in the module that needed to build a store would otherwise have to
// open its own handle, which would be a second way to touch SQLite and would put
// the hole exactly where a hostile fixture gets written.
func TestOnlyDriverGoImportsTheSQLiteDriver(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving module root: %v", err)
	}
	confined := filepath.Join(root, "internal", "sqlitex")

	scanned := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "dist" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		scanned++
		for _, imported := range importsOf(t, path) {
			switch imported {
			case "modernc.org/sqlite":
				if path != filepath.Join(confined, "driver.go") {
					t.Errorf("%s imports the SQLite driver: only internal/sqlitex/driver.go may (ADR-0009)", path)
				}
			case "database/sql":
				if filepath.Dir(path) != confined {
					t.Errorf("%s imports database/sql: internal/sqlitex is the only way this codebase touches SQLite (ADR-0009)", path)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if scanned == 0 {
		t.Fatal("found no .go files to scan: the confinement assertion would be vacuous")
	}
}

// TestNoCommandPathCreatesAStore keeps the one write path honest. CreateFixture
// opens a store read-write, and nothing a command can reach may call it.
func TestNoCommandPathCreatesAStore(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving module root: %v", err)
	}
	confined := filepath.Join(root, "internal", "sqlitex")

	scanned := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "dist" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") || filepath.Dir(path) == confined {
			return nil
		}
		scanned++
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(source), "sqlitex.CreateFixture") {
			t.Errorf("%s references sqlitex.CreateFixture: no command path may create a store", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if scanned == 0 {
		t.Fatal("found no non-test .go files to scan: the assertion would be vacuous")
	}
}

// importsOf parses one file for its import paths only, sorted.
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	imports := make([]string, 0, len(parsed.Imports))
	for _, spec := range parsed.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquoting import path %s in %s: %v", spec.Path.Value, path, err)
		}
		imports = append(imports, imported)
	}
	slices.Sort(imports)
	return imports
}
