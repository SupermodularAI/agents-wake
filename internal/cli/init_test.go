package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// Acceptance item 2, mechanically: `init`'s RunE starts no process and discovers no
// root itself.
//
// The test reads the source because "no logic lives here" is a property of the file,
// not of an output: a command that shelled out to git and printed the same thing
// would satisfy every behavioural assertion in this package. ADR-0001 and plan §6.2
// put every decision below this layer, and which directory gets consented is the
// decision `init` used to make here.
func TestInitCommandRunsNoProcessAndDiscoversNoRootItself(t *testing.T) {
	// A `go test` binary starts in its own package directory and no test in this
	// package calls t.Parallel(), so the relative path is stable.
	raw, err := os.ReadFile("init.go")
	if err != nil {
		t.Fatalf("reading init.go: %v", err)
	}
	for _, forbidden := range []string{"exec.Command", "os/exec", "rev-parse"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("init.go names %q; root discovery belongs in internal/config, and this layer only parses and prints", forbidden)
		}
	}
}

// Acceptance item 3, end to end: the behaviour the move has to preserve.
//
// Run from a subdirectory, `init` must still consent to the enclosing repository —
// consent is given for a repository, and a record of the subdirectory would make
// every later scan collect part of it and report a complete pass (ADR-0019 §1).
// This is also the assertion that fails if the delegated resolver silently fell back
// to the working directory.
func TestInitRegistersTheEnclosingRepositoryRootFromASubdirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	paths := isolate(t)

	// Symlink-resolved before git sees it: on darwin t.TempDir() sits behind
	// /var → /private/var, and both git and config.Register report the resolved
	// spelling.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temporary directory: %v", err)
	}
	repo := filepath.Join(base, "repo")
	nested := filepath.Join(repo, "nested")
	if mkErr := os.MkdirAll(nested, 0o700); mkErr != nil {
		t.Fatalf("creating %s: %v", nested, mkErr)
	}
	if output, initErr := exec.Command("git", "init", repo).CombinedOutput(); initErr != nil {
		t.Fatalf("git init: %v: %s", initErr, output)
	}
	t.Chdir(nested)

	out, err := run(t, "init")
	if err != nil {
		t.Fatalf("init from a repository subdirectory: %v; output:\n%s", err, out)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolving the working directory: %v", err)
	}
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	got, err := repos.ConsentedRoot(cwd)
	if err != nil {
		t.Fatalf("ConsentedRoot() error = %v", err)
	}
	if got != repo {
		t.Errorf("the consented root is %q, want the repository root %q", got, repo)
	}

	// The disclosure still precedes the result, and still names the files this run
	// will change (ADR-0010). The whole list is asserted by
	// TestInitDisclosesAndImportsHistoryOnlyWithFull below; these keep this case
	// honest about the ordering without duplicating it.
	for _, want := range []string{
		paths.ConfigFile,
		filepath.Join(claudeHome(t), "settings.json"),
		"Claude Code collection enabled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init output is missing %q; got:\n%s", want, out)
		}
	}
}

// The disclosure says whether this invocation will touch the event spool, and both
// paths are driven end to end (ADR-0024, ADR-0010).
//
// The transcript is written before either run, so the default path has real history
// available and still imports none of it — the assertion is about what init does,
// not about what was there to find. The spool's absence afterwards is the
// filesystem's witness of the same thing.
func TestInitDisclosesAndImportsHistoryOnlyWithFull(t *testing.T) {
	// Neutralised for the reason its neighbour above neutralises them: root discovery
	// runs git, and a global or system config — an includeIf, a safe.directory, a
	// core.hooksPath — would otherwise decide what this test consents to.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	paths := isolate(t)
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll() root error = %v", err)
	}
	t.Chdir(root)
	consented, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	transcriptDir := filepath.Join(claudeHome(t), "projects", "session")
	if mkErr := os.MkdirAll(transcriptDir, 0o700); mkErr != nil {
		t.Fatalf("MkdirAll() transcript error = %v", mkErr)
	}
	transcript := `{"uuid":"entry-1","sessionId":"session-1","cwd":"` + consented + `","timestamp":"2026-08-17T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}
{"uuid":"entry-2","sessionId":"session-1","cwd":"` + consented + `","timestamp":"2026-08-17T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`
	if writeErr := os.WriteFile(filepath.Join(transcriptDir, "session.jsonl"), []byte(transcript), 0o600); writeErr != nil {
		t.Fatalf("WriteFile() transcript error = %v", writeErr)
	}
	spool := filepath.Join(paths.DataDir, "events.ndjson")

	out, err := run(t, "init")
	if err != nil {
		t.Fatalf("init error = %v; output:\n%s", err, out)
	}
	// The spool is not in the modify list — a whole line of its own is what listing
	// it looks like — and the sentence says so in words, naming --full.
	if strings.Contains(out, "\n"+spool+"\n") {
		t.Errorf("plain init listed the event spool as a file it will modify:\n%s", out)
	}
	for _, want := range []string{
		paths.ConfigFile,
		paths.SaltFile,
		paths.ProjectsFile,
		paths.PrimitivesFile,
		paths.HealthFile,
		filepath.Join(claudeHome(t), "settings.json"),
		"Existing Claude Code history will not be imported, so " + spool + " is not written;",
		// The disclosure is about the triggers too, not only about this call: they are
		// what would otherwise import the history one session later (ADR-0025).
		"the session triggers this installs collect only what happens from now on",
		`Run "wake init --full" to import it now.`,
		"collection starts now",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plain init output is missing %q; got:\n%s", want, out)
		}
	}
	// The count line is what an import that ran looks like, and it is the phrase to
	// forbid here: the negative sentence above necessarily contains the word
	// "imported", so the word alone cannot distinguish the two paths. Singular, so it
	// catches both spellings of the count.
	if strings.Contains(out, "terminal event") {
		t.Errorf("plain init reported a count of imported events it never scanned for:\n%s", out)
	}
	// Every path the disclosure listed is one this run actually wrote. A list that
	// names a file init leaves alone is as much a wrong disclosure as one that omits a
	// file it writes, and the two files added here are the ones nobody asks for.
	for _, path := range []string{paths.ConfigFile, paths.SaltFile, paths.ProjectsFile, paths.PrimitivesFile, paths.HealthFile} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("Stat(%q) = %v, want a file the disclosure named to exist afterwards", path, statErr)
		}
	}
	if _, statErr := os.Stat(spool); !os.IsNotExist(statErr) {
		t.Errorf("Stat(spool) = %v, want plain init to create no spool", statErr)
	}

	out, err = run(t, "init", "--full")
	if err != nil {
		t.Fatalf("init --full error = %v; output:\n%s", err, out)
	}
	if !strings.Contains(out, "\n"+spool+"\n") {
		t.Errorf("init --full did not disclose the event spool it writes:\n%s", out)
	}
	for _, want := range []string{
		"Existing Claude Code history will be imported now.",
		// Two: the transcript's one call, and the session_end for its long-silent
		// session id (ADR-0034).
		"Claude Code collection enabled; imported 2 terminal events.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init --full output is missing %q; got:\n%s", want, out)
		}
	}
	if _, statErr := os.Stat(spool); statErr != nil {
		t.Errorf("Stat(spool) = %v, want init --full to have written it", statErr)
	}
}
