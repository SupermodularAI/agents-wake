package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/inventory"
)

func TestResolveDiscoveryScopeWithholdsProjectDiscoveryOutsideAConsentedRepository(t *testing.T) {
	paths, notices, cmd := scopeFixture(t, t.TempDir())

	scope, _, err := resolveDiscoveryScope(cmd, paths)
	if err != nil {
		t.Fatalf("resolveDiscoveryScope() error = %v", err)
	}
	if scope.Project != inventory.ProjectUnconsented || scope.Root != "" {
		t.Fatalf("scope = %+v", scope)
	}
	if !strings.Contains(notices.String(), "not a consented repository") {
		t.Fatalf("notice = %q", notices.String())
	}
}

func TestResolveDiscoveryScopeNoticeNamesNoPath(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "identifiable-project-name")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	paths, notices, cmd := scopeFixture(t, directory)

	if _, _, err := resolveDiscoveryScope(cmd, paths); err != nil {
		t.Fatalf("resolveDiscoveryScope() error = %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	for _, forbidden := range []string{cwd, filepath.Base(cwd), "/"} {
		if strings.Contains(notices.String(), forbidden) {
			t.Fatalf("notice %q names %q", notices.String(), forbidden)
		}
	}
}

func TestResolveDiscoveryScopeGrantsProjectDiscoveryInsideAConsentedRepository(t *testing.T) {
	paths, notices, cmd := scopeFixture(t, t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	repos, err := config.OpenRepos(paths)
	if err != nil {
		t.Fatalf("OpenRepos() error = %v", err)
	}
	if _, registerErr := repos.Register(cwd, filepath.Base(cwd), time.Time{}); registerErr != nil {
		t.Fatalf("Register() error = %v", registerErr)
	}

	scope, _, err := resolveDiscoveryScope(cmd, paths)
	if err != nil {
		t.Fatalf("resolveDiscoveryScope() error = %v", err)
	}
	if scope.Project != inventory.ProjectConsented || scope.Root != cwd {
		t.Fatalf("scope = %+v, cwd = %q", scope, cwd)
	}
	if notices.String() != "" {
		t.Fatalf("consented discovery printed a notice: %q", notices.String())
	}
}

// scopeFixture isolates Wake's data directory and home, moves into directory,
// and returns a command whose stderr is a buffer the test can assert on.
func scopeFixture(t *testing.T, directory string) (config.Paths, *bytes.Buffer, *cobra.Command) {
	t.Helper()
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Chdir(directory)
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths() error = %v", err)
	}
	notices := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(notices)
	return paths, notices, cmd
}
