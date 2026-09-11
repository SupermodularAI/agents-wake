package adapter

import (
	"slices"
	"sync"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

// registered is this build's own answer to "which harnesses does this binary
// read". It is guarded because a reader registers from its own init() and a
// caller may ask from anywhere.
var registered struct {
	sync.Mutex
	harnesses []record.Identifier
}

// Register declares that this build has a reader for a harness.
//
// Each reader calls it from its own init(), so a build that does not link a
// reader cannot claim it — which is what lets a renderer say "not observed" about
// a harness this build genuinely cannot see, rather than about a name somebody
// hardcoded into a sentence. It is the same self-registration internal/cli uses
// for subcommands, and for the same second reason: a shared list is a file every
// parallel change conflicts on.
//
// Registering the same harness twice is a no-op rather than an error. Two calls
// mean one reader, and a panic at init() would turn a duplicated line into a
// binary that cannot start.
func Register(harness record.Identifier) {
	registered.Lock()
	defer registered.Unlock()
	if slices.Contains(registered.harnesses, harness) {
		return
	}
	registered.harnesses = append(registered.harnesses, harness)
	slices.Sort(registered.harnesses)
}

// Harnesses is every harness this build reads, sorted, as a copy. Sorted because
// two renderings of one build must not differ, and a copy because a caller must
// not be able to edit what the build declares.
func Harnesses() []record.Identifier {
	registered.Lock()
	defer registered.Unlock()
	return slices.Clone(registered.harnesses)
}
