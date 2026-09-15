package adapter_test

import (
	"slices"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/adapter/claudecode"
	"github.com/SupermodularAI/agents-wake/internal/adapter/opencode"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// This test is in adapter_test rather than adapter because it imports both
// readers, and both readers import adapter. It is also the only place both are
// linked at once, which is what makes the registry assertion below meaningful.

func TestBothReadersSatisfyScan(t *testing.T) {
	var (
		_ adapter.Scan = (*claudecode.Scan)(nil)
		_ adapter.Scan = (*opencode.Scan)(nil)
	)
	scans := []adapter.Scan{
		claudecode.NewScan(nil, record.Namer{}, claudecode.Installed{}, adapter.Staleness{}, adapter.Idleness{}),
		opencode.NewScan(nil, record.Namer{}, opencode.NewServers(nil), adapter.Staleness{}, adapter.Idleness{}),
	}
	for _, scan := range scans {
		if scan.Harness() == "" {
			t.Errorf("a reader's Scan names no harness")
		}
		if scan.Buffered() != 0 {
			t.Errorf("a fresh scan of %s reports %d buffered", scan.Harness(), scan.Buffered())
		}
	}
}

func TestHarnessesNamesEveryLinkedReader(t *testing.T) {
	// Derived from what this build actually links, never from a list somebody
	// wrote down: a build with no opencode reader cannot name opencode at all.
	got := adapter.Harnesses()
	want := []record.Identifier{claudecode.Harness(), opencode.Harness()}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Harnesses() = %v, want %v", got, want)
	}
}

func TestRegisterIsIdempotentPerHarness(t *testing.T) {
	before := len(adapter.Harnesses())
	adapter.Register(claudecode.Harness())
	adapter.Register(claudecode.Harness())
	if after := len(adapter.Harnesses()); after != before {
		t.Fatalf("harness count = %d after re-registering, want %d", after, before)
	}
}

func TestHarnessesIsACopy(t *testing.T) {
	got := adapter.Harnesses()
	if len(got) == 0 {
		t.Fatal("no reader is registered")
	}
	got[0] = "tampered"
	if adapter.Harnesses()[0] == "tampered" {
		t.Fatal("a caller edited what the build declares")
	}
}

func TestStalenessAndIdlenessZeroValuesAreDisabled(t *testing.T) {
	// The zero value is what a caller that cannot read its threshold must use: both
	// records are permanent once written, so a scan that cannot read its own
	// threshold defers rather than guesses.
	if (adapter.Staleness{}).Enabled() {
		t.Error("the zero Staleness is enabled")
	}
	if (adapter.Idleness{}).Enabled() {
		t.Error("the zero Idleness is enabled")
	}
	now := time.Now().UTC()
	if (adapter.Staleness{Timeout: time.Hour}).Enabled() {
		t.Error("a Staleness with no clock is enabled: every session would look infinitely idle")
	}
	if (adapter.Idleness{Now: now}).Enabled() {
		t.Error("an Idleness with no timeout is enabled: every session would finish on sight")
	}
	if !(adapter.Staleness{Timeout: time.Hour, Now: now}).Enabled() {
		t.Error("a complete Staleness is disabled")
	}
	if !(adapter.Idleness{Timeout: time.Hour, Now: now}).Enabled() {
		t.Error("a complete Idleness is disabled")
	}
}
