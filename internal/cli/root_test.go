package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// TestMain clears the harness's own relocation variable for the whole package.
//
// config.ClaudeCodeDir honours CLAUDE_CONFIG_DIR, which is how Claude Code
// relocates ~/.claude. A developer who has it set would otherwise have every
// command driven here resolve to their real Claude Code directory whatever HOME a
// test isolates — and `init` and `remove` write hooks into what they resolve.
// Clearing it once, here, keeps that impossible for tests written later too; a test
// that wants the variable sets it with t.Setenv, which restores it afterwards.
func TestMain(m *testing.M) {
	if err := os.Unsetenv(config.EnvClaudeConfigDir); err != nil {
		fmt.Fprintf(os.Stderr, "clearing %s: %v\n", config.EnvClaudeConfigDir, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

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

// `wake --help` is the first thing a new user reads and, for the consent model, often
// the only thing. It has to answer what is collected and when, where --global's path
// defaults to, where state lives, and how the three ways of undoing an install differ
// — the last of which is what ADR-0043 §3 puts in help rather than in output printed
// while a deletion runs.
func TestRootHelpIsEnoughToStart(t *testing.T) {
	long := newRootCmd().Long
	for _, want := range []string{
		"wake init", "--global", "home directory", "wake report", "wake serve",
		"WAKE_DIR", "~/.local/state/wake", "~/.config/wake",
		"wake remove", "wake remove --purge", "wake uninstall", "consent",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("`wake --help` never says %q:\n%s", want, long)
		}
	}
}

// The README's command table and the commands' own help are two descriptions of the
// same two destructive commands, and they have drifted before: the README told the
// user `wake uninstall` prints every path before deleting, which was true and useless,
// while the help said nothing at all. Now that both surfaces have to mention the
// confirmation and the way past it, this fails the build if either side drops a token
// the other keeps.
//
// Reading ../../README.md from a package test follows internal/platform's precedent.
func TestREADMEAndHelpAgreeOnTheDestructiveCommands(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	doc := string(readme)
	// Shared: each token has to appear in the command's help and in the README.
	shared := map[string][]string{
		"uninstall": {"--yes", "wake remove --purge", "~/.config/wake"},
		"remove":    {"--yes", "--purge", "~/.config/wake"},
	}
	// One-sided, because the two surfaces word the same claim differently: the help
	// says a deletion cannot be undone, the README's table calls the command
	// irreversible. Each is asserted only where it belongs.
	helpOnly := map[string][]string{"uninstall": {"cannot be undone"}}
	readmeOnly := []string{"Irreversible"}

	found := map[string]bool{}
	for _, command := range commands {
		cmd := command()
		name := cmd.Name()
		if _, wanted := shared[name]; !wanted {
			continue
		}
		found[name] = true
		for _, token := range shared[name] {
			if !strings.Contains(cmd.Long, token) {
				t.Errorf("`wake %s --help` is missing %q, which README.md carries", name, token)
			}
			if !strings.Contains(doc, token) {
				t.Errorf("README.md is missing %q, which `wake %s --help` carries", token, name)
			}
		}
		for _, token := range helpOnly[name] {
			if !strings.Contains(cmd.Long, token) {
				t.Errorf("`wake %s --help` is missing %q", name, token)
			}
		}
	}
	for name := range shared {
		if !found[name] {
			t.Errorf("no %s command is registered; the drift check covered nothing", name)
		}
	}
	for _, token := range readmeOnly {
		if !strings.Contains(doc, token) {
			t.Errorf("README.md is missing %q", token)
		}
	}
}
