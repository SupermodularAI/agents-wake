package activation

import (
	"github.com/SupermodularAI/agents-wake/internal/adapter"
	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

// selected reports which harnesses this scan may read.
//
// The rule, stated once here and asserted by test: a scan reads a harness iff
// scan.harnesses names it AND that harness's own store is present on this machine.
// The key filters; presence detects. The key is never a consent mechanism —
// consent stays decided by the project table (ADR-0014, ADR-0025) — and a harness
// excluded by either test was not observed, so every surface renders it "not
// observed" and never "0" (ADR-0046).
//
// A config this build cannot read selects the key's default rather than nothing.
// The alternative is that an unparseable config silently stops all collection,
// which is the failure doctor exists to make impossible.
//
// A spelling that is not a harness this build reads is ignored rather than
// refused: the key is a list of names and validating a list's members is the
// consumer's job, not the config surface's. Ignoring is also the honest answer —
// a build with no reader for that name cannot look there whatever the key says,
// and pretending otherwise would make "not observed" a claim about a place nobody
// could ever have looked.
func selected(paths config.Paths) map[record.Identifier]bool {
	names, err := readHarnessKey(paths)
	if err != nil {
		names = defaultHarnesses()
	}
	enabled := map[record.Identifier]bool{}
	for _, name := range names {
		harness, err := record.BoundedIdentifier(name)
		if err != nil || !record.ValidHarness(harness) {
			continue
		}
		enabled[harness] = true
	}
	return enabled
}

// readHarnessKey resolves the configured list, or reports that it could not.
func readHarnessKey(paths config.Paths) ([]string, error) {
	settings, err := config.Load(paths)
	if err != nil {
		return nil, err
	}
	return settings.StringList("scan.harnesses")
}

// defaultHarnesses is what a scan that could not read the key collects from:
// every harness this build has a reader for. It is taken from the registry rather
// than from the key's default string, so the fallback cannot name a harness this
// binary does not link.
func defaultHarnesses() []string {
	harnesses := adapter.Harnesses()
	names := make([]string, 0, len(harnesses))
	for _, harness := range harnesses {
		names = append(names, string(harness))
	}
	return names
}
