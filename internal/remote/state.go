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
// It carries endpoint and credential *presence* and never either value, and no
// path or label. ADR-0018 § Command surface listed "endpoint" among what
// `remote status` reports and ADR-0028 said never to echo what was read;
// ADR-0029 settled the two by consumer rather than by seniority. This struct is
// the one `doctor` renders, and `doctor` output is what people paste into
// issues, so it answers presence — which is also exactly what ADR-0030 asks
// `doctor` for. The host that the person who configured it asked to see is
// rendered by internal/cli from config.RemoteEndpointHost, which is a separate
// entry point precisely so this struct cannot grow the value by accident.
//
// Every field is a bool, a count, or a time. There is no free-text field and no
// field that could hold one, which is health.Report's rule for the same reason:
// the temptation a later change will feel is to add "and here is why" as a
// string. TestStatusFieldsAreExactly asserts both the field list and the types.
type Status struct {
	// EndpointConfigured is whether a destination is set, and nothing about
	// what it is.
	EndpointConfigured bool
	// CredentialConfigured is whether anything authorises the delivery —
	// either the store or EnvRemoteAuthorization — and nothing about what.
	//
	// It is here because it is the third of the three conditions Flush gates
	// on, and a caller that can see only the other two renders an endpoint
	// that is on with no credential as a healthy configuration while every
	// flush delivers nothing (plan §12: "collects nothing" is not "collects
	// zero"). Presence and never more: a credential has no bare-host
	// analogue, because every byte of it is the secret (ADR-0029).
	CredentialConfigured bool
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

	// Both secrets are mapped to a bool here and the values are discarded in
	// the same expression. Nothing below this line holds either. The credential
	// is read through LoadRemoteAuth, so the environment override counts as
	// configured: what is reported is what would authorise a delivery, not what
	// a file happens to hold (ADR-0028).
	return Status{
		EndpointConfigured:   auth.Endpoint != "",
		CredentialConfigured: auth.Credential != "",
		Enabled:              auth.Enabled,
		LastFlush:            state.LastFlush,
		DeliveredThrough:     delivered,
		Pending:              head - delivered,
	}, nil
}
