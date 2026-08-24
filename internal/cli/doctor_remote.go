//go:build remote

package cli

import (
	"fmt"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
	"github.com/SupermodularAI/agents-wake/internal/remote"
)

func init() { diagnosisSections = append(diagnosisSections, remoteDiagnosis) }

// remoteDiagnosis is doctor's delivery section: presence, a state word, a
// timestamp and two counts.
//
// It renders remote.Describe's Status and derives nothing of its own — the
// watermark arithmetic and the pending subtraction live in internal/remote, and
// a second path to the same numbers is a second answer waiting to disagree
// (ADR-0011, ADR-0001).
//
// Presence only, on every line. doctor output is what people paste into issues
// (ADR-0019 §7), so no endpoint, no scheme, no host, no path and no credential —
// not even a substring of one. ADR-0029 carves the bare host out of ADR-0028's
// never-echo rule for exactly one consumer, `wake remote status`, which reads it
// from config.RemoteEndpointHost. That call is deliberately absent here.
//
// A Describe that failed renders one state word and nothing else. The section
// signature has no error channel, and a row of zeros would report a delivery
// path nobody could read as a healthy one that has sent nothing — the
// "collects nothing" / "collects zero" distinction doctor exists to keep
// (ADR-0010).
func remoteDiagnosis(paths config.Paths) []string {
	status, err := remote.Describe(paths)
	if err != nil {
		return []string{"remote delivery: unreadable"}
	}

	endpoint := "not configured"
	if status.EndpointConfigured {
		endpoint = "configured"
	}
	credential := "not configured"
	if status.CredentialConfigured {
		credential = "set"
	}
	state := "off"
	if status.Enabled {
		state = "on"
	}
	lastFlush := "never"
	if !status.LastFlush.IsZero() {
		lastFlush = status.LastFlush.UTC().Format(time.RFC3339)
	}

	return []string{
		fmt.Sprintf("remote endpoint: %s", endpoint),
		fmt.Sprintf("remote credential: %s", credential),
		fmt.Sprintf("remote delivery: %s", state),
		fmt.Sprintf("remote last flush: %s", lastFlush),
		fmt.Sprintf("remote delivered through: %d", status.DeliveredThrough),
		fmt.Sprintf("remote pending: %d", status.Pending),
	}
}
