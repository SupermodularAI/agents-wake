package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// runSplit executes the root command with args and returns stdout and stderr
// separately.
//
// Separately, unlike run, because "never prints to stderr" is the property under
// test here and a single merged buffer cannot witness it.
func runSplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// isolate points HOME and the data root at fresh temporary directories, so no test
// here reads or writes the developer's real state.
func isolate(t *testing.T) config.Paths {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(config.EnvDataDir, t.TempDir())
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() error = %v", err)
	}
	return paths
}

// claudeHome resolves the Claude Code directory under the HOME isolate is set to,
// through the same function every command uses.
//
// A fixture that joined ".claude" itself could seed a directory the commands do
// not read, and the test would then pass on a resolver pointed somewhere else
// entirely. Going through the resolver means a fixture and the command under test
// cannot disagree about where the harness's files are.
func claudeHome(t *testing.T) string {
	t.Helper()
	dir, err := config.ClaudeCodeDir()
	if err != nil {
		t.Fatalf("ClaudeCodeDir() error = %v", err)
	}
	return dir
}

// recordHookChild replaces the detached spawn with a recorder, so the test does not
// start a background copy of the test binary.
func recordHookChild(t *testing.T, err error) *[]string {
	t.Helper()
	recorded := &[]string{}
	original := hookChild
	hookChild = func(argv []string) error {
		*recorded = argv
		return err
	}
	t.Cleanup(func() { hookChild = original })
	return recorded
}

// Acceptance: the hook-invoked form always exits 0 and prints nothing on either
// stream. A trigger that wrote to a session's terminal, or exited non-zero, is a
// trigger the user turns off.
func TestQuietIngestExitsZeroAndPrintsNothing(t *testing.T) {
	isolate(t)
	recorded := recordHookChild(t, nil)

	stdout, stderr, err := runSplit(t, "ingest", "--quiet")

	if err != nil {
		t.Errorf("error = %v, want nil", err)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("stdout = %q, stderr = %q; want both empty", stdout, stderr)
	}
	self, execErr := os.Executable()
	if execErr != nil {
		t.Fatalf("os.Executable() error = %v", execErr)
	}
	want := []string{self, "ingest", "--quiet", "--hook-scan"}
	if strings.Join(*recorded, " ") != strings.Join(want, " ") {
		t.Errorf("spawned %v, want %v", *recorded, want)
	}
}

func TestQuietIngestStaysSilentWhenTheChildCannotBeStarted(t *testing.T) {
	isolate(t)
	recordHookChild(t, errors.New("sentinel"))

	stdout, stderr, err := runSplit(t, "ingest", "--quiet")

	if err != nil {
		t.Errorf("error = %v, want nil — the trigger reports nothing, ever", err)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("stdout = %q, stderr = %q; want both empty", stdout, stderr)
	}
}

// The detached child's own failures are just as silent. WAKE_DIR under an existing
// regular file resolves fine and then makes every write fail.
func TestQuietIngestStaysSilentWhenTheDataRootIsUnusable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	occupied := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(occupied, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv(config.EnvDataDir, filepath.Join(occupied, "data"))

	stdout, stderr, err := runSplit(t, "ingest", "--quiet", "--hook-scan")

	if err != nil {
		t.Errorf("error = %v, want nil", err)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("stdout = %q, stderr = %q; want both empty", stdout, stderr)
	}
}

func TestQuietIngestStaysSilentWhenTheProjectTableCannotBeRead(t *testing.T) {
	paths := isolate(t)
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(paths.ProjectsFile, []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	stdout, stderr, err := runSplit(t, "ingest", "--quiet", "--hook-scan")

	if err != nil {
		t.Errorf("error = %v, want nil", err)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("stdout = %q, stderr = %q; want both empty", stdout, stderr)
	}
}

// The flag is Wake's own re-exec detail, not a surface anyone should reach for.
func TestHookScanFlagIsHidden(t *testing.T) {
	flag := newIngestCmd().Flags().Lookup("hook-scan")
	if flag == nil {
		t.Fatal("the hook-scan flag is not registered")
	}
	if !flag.Hidden {
		t.Error("the hook-scan flag is visible; it is an internal re-exec detail")
	}

	isolate(t)
	out, err := run(t, "ingest", "--help")
	if err != nil {
		t.Fatalf("ingest --help error = %v", err)
	}
	if strings.Contains(out, "hook-scan") {
		t.Errorf("ingest --help mentions hook-scan:\n%s", out)
	}
}

// The silence is scoped to the trigger, not to the command. A user who runs `wake
// ingest` and gets nothing back has no way to find out why.
func TestVisibleIngestStillReportsItsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	occupied := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(occupied, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv(config.EnvDataDir, filepath.Join(occupied, "data"))

	if _, _, err := runSplit(t, "ingest"); err == nil {
		t.Fatal("error = nil, want the visible form to report what went wrong")
	}
}
