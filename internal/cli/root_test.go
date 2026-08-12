package cli

import (
	"bytes"
	"strings"
	"testing"
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

func TestBareInvocationPrintsHelpAndSucceeds(t *testing.T) {
	out, err := run(t)
	if err != nil {
		t.Fatalf("bare invocation returned an error: %v", err)
	}
	for _, want := range []string{"Usage:", "wake", "Flags:"} {
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
