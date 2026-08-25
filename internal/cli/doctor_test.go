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
		// A project table entry this build refuses is attribution it could not
		// perform, so every transcript belonging to that repository resolved to no
		// consented project — the numbers below are missing all of it. This is the
		// state every install written before match_mac reaches on its first scan.
		{"a project entry this build refuses", health.Scan{Transcripts: 1, Skipped: 1, RefusedProjects: 1}, "integration: collects nothing"},
		// A call this build could not name is collection it lost: the primitive was
		// invoked and the numbers do not carry it. This is the shape a Claude Code
		// field rename takes, and calling it a complete count of zero is how
		// `unused` would come to recommend removing a subagent the user runs daily.
		{"a call whose primitive name this build refuses", health.Scan{Transcripts: 1, RefusedCalls: 1}, "integration: collects nothing"},
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

// The count itself, not only the state word: "how much did it lose" is the next
// question after "is it collecting", and a counter nothing prints cannot answer it.
func TestDoctorReportsRefusedCalls(t *testing.T) {
	paths := isolate(t)
	if err := health.New(paths.HealthFile).RecordScan(health.Scan{At: time.Now().UTC(), Transcripts: 1, RefusedCalls: 2}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "refused calls: 2") {
		t.Errorf("output is missing the refused-call count:\n%s", out)
	}
}

func TestDoctorReportsPendingAndInterruptedCallsSeparately(t *testing.T) {
	// Two lines, not one. "Buffered, may still finish" and "resolved as never
	// finishing" are different facts, and one number would conflate them — which is
	// the conflation the ticket exists to prevent.
	paths := isolate(t)
	if err := health.New(paths.HealthFile).RecordScan(health.Scan{At: time.Now().UTC(), Transcripts: 1, PendingCalls: 2, InterruptedCalls: 1}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "pending calls: 2") {
		t.Errorf("output is missing the pending-call count:\n%s", out)
	}
	if !strings.Contains(out, "interrupted calls: 1") {
		t.Errorf("output is missing the interrupted-call count:\n%s", out)
	}
}

func TestDoctorDoesNotCallPendingCallsLostCollection(t *testing.T) {
	// A buffered call is a number that is not final yet, and a call that resolved to
	// interrupted is an invocation the store has. Neither is a source nobody could
	// read, so neither may move integration state into "collects nothing".
	paths := isolate(t)
	if err := health.New(paths.HealthFile).RecordScan(health.Scan{At: time.Now().UTC(), EventsWritten: 1, PendingCalls: 5, InterruptedCalls: 3}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "integration: collecting") {
		t.Errorf("integration state is not collecting:\n%s", out)
	}
	if strings.Contains(out, "collects nothing") {
		t.Errorf("pending or interrupted calls were reported as lost collection:\n%s", out)
	}
	// No threshold is printed with them. The value is provisional and uncalibrated
	// (ADR-0014), and a duration here would read as a calibrated promise.
	for _, unit := range []string{"24h", "h0m0s", "30m"} {
		if strings.Contains(out, unit) {
			t.Errorf("output names a duration %q, which reads as a calibrated threshold:\n%s", unit, out)
		}
	}
}

// A rebuild that is still owed has to name the command that performs it, because the
// scan that found the records may be the hook-fired one, which is not allowed to
// perform it: it collects inside each repository's recorded boundary (ADR-0025), so it
// would re-derive less than it deleted. Until someone runs the command, those records
// are in the store and no surface reads them — lost collection, not an honest zero.
func TestDoctorNamesTheRebuildAScanCouldNotPerform(t *testing.T) {
	paths := isolate(t)
	if err := health.New(paths.HealthFile).RecordScan(health.Scan{At: time.Now().UTC(), Transcripts: 1, EventsWritten: 2, StaleRecords: 3}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "records from an earlier schema version: 3") {
		t.Errorf("output is missing the stale-record count:\n%s", out)
	}
	if !strings.Contains(out, "store rebuild: run wake ingest --rebuild") {
		t.Errorf("output does not name the command that rebuilds the store:\n%s", out)
	}
	if !strings.Contains(out, "integration: collects nothing") {
		t.Errorf("records nothing can read were reported as an honest zero:\n%s", out)
	}
}

// The other side: a scan that did rebuild says so, and must not tell the user to run
// the command again or contradict the events it just wrote.
func TestDoctorReportsARebuildThatHappened(t *testing.T) {
	paths := isolate(t)
	if err := health.New(paths.HealthFile).RecordScan(health.Scan{At: time.Now().UTC(), Transcripts: 1, EventsWritten: 2, StaleRecords: 3, StaleRebuilt: true}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "store rebuild: done") {
		t.Errorf("output does not report the rebuild that happened:\n%s", out)
	}
	if !strings.Contains(out, "integration: collecting") {
		t.Errorf("the scan that rebuilt the spool was reported as collecting nothing:\n%s", out)
	}
}

func TestDoctorReportsAmbiguousSkillRuns(t *testing.T) {
	// Its own line, because it is its own fact: how many attributed skill runs were
	// collapsed into an already-counted one, which is uncertainty about the invocation
	// numbers rather than a number of invocations (ADR-0023's accepted limitation).
	paths := isolate(t)
	if err := health.New(paths.HealthFile).RecordScan(health.Scan{At: time.Now().UTC(), Transcripts: 1, AmbiguousSkillRuns: 2}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "ambiguous skill runs: 2") {
		t.Errorf("output is missing the ambiguous-skill-run count:\n%s", out)
	}
}

// The collapse ADR-0023 accepts is uncertainty about a number, not a source nobody
// could read. Reporting it as lost collection would make every session with a repeated
// slash-command read as broken, and would never clear.
func TestDoctorDoesNotCallAmbiguousSkillRunsLostCollection(t *testing.T) {
	paths := isolate(t)
	if err := health.New(paths.HealthFile).RecordScan(health.Scan{At: time.Now().UTC(), EventsWritten: 1, AmbiguousSkillRuns: 3}); err != nil {
		t.Fatalf("RecordScan() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "integration: collecting") {
		t.Errorf("integration state is not collecting:\n%s", out)
	}
	if strings.Contains(out, "collects nothing") {
		t.Errorf("a collapsed skill run was reported as lost collection:\n%s", out)
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
	id, err := repos.Register(root, marker+"-label", time.Time{})
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

// The installed count is read live out of the settings file, so it answers "are
// the hooks there now" rather than "what did init once do". The two differ exactly
// when somebody edited that file since, which is when the question gets asked.
func TestDoctorReportsTheLiveHookCountNotTheRecordedOne(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	t.Chdir(root)
	if _, _, err := runSplit(t, "init"); err != nil {
		t.Fatalf("init error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "hooks installed: 2") {
		t.Errorf("output is missing the live hook count:\n%s", out)
	}

	// The hooks are deleted by hand, and nothing re-records anything. The recorded
	// count still says init installed two; the live count is the honest answer.
	settings := filepath.Join(claudeHome(t), "settings.json")
	if writeErr := os.WriteFile(settings, []byte(`{"model":"opus"}`), 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	out, _, err = runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(out, "hooks installed: 0") {
		t.Errorf("output reports hooks that are not there:\n%s", out)
	}
}

func TestDoctorReportsTheKeptOwnedCount(t *testing.T) {
	paths := isolate(t)
	if err := health.New(paths.HealthFile).RecordHooks(health.Hooks{At: time.Now().UTC(), Removed: 1, KeptOwned: 1}); err != nil {
		t.Fatalf("RecordHooks() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}

	for _, want := range []string{"hooks removed: 1", "owned hook groups kept: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// A settings file this build refuses is exactly what a user comes to doctor about,
// having been told by `wake init` to fix it. Reporting zero hooks would send them
// looking in the wrong place.
func TestDoctorReportsAnUnreadableSettingsFile(t *testing.T) {
	isolate(t)
	claudeDir := claudeHome(t)
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`null`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	out, _, err := runSplit(t, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v, want nil", err)
	}

	if !strings.Contains(out, "integration: hooks unreadable") {
		t.Errorf("output is missing the unreadable-hooks state:\n%s", out)
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
