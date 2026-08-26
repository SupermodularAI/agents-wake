package claudecode

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

// frozenPackageImports is every non-test file in this package and the exact import set
// that file may have, declared here and nowhere else. Asserting the package against a
// list it exports itself would be vacuous, so the literal lives in the test.
//
// The mechanism is ported from internal/remote/otlp_test.go, which freezes the wire
// encoder's imports for the same reason: a package whose guarantee is "this code cannot
// do X" is only as good as an assertion that it still cannot. Here the guarantee is
// ADR-0019 §1's — "derivation then never touches the filesystem" — restated by ADR-0036
// §3 for the installed-primitive set, which is why that set is injected as data rather
// than read here.
//
// Per file rather than per package, which is the stricter form: one union would let any
// file hold any allowlisted import, while an exact set per file means a new capability
// has to be declared against the file that acquires it, and a file absent from this map
// fails outright.
var frozenPackageImports = map[string][]string{
	"reader.go": {
		"bytes",
		"cmp",
		"encoding/json",
		"errors",
		"github.com/SupermodularAI/agents-wake/internal/record",
		"io",
		"slices",
		"time",
	},
	"scan.go": {
		"encoding/json",
		"errors",
		"github.com/SupermodularAI/agents-wake/internal/jsonl",
		"github.com/SupermodularAI/agents-wake/internal/record",
		"io",
	},
	"session.go": {
		"github.com/SupermodularAI/agents-wake/internal/record",
		"time",
	},
	"session_end.go": {
		"bytes",
		"cmp",
		"encoding/json",
		"github.com/SupermodularAI/agents-wake/internal/record",
		"math",
		"slices",
		"time",
	},
	"subagent.go": {
		"cmp",
		"github.com/SupermodularAI/agents-wake/internal/record",
		"slices",
		"time",
	},
	"typed.go": {
		"github.com/SupermodularAI/agents-wake/internal/record",
		"strings",
	},
}

// forbiddenReaderImports names the capabilities derivation must not have. ADR-0019 §1:
// "derivation then never touches the filesystem", restated by ADR-0036 §3 for the
// installed-primitive set, which is why that set is injected as data instead. The
// package-wide check is the strong form: no file of this reader may acquire them.
//
// internal/inventory and internal/config are on the list beside the filesystem packages
// because they are how the capability would arrive indirectly. Discovery reads the
// harness directory and consent lives in the project table; a reader that imported
// either would be reading the filesystem through a package that does it on its behalf,
// which is the same violation one import removed.
var forbiddenReaderImports = []string{
	"os",
	"path/filepath",
	"io/fs",
	"net/http",
	"os/exec",
	"github.com/SupermodularAI/agents-wake/internal/inventory",
	"github.com/SupermodularAI/agents-wake/internal/config",
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
			// The tripwire. A new file in this package is a new set of capabilities on
			// the derivation path, and it has to be declared here before it can be
			// compiled past this test.
			t.Errorf("%s has no entry in frozenPackageImports: declare its exact import set", name)
			continue
		}
		if !slices.Equal(imports, frozen) {
			t.Errorf("imports of %s = %v, frozen allowlist = %v", name, imports, frozen)
		}
		// Every scanned file, not a subset: the guarantee is about the package.
		for _, forbidden := range forbiddenReaderImports {
			if slices.Contains(imports, forbidden) {
				t.Errorf("%s imports %q: derivation must not touch the filesystem (ADR-0019 §1, ADR-0036 §3)", name, forbidden)
			}
		}
	}
	if len(scanned) == 0 {
		// Without this the whole assertion passes vacuously against an empty set if the
		// scan ever stops finding files.
		t.Fatal("found no non-test .go files to scan: the import assertion would be vacuous")
	}

	// The twin guard: a stale entry would leave a deleted file's allowlist lingering,
	// which makes the map's coverage look wider than it is.
	for name := range frozenPackageImports {
		if !slices.Contains(scanned, name) {
			t.Errorf("frozenPackageImports declares %s, which this package does not contain", name)
		}
	}
}
