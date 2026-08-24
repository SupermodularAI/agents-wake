package cli

import (
	"fmt"
	"io"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// diagnosisSections and afterScan are the two seams a build-tagged file reaches
// the default build through.
//
// Both are empty here and stay empty in the default binary. A file compiled only
// under //go:build remote appends to them from its own init(), exactly as a
// subcommand appends to commands in registry.go and a setting appends to keys in
// internal/config/registry.go. An empty slice of plain funcs is not network code
// and names no remote symbol, so the claim ADR-0012 defends — that the default
// binary contains no delivery path at all — is untouched by their existence.
//
// The alternative is what this file exists to avoid: a build-tag conditional
// inside doctor.go and ingest.go, which would put the absence of delivery behind
// something a reader has to verify rather than something they can see.
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
