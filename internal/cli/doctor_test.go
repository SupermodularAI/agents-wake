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

func TestDoctorReportsNeverScannedOnAFreshInstall(t *testing.T) {
	isolate(t)

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}

	for _, want := range []string{"integration: never scanned", "last scan: never"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// ADR-0010's distinction, which is the whole reason the counters exist: a scan that
// could not read a source is not a scan that read everything and found nothing.
func TestDoctorDistinguishesCollectsNothingFromCollectsZero(t *testing.T) {
	for _, c := range []struct {
		name string
		scan health.Scan
		want string
	}{
		{"a source that could not be read", health.Scan{Unreadable: 1}, "integration: collects nothing"},
		{"a source that could not be parsed", health.Scan{ParseErrors: 1}, "integration: collects nothing"},
		{"every source read and none held an event", health.Scan{Transcripts: 3, Skipped: 3}, "integration: collects zero"},
		{"events written", health.Scan{EventsWritten: 4}, "integration: collecting"},
	} {
		t.Run(c.name, func(t *testing.T) {
			paths := isolate(t)
			c.scan.At = time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
			if err := health.New(paths.HealthFile).RecordScan(c.scan); err != nil {
				t.Fatalf("RecordScan() error = %v", err)
			}

			out, _, err := runSplit(t, "doctor")
			if err != nil {
				t.Fatalf("doctor error = %v", err)
			}

			if !strings.Contains(out, c.want) {
				t.Errorf("output is missing %q:\n%s", c.want, out)
			}
			if !strings.Contains(out, "last scan: 2026-08-17T10:00:00Z") {
				t.Errorf("output is missing the scan timestamp:\n%s", out)
			}
		})
	}
}

// doctor output is what people paste into issues. It carries counts and one state
// word, and never a path, a label or an id.
func TestDoctorOutputNamesNoPathOrLabel(t *testing.T) {
	paths := isolate(t)
	const marker = "unmistakable"
	root := filepath.Join(t.TempDir(), marker+"-repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	id, err := repos.Register(root, marker+"-label")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if recordErr := health.New(paths.HealthFile).RecordScan(health.Scan{At: time.Now().UTC(), Transcripts: 2}); recordErr != nil {
		t.Fatalf("RecordScan() error = %v", recordErr)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}

	for _, secret := range []string{marker, root, id} {
		if strings.Contains(out, secret) {
			t.Errorf("output carries %q:\n%s", secret, out)
		}
	}
	// The strongest form of the check: nothing in this output has a legitimate
	// slash in it, so a slash is a path.
	if strings.Contains(out, "/") {
		t.Errorf("output carries a path separator:\n%s", out)
	}
}

func TestDoctorReportsTheInstalledHookCount(t *testing.T) {
	paths := isolate(t)
	if err := health.New(paths.HealthFile).RecordHooks(health.Hooks{At: time.Now().UTC(), Installed: 2, KeptOwned: 1}); err != nil {
		t.Fatalf("RecordHooks() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}

	for _, want := range []string{"hooks installed: 2", "owned hook groups kept: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// doctor reporting a diagnostic failure as a command failure would make it useless
// in the one situation it exists for.
func TestDoctorSucceedsWhenTheCounterFileIsCorrupt(t *testing.T) {
	paths := isolate(t)
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(paths.HealthFile, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v, want nil — a diagnostic must report its own failure, not fail", err)
	}

	if !strings.Contains(out, "integration: counters unreadable") {
		t.Errorf("output is missing the unreadable-counters state:\n%s", out)
	}
}
