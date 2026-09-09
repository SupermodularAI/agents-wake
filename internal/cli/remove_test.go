package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/activation"
)

// This file is a regression pin, not new coverage. `wake uninstall` arrived beside
// `remove` and shares activation.Uninstall with it, so the thing most likely to break
// is `remove`'s scope — widening `--purge` to take the config root too would make
// `uninstall` redundant and would silently re-identify every repository on the next
// `init` (ADR-0019 §3). These three tests fail if that happens.

// Acceptance: `remove` takes the hooks and keeps both roots.
func TestRemoveRemovesWakeHooksAndKeepsBothRoots(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	settings := filepath.Join(claudeHome(t), "settings.json")
	writeFixture(t, settings, settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")

	out, err := run(t, "remove")

	if err != nil {
		t.Fatalf("remove returned an error: %v\n%s", err, out)
	}
	want := "Removed Wake's Claude Code integration.\n" +
		fmt.Sprintf("Local data was kept at %s. Remove it with `wake remove --purge`.\n", paths.DataDir)
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	for _, kept := range []string{filepath.Join(paths.DataDir, "events.ndjson"), paths.ConfigFile} {
		if _, statErr := os.Stat(kept); statErr != nil {
			t.Errorf("Stat(%s) error = %v; `remove` without --purge keeps local state", kept, statErr)
		}
	}
	body, readErr := os.ReadFile(settings)
	if readErr != nil {
		t.Fatalf("ReadFile(settings) error = %v", readErr)
	}
	if !strings.Contains(string(body), "existing command") {
		t.Errorf("settings = %s; the user's own hook entry is gone", body)
	}
	if strings.Contains(string(body), `"wake"`) {
		t.Errorf("settings = %s; Wake's marked group is still there", body)
	}
}

// Acceptance: `--purge` takes the data root and only the data root. The config-root
// assertion is the one that fails if anyone widens `--purge` while implementing
// `uninstall`.
func TestRemovePurgeRemovesOnlyTheDataRoot(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	settings := filepath.Join(claudeHome(t), "settings.json")
	writeFixture(t, settings, settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")

	out, err := run(t, "remove", "--purge", "--yes")

	if err != nil {
		t.Fatalf("remove --purge returned an error: %v\n%s", err, out)
	}
	// Exact rather than a set of Contains checks, so the disclosure's wording is a
	// pinned contract: --yes skips the question, never the paths (ADR-0043 §2).
	want := "Wake will permanently delete:\n" +
		fmt.Sprintf("%s  Wake's own hook entry only; your other hooks are left as they are\n", settings) +
		fmt.Sprintf("%s  all collected activity and the local project map\n", paths.DataDir) +
		fmt.Sprintf("Configuration and the local identity salt at %s are kept; `wake uninstall` removes those too.\n", paths.ConfigDir) +
		"Removed Wake's Claude Code integration.\n" +
		fmt.Sprintf("Removed local data at %s.\n", paths.DataDir)
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	absent(t, paths.DataDir)
	if _, statErr := os.Stat(paths.ConfigFile); statErr != nil {
		t.Errorf("Stat(config.toml) error = %v; --purge keeps the config root, which is what makes `uninstall` a separate command", statErr)
	}
}

// Acceptance: an absent integration is reported plainly rather than treated as a fault.
func TestRemoveOnASystemThatWasNeverInitedReportsItWasNotInstalled(t *testing.T) {
	isolateUnderOneHome(t)

	out, err := run(t, "remove")

	if err != nil {
		t.Fatalf("remove returned an error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Wake's Claude Code integration was not installed.") {
		t.Errorf("output does not report the absent integration plainly:\n%s", out)
	}
}

// runRemove drives `remove` with a fake terminal in its place, through the same RunE
// the command tree builds. Standard input is a strings.Reader so the real osPrompter
// is never consulted.
func runRemove(t *testing.T, newPrompter promptFactory, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newRemoveCmdWith(newPrompter)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	err := cmd.Execute()
	return out.String(), err
}

// Acceptance (ADR-0043 §1): `--purge` gains the disclosure it never had, and it comes
// before the question. echoPrompter puts the question into the same stream, so the
// order is one index comparison.
func TestRemovePurgeDisclosesThePathsBeforeAsking(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	claudeDir := claudeHome(t)
	writeFixture(t, filepath.Join(claudeDir, "settings.json"), settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	var out bytes.Buffer
	cmd := newRemoveCmdWith(func(*cobra.Command) prompter { return &echoPrompter{out: &out, answer: "n"} })
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--purge"})
	cmd.SilenceUsage = true

	if err := cmd.Execute(); err != nil {
		t.Fatalf("remove --purge returned an error: %v\n%s", err, out.String())
	}

	printed := out.String()
	question := strings.Index(printed, "Permanently delete this data?")
	if question < 0 {
		t.Fatalf("the question was never put:\n%s", printed)
	}
	for _, disclosed := range []string{activation.SettingsFilePath(claudeDir), paths.DataDir} {
		at := strings.Index(printed, disclosed)
		if at < 0 {
			t.Errorf("the disclosure never names %q; got:\n%s", disclosed, printed)
			continue
		}
		if at > question {
			t.Errorf("%q is disclosed after the question was put; got:\n%s", disclosed, printed)
		}
	}
}

// Acceptance: answering no deletes nothing — not the data, and not even the hook
// entry, which is the step `--purge` shares with plain `remove`.
func TestRemovePurgeAbortedAtThePromptDeletesNothing(t *testing.T) {
	paths, home := isolateUnderOneHome(t)
	settings := filepath.Join(claudeHome(t), "settings.json")
	writeFixture(t, settings, settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	before := snapshot(t, home)
	if _, ok := before[settings]; !ok {
		t.Fatal("the snapshot never saw the settings file; the diff would be vacuous")
	}
	fake := &fakeTerminal{answers: []string{"n"}}

	out, err := runRemove(t, fake.factory(), "--purge")

	if err != nil {
		t.Fatalf("a declined remove --purge returned an error: %v\n%s", err, out)
	}
	if !strings.Contains(out, nothingDeleted) {
		t.Errorf("output does not say plainly that nothing was deleted:\n%s", out)
	}
	for path, was := range snapshotDiffBase(before, snapshot(t, home)) {
		t.Errorf("%s changed after the deletion was declined: %s", path, was)
	}
}

// snapshotDiffBase returns every path in before that after does not still hold
// unchanged, rendered for the failure message.
func snapshotDiffBase(before, after map[string]string) map[string]string {
	changed := map[string]string{}
	for path, was := range before {
		if now, still := after[path]; !still || now != was {
			changed[path] = fmt.Sprintf("was %q, now %q (present=%t)", was, now, still)
		}
	}
	return changed
}

// Acceptance (ADR-0043 §2): unattended, `--purge` refuses rather than deleting every
// record on the machine with nobody watching.
func TestRemovePurgeWithoutATerminalRefusesAndNamesYes(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	writeFixture(t, filepath.Join(claudeHome(t), "settings.json"), settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")

	out, err := runRemove(t, osPrompter, "--purge")

	if err == nil {
		t.Fatalf("remove --purge returned nil, want the refusal; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("refusal = %q, want it to name --yes", err.Error())
	}
	if strings.Contains(out, "Wake will permanently delete") {
		t.Errorf("the refusal promised a deletion it never made:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(paths.DataDir, "events.ndjson")); statErr != nil {
		t.Errorf("Stat(events.ndjson) error = %v; the refusal must leave the data in place", statErr)
	}
}

// ADR-0043 §1 is explicit that plain `remove` is not gated: a confirmation on a
// harmless, `wake init`-reversible command trains the user to answer yes without
// reading, which costs the gate its value on the two commands that need it. Pinned so
// a later change cannot quietly extend the prompt to it.
func TestPlainRemoveIsNotGated(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	settings := filepath.Join(claudeHome(t), "settings.json")
	writeFixture(t, settings, settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	fake := &fakeTerminal{answers: []string{"n"}}

	out, err := runRemove(t, fake.factory())

	if err != nil {
		t.Fatalf("remove returned an error: %v\n%s", err, out)
	}
	if len(fake.shown) != 0 {
		t.Errorf("plain `remove` asked for confirmation: %q", fake.shown)
	}
	if strings.Contains(out, "Wake will permanently delete") {
		t.Errorf("plain `remove` printed a deletion disclosure:\n%s", out)
	}
	body, readErr := os.ReadFile(settings)
	if readErr != nil {
		t.Fatalf("ReadFile(settings) error = %v", readErr)
	}
	if strings.Contains(string(body), `"wake"`) {
		t.Errorf("settings = %s; the hook entry survived an ungated `remove`", body)
	}
}

// C12 / ADR-0019 §3: `--purge` keeps the config root, and the identity salt lives
// under it. A disclosure claiming otherwise would tell the user their repositories
// are about to be re-identified when they are not — asserted in both the words and
// the effect.
func TestRemovePurgeDisclosureDoesNotClaimTheSaltIsDeleted(t *testing.T) {
	paths, _ := isolateUnderOneHome(t)
	writeFixture(t, filepath.Join(claudeHome(t), "settings.json"), settingsWith(userHookGroup, wakeHookGroup))
	writeFixture(t, filepath.Join(paths.DataDir, "events.ndjson"), "seeded")
	writeFixture(t, paths.ConfigFile, "ui.default_window = \"7d\"\n")
	writeFixture(t, paths.SaltFile, "seeded-salt")

	out, err := runRemove(t, osPrompter, "--purge", "--yes")

	if err != nil {
		t.Fatalf("remove --purge --yes returned an error: %v\n%s", err, out)
	}
	var named bool
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "salt") && strings.Contains(line, "delete") {
			t.Errorf("the disclosure names the salt as deleted: %q", line)
		}
		if !strings.Contains(line, paths.ConfigDir) {
			continue
		}
		named = true
		if !strings.Contains(line, "are kept") {
			t.Errorf("the config root is named on a line that does not say it is kept: %q", line)
		}
	}
	if !named {
		t.Errorf("the disclosure never says what happens to the config root:\n%s", out)
	}
	for _, kept := range []string{paths.ConfigFile, paths.SaltFile} {
		if _, statErr := os.Stat(kept); statErr != nil {
			t.Errorf("Stat(%s) error = %v; --purge keeps the config root", kept, statErr)
		}
	}
}

// ADR-0043 §3, for the command whose two forms differ by how much they delete: help
// has to name both, say which one is gated, and point at the irreversible one.
func TestRemoveHelpNamesBothFormsAndTheIrreversibleOne(t *testing.T) {
	for _, command := range commands {
		cmd := command()
		if cmd.Name() != "remove" {
			continue
		}
		for _, want := range []string{"Forms:", "wake remove --purge", "wake uninstall", "cannot be undone", "--yes", "~/.config/wake"} {
			if !strings.Contains(cmd.Long, want) {
				t.Errorf("`wake remove --help` never says %q:\n%s", want, cmd.Long)
			}
		}
		return
	}
	t.Fatal("no remove command is registered")
}
