package platform

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestReleaseAndCIMatricesMatchTheSupportedSet guards the one-set invariant.
//
// The announced platform set lives in three places: this package refuses at
// startup, CI's build job proves every entry compiles, and .goreleaser.yaml
// publishes them. ADR-0021 makes them one set; only a test keeps them one set.
// A platform published but never built, or built but never announced, is the
// failure this catches.
func TestReleaseAndCIMatricesMatchTheSupportedSet(t *testing.T) {
	for _, file := range []string{"../../.github/workflows/ci.yml", "../../.goreleaser.yaml"} {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", file, err)
		}
		if got := declared(t, file, string(content), "goos:"); !slices.Equal(got, OS()) {
			t.Errorf("%s builds goos %v, want %v", file, got, OS())
		}
		if got := declared(t, file, string(content), "goarch:"); !slices.Equal(got, Arch()) {
			t.Errorf("%s builds goarch %v, want %v", file, got, Arch())
		}
	}
}

// declared reads the single flow-sequence value of key from a YAML file, as
// `goos: [darwin, linux]`. It fails when the key appears zero times — the file
// stopped declaring a platform set — or more than once, which is how a second
// matrix would hide from this assertion.
//
// The comparison is case-sensitive on purpose: it is what keeps `GOOS:` env
// lines and `${{ matrix.goos }}` interpolations out of the result.
func declared(t *testing.T, file, content, key string) []string {
	t.Helper()
	var found []string
	matches := 0
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key) {
			continue
		}
		matches++
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, key))
		value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
		found = nil
		for _, entry := range strings.Split(value, ",") {
			found = append(found, strings.TrimSpace(entry))
		}
	}
	if matches != 1 {
		t.Fatalf("%s declares %q %d times, want exactly 1", file, key, matches)
	}
	return found
}
