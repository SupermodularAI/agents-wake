package platform

import (
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
)

// verifyCommand finds the artefact the README tells a user to hand to
// `gh attestation verify`.
var verifyCommand = regexp.MustCompile(`gh attestation verify\s+(\S+)`)

// TestAttestationCoversWhatTheReadmeTellsUsersToVerify guards the second
// one-set invariant in the release configuration, beside the platform set in
// matrix_test.go: the file the README documents verifying and the files
// release.yml attests must be the same file.
//
// `gh attestation verify` resolves an attestation by the sha256 digest of the
// file it is given, so a subject list that covers only the archives cannot
// answer for an extracted binary — the documented command fails with "no
// attestations found" for every user. Nothing in CI can catch that: the attest
// step needs a tag, a public repository and `id-token: write`, so the coupling
// between the two files is all that is checkable here.
func TestAttestationCoversWhatTheReadmeTellsUsersToVerify(t *testing.T) {
	readme := read(t, "../../README.md")
	target := verifyCommand.FindStringSubmatch(readme)
	if target == nil {
		t.Fatal("README.md documents no `gh attestation verify` command")
	}
	// The README installs the extracted binary, so the documented target is a
	// path to it. A future README that verifies the archive before extracting
	// instead would name a `.tar.gz`, and the archive glob below answers for it.
	want := path.Base(strings.Trim(target[1], `"'`))

	subjects := attestationSubjects(t, read(t, "../../.github/workflows/release.yml"))
	if len(subjects) == 0 {
		t.Fatal("release.yml declares no attestation subject-path")
	}
	if !covers(t, subjects, want) {
		t.Errorf("release.yml attests %v, none of which covers the README's %q", subjects, want)
	}
	// The archives are what a release publishes; losing them from the subject
	// list would leave every download unattested.
	if !covers(t, subjects, "wake_0.1.0_darwin_arm64.tar.gz") {
		t.Errorf("release.yml attests %v, none of which covers a release archive", subjects)
	}
}

// attestationSubjects reads the block-scalar value of the attest step's
// subject-path input as one glob per line.
func attestationSubjects(t *testing.T, content string) []string {
	t.Helper()
	var subjects []string
	inBlock := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "subject-path:") {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		// The block ends at the first line that is not an indented plain entry.
		if trimmed == "" || strings.HasSuffix(trimmed, ":") || strings.Contains(trimmed, ": ") {
			break
		}
		subjects = append(subjects, trimmed)
	}
	return subjects
}

// covers reports whether any subject glob matches a file of the given name. It
// compares the last path segment only: the directory a build lands in carries
// GoReleaser's GOAMD64 suffix (`dist/wake_linux_amd64_v1/`), which is why the
// glob that matches a binary has to be recursive in the first place.
func covers(t *testing.T, subjects []string, name string) bool {
	t.Helper()
	for _, subject := range subjects {
		pattern := path.Base(subject)
		matched, err := path.Match(pattern, name)
		if err != nil {
			t.Fatalf("path.Match(%q, %q) error = %v", pattern, name, err)
		}
		if matched {
			return true
		}
	}
	return false
}

func read(t *testing.T, file string) string {
	t.Helper()
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", file, err)
	}
	return string(content)
}
