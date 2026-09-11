package opencode

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// frozenPackageImports is every non-test file in this package and the exact import
// set that file may have, declared here and nowhere else. The mechanism is ported
// from internal/adapter/claudecode/imports_test.go, and it is here for the same
// reason: a package whose guarantee is "this code cannot do X" is only as good as
// an assertion that it still cannot.
//
// Per file rather than per package, which is the stricter form: a new capability
// has to be declared against the file that acquires it, and a file absent from
// this map fails outright.
var frozenPackageImports = map[string][]string{
	"reader.go": {
		"github.com/SupermodularAI/agents-wake/internal/adapter",
		"github.com/SupermodularAI/agents-wake/internal/record",
		"time",
	},
	"rows.go": {
		"github.com/SupermodularAI/agents-wake/internal/record",
		"strings",
	},
	"outcome.go": {
		"github.com/SupermodularAI/agents-wake/internal/record",
	},
	"scan.go": {
		"cmp",
		"github.com/SupermodularAI/agents-wake/internal/adapter",
		"github.com/SupermodularAI/agents-wake/internal/record",
		"slices",
		"time",
	},
	"session.go": {
		"github.com/SupermodularAI/agents-wake/internal/adapter",
		"github.com/SupermodularAI/agents-wake/internal/record",
		"slices",
		"time",
	},
}

// forbiddenReaderImports names the capabilities derivation must not have
// (ADR-0019 §1: "derivation then never touches the filesystem").
//
// internal/inventory and internal/config are on the list beside the filesystem
// packages because they are how the capability would arrive indirectly: discovery
// reads the harness's directory and consent lives in the project table, so a
// reader importing either would be reading the filesystem through a package doing
// it on its behalf.
//
// database/sql and internal/sqlitex are this reader's own two additions, and they
// are the point: opencode keeps its history in a store, and a reader that could
// reach it would decide for itself what to read, how much of it, and in what
// order — every guard ADR-0009 puts in one place, relocated into derivation.
var forbiddenReaderImports = []string{
	"os",
	"path/filepath",
	"io/fs",
	"net/http",
	"os/exec",
	"database/sql",
	"github.com/SupermodularAI/agents-wake/internal/inventory",
	"github.com/SupermodularAI/agents-wake/internal/config",
	"github.com/SupermodularAI/agents-wake/internal/sqlitex",
}

func TestPackageImportsAreFrozen(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}

	fileSet := token.NewFileSet()
	scanned := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned = append(scanned, name)

		parsed, err := parser.ParseFile(fileSet, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		imports := make([]string, 0, len(parsed.Imports))
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquoting import path %s in %s: %v", spec.Path.Value, name, err)
			}
			imports = append(imports, path)
		}
		slices.Sort(imports)

		frozen, declared := frozenPackageImports[name]
		if !declared {
			// The tripwire. A new file in this package is a new set of capabilities
			// on the derivation path, and it has to be declared here before it can
			// be compiled past this test.
			t.Errorf("%s has no entry in frozenPackageImports: declare its exact import set", name)
			continue
		}
		if !slices.Equal(imports, frozen) {
			t.Errorf("imports of %s = %v, frozen allowlist = %v", name, imports, frozen)
		}
		for _, forbidden := range forbiddenReaderImports {
			if slices.Contains(imports, forbidden) {
				t.Errorf("%s imports %q: derivation must not touch the filesystem or the store (ADR-0019 §1, ADR-0009)", name, forbidden)
			}
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

// TestNoFreeTextFieldIsNamedAnywhere is the source-level half of ADR-0007 for this
// adapter: opencode's free-text keys have no field in the row types, and this
// asserts no file names them at all — so a future change cannot reach one by
// decoding the blob a second time.
func TestNoFreeTextFieldIsNamedAnywhere(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}
	forbidden := []string{"state.input", "state.output", "state.title", "state.error", "state.raw", "callID"}
	for _, entry := range entries {
		name := entry.Name()
		// Test files are out of scope: this file has to name the keys in order
		// to forbid them, and the guarantee is about the code that ships.
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Comments are where these keys are legitimately named: the decision not to
		// read them has to be written down somewhere. Code is what this forbids.
		stripped := withoutComments(t, name)
		for _, key := range forbidden {
			if strings.Contains(stripped, key) {
				t.Errorf("%s names %q outside a comment: it is free text and has no field here (ADR-0007)", name, key)
			}
		}
	}
}

// withoutComments returns a file's source with every comment removed, so an
// assertion about what the code does is not defeated by what the comments explain.
func withoutComments(t *testing.T, name string) string {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, name, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	source, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	stripped := source
	for _, group := range parsed.Comments {
		start := fileSet.Position(group.Pos()).Offset
		end := fileSet.Position(group.End()).Offset
		for index := start; index < end && index < len(stripped); index++ {
			stripped[index] = ' '
		}
	}
	return string(stripped)
}
