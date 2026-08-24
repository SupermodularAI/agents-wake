//go:build remote

package cli

import (
	"os"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/detach"
)

func init() { afterScan = append(afterScan, spawnFlush) }

// flushChild is what the post-scan hook spawns. A variable for the reason
// hookChild in ingest.go is one: a test that let this run would start a detached
// background copy of the test binary.
var flushChild = detach.Start

// spawnFlush starts `wake remote flush` as a detached child and returns.
//
// Detached and never waited on, so no command waits on the network: ADR-0018
// requires a dead endpoint to be indistinguishable from an absent one from the
// user's point of view, and a scan that blocked on a far end would be the one
// observable difference. The child re-execs this same binary, so it is the
// tagged build by construction.
//
// Nothing is gated here. Single-flight is remote-flush.lock inside
// remote.FlushReport, and remote.min_interval is enforced beside it
// (internal/remote/deliver.go); re-deciding either above the seam would be a
// second gate that can disagree with the first. Every condition a flush needs —
// endpoint, credential, enabled — is likewise checked by the child.
//
// Failures go to discard, the one named sink on this path. The hook-invoked scan
// exits 0 in silence whatever happened (ADR-0016), and because every id is
// derived from its source event (ADR-0004), a spawn that was lost costs nothing
// the next scan's flush cannot recover.
//
// The paths argument is unused: what this hook needs is the path to this
// executable, and the child resolves everything else for itself. The parameter
// is the seam's shape, shared with the diagnosis sections, not a requirement of
// this hook.
func spawnFlush(_ config.Paths) {
	self, err := os.Executable()
	if err != nil {
		discard(err)
		return
	}
	discard(flushChild([]string{self, "remote", "flush"}))
}
