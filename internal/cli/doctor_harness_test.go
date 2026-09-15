package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
)

// recordHarnessScan writes one scan record with the two per-harness sections set.
func recordHarnessScan(t *testing.T, paths config.Paths, scan health.Scan) {
	t.Helper()
	scan.At = time.Now().UTC()
	if err := health.New(paths.HealthFile).RecordScan(scan); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}
}

func TestAnUnscannedMachineReportsEveryHarnessNotObserved(t *testing.T) {
	isolate(t)
	out, err := run(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v:\n%s", err, out)
	}
	for _, want := range []string{
		"claude code: not observed",
		"claude code sources: not observed",
		"claude code events written: not observed",
		"opencode: not observed",
		"opencode sources: not observed",
		"opencode events written: not observed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// Never a 0 for a harness nobody looked at: that reads as a measurement, and a
	// removal recommendation built on it would be wrong (ADR-0046).
	if strings.Contains(out, "opencode sources: 0") {
		t.Errorf("an unobserved harness rendered a zero:\n%s", out)
	}
}

func TestAHarnessWithCountersReportsThem(t *testing.T) {
	paths := isolate(t)
	recordHarnessScan(t, paths, health.Scan{
		EventsWritten: 40,
		ClaudeCode:    health.HarnessScan{Observed: true, Sources: 12, EventsWritten: 40},
		OpenCode:      health.HarnessScan{Observed: true, Sources: 3756, EventsWritten: 3692, UnknownOutcomes: 2},
	})

	out, err := run(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v:\n%s", err, out)
	}
	for _, want := range []string{
		"claude code: collecting",
		"claude code sources: 12",
		"claude code events written: 40",
		"opencode sources: 3756",
		"opencode unknown outcomes: 2",
		"opencode events written: 3692",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// TestAnUnreadOpenCodeIsNotObservedWhileClaudeCodeCollects is the acceptance
// criterion, in all three directions: a harness that was read and failed prints
// "collects nothing", one that was read and found nothing prints "collects zero",
// and one nobody read prints "not observed".
func TestAnUnreadOpenCodeIsNotObservedWhileClaudeCodeCollects(t *testing.T) {
	for _, c := range []struct {
		name     string
		openCode health.HarnessScan
		want     string
	}{
		{"never read", health.HarnessScan{}, "opencode: not observed"},
		{"read and blind", health.HarnessScan{Observed: true, Unreadable: 1}, "opencode: collects nothing"},
		{"drifted", health.HarnessScan{Observed: true, EventsWritten: 9, UnknownOutcomes: 3}, "opencode: collects nothing"},
		{"read and empty", health.HarnessScan{Observed: true, Sources: 5}, "opencode: collects zero"},
		{"collecting", health.HarnessScan{Observed: true, Sources: 5, EventsWritten: 5}, "opencode: collecting"},
	} {
		t.Run(c.name, func(t *testing.T) {
			paths := isolate(t)
			recordHarnessScan(t, paths, health.Scan{
				EventsWritten: 40,
				ClaudeCode:    health.HarnessScan{Observed: true, Sources: 12, EventsWritten: 40},
				OpenCode:      c.openCode,
			})

			out, err := run(t, "doctor")
			if err != nil {
				t.Fatalf("doctor error = %v:\n%s", err, out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("output is missing %q:\n%s", c.want, out)
			}
			// Whatever opencode's state, Claude Code's line is unchanged: adapters
			// fail independently and soft.
			if !strings.Contains(out, "claude code: collecting") {
				t.Errorf("Claude Code's line moved with opencode's:\n%s", out)
			}
		})
	}
}

// TestNoHarnessLineCarriesASlash is load-bearing rather than redundant: doctor's
// own no-path assertion does not reach the extension sections, as the boundary
// section's test says in as many words.
func TestNoHarnessLineCarriesASlash(t *testing.T) {
	paths := isolate(t)
	recordHarnessScan(t, paths, health.Scan{
		ClaudeCode: health.HarnessScan{Observed: true, Sources: 12, EventsWritten: 40},
		OpenCode:   health.HarnessScan{Observed: true, Sources: 3756, EventsWritten: 3692},
	})

	out, err := run(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v:\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "claude code") && !strings.HasPrefix(line, "opencode") {
			continue
		}
		if strings.Contains(line, "/") {
			t.Errorf("harness line carries a path separator: %q", line)
		}
	}
}

func TestAnUnreadableHealthFileDegrades(t *testing.T) {
	paths := isolate(t)
	if err := os.MkdirAll(filepath.Dir(paths.HealthFile), 0o700); err != nil {
		t.Fatalf("creating the data dir: %v", err)
	}
	if err := os.WriteFile(paths.HealthFile, []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("writing the counter file: %v", err)
	}
	out, _ := run(t, "doctor")
	if !strings.Contains(out, "harnesses: unreadable") {
		t.Errorf("output is missing the degraded line:\n%s", out)
	}
	if strings.Contains(out, "opencode: not observed") {
		t.Errorf("a counter file nobody could read reported a harness as unobserved:\n%s", out)
	}
}
