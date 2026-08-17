package activation

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/health"
	"github.com/SupermodularAI/agents-wake/internal/lockfile"
)

// triggerFixture consents to a repository with one transcript in it, so a Trigger
// has something to find.
func triggerFixture(t *testing.T) (paths config.Paths, claudeDir, root string) {
	t.Helper()
	paths = testPaths(t)
	claudeDir, root = inventoryFixture(t)
	if _, err := Init(paths, root, claudeDir, testExecutable(t)); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	return paths, claudeDir, root
}

func TestTriggerScansWhenTheLockIsFree(t *testing.T) {
	paths, claudeDir, _ := triggerFixture(t)

	ran, err := Trigger(paths, claudeDir)
	if err != nil {
		t.Fatalf("Trigger() error = %v", err)
	}

	if !ran {
		t.Error("ran = false, want true when nothing else is scanning")
	}
	if _, statErr := os.Stat(filepath.Join(paths.DataDir, eventsFile)); statErr != nil {
		t.Errorf("Stat(events) = %v, want the spool to exist", statErr)
	}
}

// Single-flight, not a queue. ADR-0016 rules out concurrent session-ends each
// running a full independent scan, and skipping is safe because every id is derived
// from the source event: whatever this run skips, the next SessionStart picks up.
func TestTriggerSkipsWhenAnotherScanHoldsTheLock(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	if _, err := Init(paths, root, claudeDir, testExecutable(t)); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	// Init already scanned, so the spool exists; the assertion below is that this
	// trigger did not run at all, which the health counters record.
	if err := os.Remove(filepath.Join(paths.DataDir, eventsFile)); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	held := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- lockfile.WithLock(filepath.Join(paths.DataDir, ingestLockName), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	ran, err := Trigger(paths, claudeDir)
	close(release)

	if err != nil {
		t.Errorf("Trigger() error = %v, want nil — another scan running is not a failure", err)
	}
	if ran {
		t.Error("ran = true, want false while another scan holds the lock")
	}
	if _, statErr := os.Stat(filepath.Join(paths.DataDir, eventsFile)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("Stat(events) = %v, want ErrNotExist — the skipped trigger scanned anyway", statErr)
	}
	if holderErr := <-holderDone; holderErr != nil {
		t.Fatalf("holder WithLock() error = %v", holderErr)
	}
}

func TestTriggerRecordsTheScanCounters(t *testing.T) {
	paths, claudeDir, _ := triggerFixture(t)

	if _, err := Trigger(paths, claudeDir); err != nil {
		t.Fatalf("Trigger() error = %v", err)
	}

	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if report.Scan.At.IsZero() {
		t.Error("Scan.At is zero; the trigger recorded no scan")
	}
	if report.Scan.Transcripts != 1 {
		t.Errorf("Scan.Transcripts = %d, want 1", report.Scan.Transcripts)
	}
}

// The distinction ADR-0010 puts on doctor: a source that could not be read is not
// a source that held nothing. The counters have to tell them apart, and they have
// to do it without carrying a path or a line of what they read.
func TestScanCountersDistinguishAnUnreadableSourceFromACleanZero(t *testing.T) {
	transcriptOf := func(claudeDir string) string {
		return filepath.Join(claudeDir, "projects", "project", "session.jsonl")
	}

	t.Run("a readable transcript that yields no terminal event", func(t *testing.T) {
		paths := testPaths(t)
		claudeDir, root := inventoryFixture(t)
		// Truncated to the tool_use line only: the call never terminates, so no
		// terminal event is emitted (ADR-0015) and nothing is written.
		if err := os.WriteFile(transcriptOf(claudeDir), []byte(`{"uuid":"entry-1","sessionId":"session-1","cwd":"`+root+`","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if _, err := Init(paths, root, claudeDir, testExecutable(t)); err != nil {
			t.Fatalf("Init() error = %v", err)
		}

		scan := scanCounters(t, paths)
		if scan.Unreadable != 0 || scan.ParseErrors != 0 {
			t.Errorf("Unreadable = %d, ParseErrors = %d; want 0 and 0 — nothing failed", scan.Unreadable, scan.ParseErrors)
		}
		if scan.Skipped != 1 {
			t.Errorf("Skipped = %d, want 1 — the transcript was read and held no terminal event", scan.Skipped)
		}
		if scan.EventsWritten != 0 {
			t.Errorf("EventsWritten = %d, want 0", scan.EventsWritten)
		}
	})

	// A machine with no Claude Code history at all is a clean zero, not an
	// unreadable source. filepath.WalkDir reports the root's own stat error through
	// the callback, so "the directory is not there" arrives by the same route as
	// "the directory could not be read" and the two have to be told apart.
	t.Run("no transcript directory at all", func(t *testing.T) {
		paths := testPaths(t)
		claudeDir := filepath.Join(t.TempDir(), "claude")

		if _, err := Trigger(paths, claudeDir); err != nil {
			t.Fatalf("Trigger() error = %v", err)
		}

		scan := scanCounters(t, paths)
		if scan.Unreadable != 0 {
			t.Errorf("Unreadable = %d, want 0 — nothing was there to read", scan.Unreadable)
		}
		if scan.Transcripts != 0 {
			t.Errorf("Transcripts = %d, want 0", scan.Transcripts)
		}
	})

	t.Run("a transcript that cannot be opened", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("running as root: a 0o000 file is still readable")
		}
		paths := testPaths(t)
		// A fixture whose every name carries an unmistakable marker, so the
		// disclosure check below has teeth: the shared fixture's "project" is a
		// substring of the "refused_projects" counter's own key, which would make the
		// assertion fail for a reason that is not a leak.
		const marker = "unmistakable"
		claudeDir := filepath.Join(t.TempDir(), "claude")
		root := filepath.Join(t.TempDir(), marker+"-repo")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		transcript := filepath.Join(claudeDir, "projects", marker+"-dir", marker+"-session.jsonl")
		writeFixture(t, transcript, `{"uuid":"`+marker+`-entry","sessionId":"`+marker+`-session","cwd":"`+root+`","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"`+marker+`Tool"}]}}`)
		if err := os.Chmod(transcript, 0o000); err != nil {
			t.Fatalf("Chmod() error = %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(transcript, 0o600) })
		if _, err := Init(paths, root, claudeDir, testExecutable(t)); err != nil {
			t.Fatalf("Init() error = %v", err)
		}

		scan := scanCounters(t, paths)
		if scan.Unreadable != 1 {
			t.Errorf("Unreadable = %d, want 1 — a source that could not be opened must not read as a clean zero", scan.Unreadable)
		}
		if scan.Transcripts != 1 {
			t.Errorf("Transcripts = %d, want 1", scan.Transcripts)
		}

		// The counters are what a user pastes into an issue, so they must name
		// nothing. The whole file is checked, not one field.
		raw, err := os.ReadFile(paths.HealthFile)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if strings.Contains(string(raw), marker) {
			t.Errorf("health.json = %s; it carries a fragment of a path or a transcript, and a counter carries a count", raw)
		}
	})
}

// The ordering constraint the ticket puts on init, in its strongest form: an
// unsupported installation writes nothing at all, not merely nothing in the
// settings file.
func TestInitRejectsAnUnsupportedInstallationBeforeWritingAnything(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	unsupported := filepath.Join(t.TempDir(), "wake binary")
	if err := os.WriteFile(unsupported, []byte("x"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Init(paths, root, claudeDir, unsupported); err == nil {
		t.Fatal("Init() error = nil, want a refusal for an installation that cannot host a hook command")
	}

	for _, path := range []string{
		paths.ConfigFile,
		paths.ProjectsFile,
		paths.SaltFile,
		paths.HealthFile,
		filepath.Join(claudeDir, "settings.json"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("os.Lstat(%q) = %v, want ErrNotExist — a refused init must write nothing", path, err)
		}
	}
}

// Two processes editing settings.json at once must leave a file that parses and
// still holds what neither of them owns.
func TestConcurrentInitAndRemoveLeaveValidJsonAndKeepThirdPartyHooks(t *testing.T) {
	paths := testPaths(t)
	claudeDir, root := inventoryFixture(t)
	executable := testExecutable(t)
	command, err := hookCommandFor(executable)
	if err != nil {
		t.Fatalf("hookCommandFor() error = %v", err)
	}
	settings := `{"model":"opus","hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"lint"}]}],"SessionEnd":[{"hooks":[{"type":"command","command":` + quote(command) + `}]}]}}`
	if writeErr := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	var actors sync.WaitGroup
	for i := range 4 {
		actors.Add(1)
		go func() {
			defer actors.Done()
			if i%2 == 0 {
				_, _ = Init(paths, root, claudeDir, executable)
				return
			}
			_, _ = Uninstall(paths, claudeDir, false)
		}()
	}
	actors.Wait()

	raw, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var parsed map[string]any
	if parseErr := json.Unmarshal(raw, &parsed); parseErr != nil {
		t.Fatalf("settings.json does not parse as a JSON object: %v\n%s", parseErr, raw)
	}
	if parsed["model"] != "opus" {
		t.Errorf("model = %v, want \"opus\" — an unknown setting was lost", parsed["model"])
	}
	if !strings.Contains(string(raw), `"lint"`) {
		t.Errorf("settings = %s, want the third-party PreToolUse hook preserved", raw)
	}
	if !strings.Contains(string(raw), command) {
		t.Errorf("settings = %s, want the marker-less group holding Wake's command preserved", raw)
	}

	entries, err := os.ReadDir(claudeDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".publish-") {
			t.Errorf("leftover temporary file %q in the Claude Code directory", entry.Name())
		}
	}
}

func scanCounters(t *testing.T, paths config.Paths) health.Scan {
	t.Helper()
	report, err := health.New(paths.HealthFile).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	return report.Scan
}
