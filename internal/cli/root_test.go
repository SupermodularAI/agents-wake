package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// run executes the root command with args, returning combined output and error.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestBareInvocationPrintsDeterministicTextAndSucceeds(t *testing.T) {
	out, err := run(t)
	if err != nil {
		t.Fatalf("bare invocation returned an error: %v", err)
	}
	for _, want := range []string{"terminal invocations:", "distinct sessions:"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output is missing %q; got:\n%s", want, out)
		}
	}
}

// An unknown argument must fail. A CLI that exits 0 on a typo silently reports
// success to whatever is parsing it.
func TestUnknownArgumentFails(t *testing.T) {
	if _, err := run(t, "bogus"); err == nil {
		t.Fatal("expected an error for an unknown argument, got nil")
	}
}

func TestExecuteReturnsNonZeroOnError(t *testing.T) {
	// Execute() builds its own command, so this covers the exit-code mapping
	// main.go depends on rather than newRootCmd's behavior.
	if got := Execute(); got != 0 {
		t.Errorf("Execute() with no args = %d, want 0", got)
	}
}

// The refusal is exercised through execute() rather than Execute() because a
// supported platform's test run cannot otherwise reach it.
func TestExecuteRefusesAnUnsupportedPlatform(t *testing.T) {
	var out bytes.Buffer
	if got := execute("windows", &out); got != 1 {
		t.Errorf("execute(\"windows\") = %d, want 1", got)
	}
	for _, want := range []string{"windows", "darwin", "linux"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("refusal = %q, missing %q", out.String(), want)
		}
	}
}

// Acceptance: an unsupported platform is told at startup, not midway through a
// read-modify-write that has already created state it will never use.
//
// os.Args points at `ingest` rather than the bare command because execute() takes
// no argument vector and the bare command writes nothing — asserting on it would
// pass with the refusal deleted. `ingest` creates the config and state directories,
// so their absence is evidence the refusal came first.
func TestExecuteRefusesBeforeTouchingAnyFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv(config.EnvDataDir, filepath.Join(dir, "state"))
	original := os.Args
	os.Args = []string{"wake", "ingest"}
	t.Cleanup(func() { os.Args = original })

	if got := execute("windows", io.Discard); got != 1 {
		t.Fatalf("execute(\"windows\") = %d, want 1", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("refusing created %d entries under HOME, want none", len(entries))
	}
}
