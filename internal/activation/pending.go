package activation

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
	"github.com/SupermodularAI/agents-wake/internal/atomicfile"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/lockfile"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// pendingFileName is the carry across scans: the subagent runs one scan
// anchored and could not resolve, and the children it derived and could not
// parent. It lives beside the spool because it is the same kind of state —
// derived, non-precious, and rebuilt from the harness history plus this file —
// with one difference the file exists for: the spool's contents re-derive from
// any re-scan, while a pending run's entries may never be re-read once the
// recorded boundary moves past them.
//
// The content is bounded ids, hashes, timestamps, enums and complete records —
// the allowlist the spool already holds (ADR-0007), so the local data boundary
// does not widen.
const (
	pendingFileName = "pending.json"
	pendingLockName = "pending.lock"
)

// pendingState is the file's content. Version stamps the shape so a future
// change can tell what it is reading; there is no migration — a version this
// build does not read is dropped, which costs the loss this file exists to
// prevent and is therefore reported rather than silently ignored.
type pendingState struct {
	Version  int                             `json:"version"`
	Runs     []claudecode.PendingSubagentRun `json:"runs"`
	Children []claudecode.PendingChild       `json:"children"`
}

const pendingVersion = 1

// loadPending reads the carry the last scan left. A missing file is the clean
// zero every first scan starts from. A file that cannot be read or parsed is
// the same zero: the scan still runs and re-anchors what the harness history
// still offers, and the loss is exactly the one this file exists to bound —
// never wider than what one interrupted carry held, and never an error that
// breaks a command (plan §4.3).
func loadPending(paths config.Paths) pendingState {
	raw, err := readPendingFile(paths)
	if err != nil {
		return pendingState{Version: pendingVersion}
	}
	var state pendingState
	if err := json.Unmarshal(raw, &state); err != nil {
		return pendingState{Version: pendingVersion}
	}
	if state.Version != pendingVersion {
		// A version this build does not read is dropped rather than misread:
		// the next scan re-derives what the history still offers, and the file
		// is republished in this build's version at the end of it.
		return pendingState{Version: pendingVersion}
	}
	return state
}

// storePending publishes what this scan left unresolved. The write is a
// read-merge-write under the carry's own lock, because two scans can race —
// a hook-fired one and a `wake ingest` — and the loser dropping the winner's
// unresolved runs would re-open the loss this file closes. The merge is the
// same min-fold the scan applies, so two concurrent carries of one run
// converge on the value one scan over the union would have produced.
func storePending(paths config.Paths, state pendingState) error {
	return lockfile.WithLock(filepath.Join(paths.DataDir, pendingLockName), func() error {
		merged := mergePending(loadPending(paths), state)
		merged.Version = pendingVersion
		raw, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		return atomicfile.Publish(filepath.Join(paths.DataDir, pendingFileName), raw, 0o600)
	})
}

// mergePending folds two carries into one. Runs are keyed by agent id and
// children by event id — the same identities the scan derives them from — so a
// run or child both scans carried appears once, and the fold between two
// versions of one run is the scan's own min order.
func mergePending(a, b pendingState) pendingState {
	runs := map[record.Identifier]claudecode.PendingSubagentRun{}
	for _, run := range a.Runs {
		runs[run.AgentID] = run
	}
	for _, run := range b.Runs {
		if held, seen := runs[run.AgentID]; seen {
			runs[run.AgentID] = mergePendingRun(held, run)
		} else {
			runs[run.AgentID] = run
		}
	}
	children := map[record.Hash]claudecode.PendingChild{}
	for _, child := range a.Children {
		children[child.Event.EventID] = child
	}
	for _, child := range b.Children {
		children[child.Event.EventID] = child
	}
	merged := pendingState{Version: pendingVersion}
	for _, run := range runs {
		merged.Runs = append(merged.Runs, run)
	}
	for _, child := range children {
		merged.Children = append(merged.Children, child)
	}
	return merged
}

// mergePendingRun folds two carries of one run: the anchor takes the earlier
// (timestamp, uuid), the declaration takes the earlier (timestamp, uuid), and
// every other dimension follows its fold — the same comparisons the scan's own
// min-folds apply, so the merge is what one scan over both carries would have
// produced.
func mergePendingRun(a, b claudecode.PendingSubagentRun) claudecode.PendingSubagentRun {
	merged := a
	if b.UUID != "" && (a.UUID == "" || b.Timestamp.Before(a.Timestamp) ||
		(b.Timestamp.Equal(a.Timestamp) && b.UUID < a.UUID)) {
		merged.UUID, merged.Timestamp = b.UUID, b.Timestamp
		merged.SessionID, merged.Repo = b.SessionID, b.Repo
		merged.Entrypoint, merged.Version = b.Entrypoint, b.Version
	}
	if b.Name != "" && (a.Name == "" || b.DeclTime.Before(a.DeclTime) ||
		(b.DeclTime.Equal(a.DeclTime) && b.DeclUUID < a.DeclUUID)) {
		merged.Name, merged.Model = b.Name, b.Model
		merged.DeclUUID, merged.DeclTime = b.DeclUUID, b.DeclTime
	}
	return merged
}

// readPendingFile reads the carry's bytes. It is the unlocked half of the
// load: atomicfile.Publish guarantees a reader sees one whole version or
// another, never a half-written file, so the lock is the writer's concern.
func readPendingFile(paths config.Paths) ([]byte, error) {
	return os.ReadFile(filepath.Join(paths.DataDir, pendingFileName))
}
