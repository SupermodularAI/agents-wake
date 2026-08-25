package selfupdate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/platform"
)

// installScript is read rather than duplicated: ADR-0026 defers extracting a
// shared helper between the shell installer and this package until both exist,
// so the literals are duplicated on purpose — and these tests are what keeps the
// duplication from drifting.
const installScript = "../../install.sh"

func TestNewCurlFetcherReportsAMissingCurlPlainly(t *testing.T) {
	// An empty PATH is the only way to reach this branch on a machine that has
	// curl, which every supported platform ships.
	t.Setenv("PATH", t.TempDir())
	_, err := NewCurlFetcher()
	if !errors.Is(err, ErrCurlMissing) {
		t.Fatalf("NewCurlFetcher() with an empty PATH error = %v, want ErrCurlMissing", err)
	}
	if !strings.Contains(err.Error(), "curl") {
		t.Errorf("error = %q, want it to name curl so the user knows what to install", err)
	}
}

func TestNewCurlFetcherFindsCurlWhenInstalled(t *testing.T) {
	fetcher, err := NewCurlFetcher()
	if errors.Is(err, ErrCurlMissing) {
		t.Skip("curl is not installed on this machine")
	}
	if err != nil {
		t.Fatalf("NewCurlFetcher() error = %v", err)
	}
	if fetcher.path == "" {
		t.Error("NewCurlFetcher() returned an empty path; the resolved binary is what gets executed")
	}
}

// The two invocations are install.sh's, argument for argument. A flag that
// drifted here would still work in isolation and silently stop matching the
// installer this command is supposed to reuse (ADR-0026).
func TestCurlArgumentsMatchTheInstallScript(t *testing.T) {
	script := readInstallScript(t)
	for _, want := range []string{`-fsSL -o /dev/null -w '%{url_effective}'`, `-fsSL -o "$tmp_dir/$archive"`} {
		if !strings.Contains(script, want) {
			t.Errorf("install.sh no longer contains %q; the arguments below were copied from it", want)
		}
	}
	if got, want := strings.Join(effectiveURLArgs("https://example.test"), " "), "-fsSL -o /dev/null -w %{url_effective} https://example.test"; got != want {
		t.Errorf("effectiveURLArgs = %q, want %q", got, want)
	}
	if got := downloadArgs("https://example.test", "/tmp/x"); !slices.Equal(got, []string{"-fsSL", "-o", "/tmp/x", "https://example.test"}) {
		t.Errorf("downloadArgs = %q, want [-fsSL -o /tmp/x https://example.test]", got)
	}
}

// The archive name and the checksum file are the other pair of duplicated
// literals, and the same drift guard applies.
func TestInstallScriptAndAssetNameAgreeOnTheArchiveTemplate(t *testing.T) {
	script := readInstallScript(t)
	for _, want := range []string{`wake_${version#v}_${os}_${arch}.tar.gz`, "checksums.txt"} {
		if !strings.Contains(script, want) {
			t.Errorf("install.sh no longer contains %q; assetName replicates that template", want)
		}
	}
	name, err := assetName("v1.2.3", platform.OS()[0], platform.Arch()[0])
	if err != nil {
		t.Fatalf("assetName() error = %v", err)
	}
	if !strings.HasPrefix(name, binaryName+"_") || !strings.HasSuffix(name, ".tar.gz") {
		t.Errorf("assetName() = %q, want a %s_… .tar.gz name", name, binaryName)
	}
	if strings.Contains(name, "_v1.2.3_") {
		t.Errorf("assetName() = %q, want the leading v stripped the way install.sh's ${version#v} does", name)
	}
}

// ADR-0026's own guard is a human reading the diff, and this is as much of it as
// a test can carry: no file in this package that ships may name an in-process
// network client. It is scoped to this package deliberately — internal/ui
// legitimately imports net/http for the loopback dashboard, and internal/remote
// for a flush the user configured and turned on (ADR-0030), so a repo-wide ban
// would fail against code ADR-0026 was never about.
func TestSelfupdateNamesNoInProcessNetworkClient(t *testing.T) {
	forbidden := []string{"net/http", "net.Dial", "http.Get", "http.Client"}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, name := range forbidden {
			if strings.Contains(string(raw), name) {
				t.Errorf("%s names %q; the socket belongs to the curl subprocess (ADR-0026)", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the package: %v", err)
	}
}

func readInstallScript(t *testing.T) string {
	t.Helper()
	// A `go test` binary starts in its own package directory and no test in this
	// package calls t.Parallel(), so the relative path is stable — the same way
	// internal/platform/matrix_test.go reads .goreleaser.yaml.
	raw, err := os.ReadFile(installScript)
	if err != nil {
		t.Fatalf("reading %s: %v", installScript, err)
	}
	return string(raw)
}
