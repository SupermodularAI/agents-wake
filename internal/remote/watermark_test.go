//go:build remote

package remote

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// testPaths gives each test its own config and data roots. It constructs the
// layout directly rather than going through config.ResolvePaths, which is the
// pattern internal/activation's own testPaths uses: what these tests exercise is
// delivery, and borrowing the resolver would make every one of them depend on
// the process environment as well.
//
// No test in this package may call t.Parallel. They swap deliveryClient, which
// is a package variable, and a parallel test would swap it out from under
// another one — see the comment on deliveryClient itself.
func testPaths(t *testing.T) config.Paths {
	t.Helper()
	paths := config.Paths{ConfigDir: filepath.Join(t.TempDir(), "config"), DataDir: filepath.Join(t.TempDir(), "data")}
	paths.ConfigFile = filepath.Join(paths.ConfigDir, "config.toml")
	paths.SaltFile = filepath.Join(paths.ConfigDir, "salt.bin")
	paths.ProjectsFile = filepath.Join(paths.DataDir, "projects.bin")
	paths.PrimitivesFile = filepath.Join(paths.DataDir, "primitives.json")
	paths.HealthFile = filepath.Join(paths.DataDir, "health.json")
	return paths
}

func TestDeliveryStateRoundTrips(t *testing.T) {
	paths := testPaths(t)
	path := deliveryStatePath(paths)
	flushedAt := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)

	if err := writeDeliveryState(path, deliveryState{Position: 42, LastFlush: flushedAt}); err != nil {
		t.Fatalf("writeDeliveryState() error = %v", err)
	}

	got := readDeliveryState(path)
	if got.Position != 42 {
		t.Errorf("Position = %d, want 42", got.Position)
	}
	if !got.LastFlush.Equal(flushedAt) {
		t.Errorf("LastFlush = %v, want %v", got.LastFlush, flushedAt)
	}
}

// TestDeliveryStateMissingFileIsZero pins the fresh-install case: asking how far
// delivery got must not create a file, and the answer for a machine that has
// never flushed is "through nothing".
func TestDeliveryStateMissingFileIsZero(t *testing.T) {
	paths := testPaths(t)
	path := deliveryStatePath(paths)

	got := readDeliveryState(path)
	if got.Position != 0 || !got.LastFlush.IsZero() {
		t.Errorf("readDeliveryState() = %+v, want the zero value", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(%s) = %v, want ErrNotExist — reading the state created it", deliveryStateFileName, err)
	}
}

// TestDeliveryStateUnreadableResetsToZero asserts the direction every fault
// falls in. Re-sending is free (the receiver deduplicates on a span id derived
// from the deterministic event id), while a cursor that failed *forward* would
// skip records permanently.
func TestDeliveryStateUnreadableResetsToZero(t *testing.T) {
	cases := map[string]string{
		"garbage": "not json at all",
		"wrong version": `{"version": 99, "position": 500, ` +
			`"last_flush": "2026-08-24T12:00:00Z"}`,
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			paths := testPaths(t)
			path := deliveryStatePath(paths)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(path, []byte(contents), deliveryStateFileMode); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			if got := readDeliveryState(path); got.Position != 0 {
				t.Errorf("Position = %d, want 0 — an unreadable cursor must fail toward re-sending", got.Position)
			}
		})
	}
}

func TestDeliveryStateFileMode(t *testing.T) {
	paths := testPaths(t)
	path := deliveryStatePath(paths)

	if err := writeDeliveryState(path, deliveryState{Position: 1}); err != nil {
		t.Fatalf("writeDeliveryState() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != deliveryStateFileMode {
		t.Errorf("mode = %v, want %v", got, deliveryStateFileMode)
	}
}

// TestDeliveryStateStampsItsOwnVersion keeps a caller from being able to write a
// file this build then refuses to read back.
func TestDeliveryStateStampsItsOwnVersion(t *testing.T) {
	paths := testPaths(t)
	path := deliveryStatePath(paths)

	if err := writeDeliveryState(path, deliveryState{Version: 99, Position: 7}); err != nil {
		t.Fatalf("writeDeliveryState() error = %v", err)
	}

	if got := readDeliveryState(path); got.Position != 7 {
		t.Errorf("Position = %d, want 7 — the write did not stamp the current version", got.Position)
	}
}

// TestDeliveryStateLivesUnderTheDataRoot is the ADR-0014 half: the watermark is
// derived state and must die with the spool, so it may not land beside the
// credential store under the config root.
func TestDeliveryStateLivesUnderTheDataRoot(t *testing.T) {
	paths := testPaths(t)
	if got := filepath.Dir(deliveryStatePath(paths)); got != paths.DataDir {
		t.Errorf("delivery state directory = %s, want the data root %s", got, paths.DataDir)
	}
}
