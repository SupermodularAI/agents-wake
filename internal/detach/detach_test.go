//go:build unix

package detach

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The property ADR-0016 asks for: the child is in its own process group, so a
// shell or a harness that signals the foreground group on terminal close does not
// take the scan with it.
func TestStartRunsTheChildInItsOwnProcessGroup(t *testing.T) {
	out := filepath.Join(t.TempDir(), "pgid")

	// ps -o pgid= -p $$ works on both released targets. The child writes its own
	// process group id and this process compares it with its own.
	if err := Start([]string{"/bin/sh", "-c", "ps -o pgid= -p $$ > " + out}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Polled rather than waited on: the parent deliberately does not wait, which is
	// what makes the hook fast.
	raw := waitForFile(t, out)
	child, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		t.Fatalf("parsing %q as a process group id: %v", raw, err)
	}
	if parent := syscall.Getpgrp(); child == parent {
		t.Errorf("the child's process group is %d, the same as this process's; it would die with the terminal", child)
	}
}

func TestStartRejectsAnEmptyCommand(t *testing.T) {
	if err := Start(nil); err == nil {
		t.Fatal("Start(nil) error = nil, want a refusal")
	}
	if err := Start([]string{}); err == nil {
		t.Fatal("Start([]) error = nil, want a refusal")
	}
}

// A binary that is not there is reported, not swallowed. The silence ADR-0016 asks
// for belongs to the caller that decides not to report; this package still says
// what happened.
func TestStartReportsAMissingBinary(t *testing.T) {
	if err := Start([]string{filepath.Join(t.TempDir(), "absent")}); err == nil {
		t.Fatal("Start() error = nil, want a refusal for a binary that does not exist")
	}
}

func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(raw)) != "" {
			return string(raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared; the detached child did not run", path)
	return ""
}
