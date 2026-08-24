//go:build remote

package remote

import (
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/store"
)

// Status is what `remote status` and `doctor` both render. It is a struct and
// not a renderer: one aggregation, several renderers over the same value, and no
// formatting decision in this package (ADR-0011).
//
// It carries endpoint *presence* and never the endpoint itself, and no
// credential, path, or label. ADR-0018 § Command surface listed "endpoint" among
// what `remote status` reports; ADR-0028 is later and narrower — "never echo
// what was read" — and the narrower constraint governs. Presence still answers
// ADR-0012's requirement that `doctor` state whether an endpoint is configured,
// which is the question a bug report needs answered, and `doctor` output is what
// people paste into issues.
//
// Every field is a bool, a count, or a time. There is no free-text field and no
// field that could hold one, which is health.Report's rule for the same reason:
// the temptation a later change will feel is to add "and here is why" as a
// string. TestStatusFieldsAreExactly asserts both the field list and the types.
type Status struct {
	// EndpointConfigured is whether a destination is set, and nothing about
	// what it is.
	EndpointConfigured bool
	// Enabled is whether delivery happens at all. False with an endpoint
	// configured is a legitimate state — it is how delivery is turned off
	// without discarding where it was going.
	Enabled bool
	// LastFlush is when a flush was last attempted, successful or not. It is
	// never a cursor: see deliveryState.
	LastFlush time.Time
	// DeliveredThrough is the store position the receiver has accepted
	// everything up to.
	DeliveredThrough uint64
	// Pending is how many spooled records are still to go.
	Pending uint64
}

// Describe reports the delivery state without touching the network.
//
// It applies the same self-heal view Flush does: a watermark past head means the
// spool was rebuilt under it, so delivered-through reads as 0 and everything
// counts as pending. Reporting the stale position instead would claim delivery
// of records the spool no longer holds, and the pending subtraction would
// underflow into an enormous number — a uint64 does not go negative, it wraps,
// which is the worst available way for this to be wrong.
func Describe(p config.Paths) (Status, error) {
	auth, err := config.LoadRemoteAuth(p)
	if err != nil {
		return Status{}, err
	}

	head, err := store.New(eventsPath(p)).Head()
	if err != nil {
		return Status{}, err
	}

	state := readDeliveryState(deliveryStatePath(p))
	delivered := state.Position
	if delivered > head {
		delivered = 0
	}

	// The endpoint is mapped to a bool here and the value is discarded in the
	// same expression. Nothing below this line holds it.
	return Status{
		EndpointConfigured: auth.Endpoint != "",
		Enabled:            auth.Enabled,
		LastFlush:          state.LastFlush,
		DeliveredThrough:   delivered,
		Pending:            head - delivered,
	}, nil
}
