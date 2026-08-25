package cli

import (
	"fmt"
	"io"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// diagnosisSections and afterScan are the two seams a feature adds a `doctor`
// section or a post-scan hook through.
//
// A file appends to them from its own init(), exactly as a subcommand appends to
// commands in registry.go and a setting appends to keys in
// internal/config/registry.go, and for the same mechanical reason: the
// alternative is editing doctor.go and ingest.go — two shared files — to add one
// feature, so two lanes adding unrelated hooks in parallel would conflict on the
// same line for no design reason.
//
// Access is unsynchronised by design, for the reason internal/config/registry.go
// gives: init() runs before any goroutine of ours, and nothing appends after it.

// diagnosisSections are extra `key: value` lines doctor prints after its own.
// Each returns whole lines, already formatted, and prints nothing itself: the
// print loop stays in one place so one writer error is handled once.
var diagnosisSections []func(config.Paths) []string

// afterScan runs after a scan has finished, on both the interactive and the
// hook-invoked path. A hook returns nothing on purpose — the hook-invoked scan
// exits 0 in silence whatever happened (ADR-0016), so there is no error channel
// for one to introduce an exit code through.
var afterScan []func(config.Paths)

// writeDiagnosisSections prints every registered section, in registration order.
func writeDiagnosisSections(out io.Writer, paths config.Paths) error {
	for _, section := range diagnosisSections {
		for _, line := range section(paths) {
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// runAfterScan runs every registered post-scan hook, in registration order.
func runAfterScan(paths config.Paths) {
	for _, hook := range afterScan {
		hook(paths)
	}
}
