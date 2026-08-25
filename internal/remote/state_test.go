package remote

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/config"
)

// TestStatusReportsPresenceNotTheEndpoint is ADR-0029's visibility model applied
// to the one struct that exists to be printed. ADR-0018 § Command surface listed
// "endpoint" among what `remote status` reports and ADR-0028 said never to echo
// what was read; ADR-0029 settled the two by consumer rather than by seniority —
// this struct is what `doctor` renders and people paste into issues, so it
// carries presence, and the host the person who configured it asked to see is
// rendered by internal/cli from config.RemoteEndpointHost instead.
func TestStatusReportsPresenceNotTheEndpoint(t *testing.T) {
	const endpoint = "https://never-echoed.example/v1/traces"
	paths := testPaths(t)
	enable(t, paths, endpoint)

	status, err := Describe(paths)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if !status.EndpointConfigured {
		t.Error("EndpointConfigured = false, want true")
	}
	if !status.Enabled {
		t.Error("Enabled = false, want true")
	}

	rendered := fmt.Sprintf("%+v", status)
	for _, secret := range []string{endpoint, "never-echoed", testCredential, testPublicKey, testSecretKey} {
		if strings.Contains(rendered, secret) {
			t.Errorf("Status renders a value it must never carry: %s", rendered)
		}
	}
}

// TestStatusFieldsAreExactly mirrors internal/config's
// TestExportedTypesCarryNoPathOrLabelField, which lives in another package and
// so cannot see this type at all. Equality rather than containment: a field added later has to
// be justified here, which is the only point at which "and here is why, as a
// string" gets stopped.
func TestStatusFieldsAreExactly(t *testing.T) {
	want := []string{"EndpointConfigured", "CredentialConfigured", "Enabled", "LastFlush", "DeliveredThrough", "Pending"}
	forbidden := []string{"path", "root", "label", "dir", "cwd", "credential", "url", "host"}

	statusType := reflect.TypeOf(Status{})
	got := make([]string, 0, statusType.NumField())
	for i := range statusType.NumField() {
		field := statusType.Field(i)
		got = append(got, field.Name)

		// A name ending in "Configured" reports presence and never the value —
		// the whole of Status's visibility model (ADR-0029). The token scan is
		// about fields that carry a value, so a presence field is checked for
		// being a bool instead: a presence field that is not a bool is a value
		// field wearing the name of one.
		lowered := strings.ToLower(field.Name)
		if strings.HasSuffix(field.Name, "Configured") {
			if field.Type.Kind() != reflect.Bool {
				t.Errorf("Status.%s is %s: a presence field is a bool or it is carrying the value", field.Name, field.Type)
			}
			continue
		}
		for _, token := range forbidden {
			if strings.Contains(lowered, token) {
				t.Errorf("Status.%s names %q: doctor output is what people paste into issues", field.Name, token)
			}
		}
		if strings.Contains(lowered, "endpoint") {
			t.Errorf("Status.%s carries the endpoint itself, not its presence", field.Name)
		}

		// Every field is a bool, a count, or a time. There is no string field
		// and no field that could hold one — the temptation a later change will
		// feel is to add free text, and this is where it fails (ADR-0007
		// applied to diagnostics).
		switch field.Type.Kind() {
		case reflect.Bool, reflect.Uint64:
		default:
			if field.Type != reflect.TypeOf(time.Time{}) {
				t.Errorf("Status.%s is %s, want a bool, a uint64, or a time.Time", field.Name, field.Type)
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("Status fields = %v, want %v", got, want)
	}
}

// TestStatusReportsCredentialPresence is the third of the three conditions a
// flush gates on, made visible.
//
// An endpoint that is configured and on but has no credential delivers nothing,
// and before this field the only two conditions a caller could see were the two
// that were satisfied — so the state rendered as a healthy one and every flush
// reported a clean zero. That is plan §12's "collects nothing" versus "collects
// zero" applied to delivery.
//
// Presence and never the value: every byte of a credential is the secret, so it
// has no bare-host analogue and never rises above a bool (ADR-0028, ADR-0029).
func TestStatusReportsCredentialPresence(t *testing.T) {
	const endpoint = "https://never-echoed.example/v1/traces"

	t.Run("stored", func(t *testing.T) {
		paths := testPaths(t)
		enable(t, paths, endpoint)

		status, err := Describe(paths)
		if err != nil {
			t.Fatalf("Describe() error = %v", err)
		}
		if !status.CredentialConfigured {
			t.Error("CredentialConfigured = false with a credential in the store")
		}
	})

	t.Run("absent", func(t *testing.T) {
		paths := testPaths(t)
		if err := config.SetRemoteAuth(paths, config.RemoteAuth{Endpoint: endpoint, Enabled: true}); err != nil {
			t.Fatalf("SetRemoteAuth() error = %v", err)
		}

		status, err := Describe(paths)
		if err != nil {
			t.Fatalf("Describe() error = %v", err)
		}
		if !status.EndpointConfigured || !status.Enabled {
			t.Fatalf("Describe() = %+v, want an endpoint that is configured and on", status)
		}
		if status.CredentialConfigured {
			t.Error("CredentialConfigured = true with no credential anywhere: this state delivers nothing")
		}
	})

	t.Run("from the environment", func(t *testing.T) {
		paths := testPaths(t)
		if err := config.SetRemoteAuth(paths, config.RemoteAuth{Endpoint: endpoint, Enabled: true}); err != nil {
			t.Fatalf("SetRemoteAuth() error = %v", err)
		}
		t.Setenv(config.EnvRemoteAuthorization, testCredential)

		status, err := Describe(paths)
		if err != nil {
			t.Fatalf("Describe() error = %v", err)
		}
		// The override is what authorises the delivery, so it is what presence
		// has to answer for: a machine deliberately keeping no secret on disk
		// is configured, not broken (ADR-0028).
		if !status.CredentialConfigured {
			t.Error("CredentialConfigured = false with the credential supplied by the environment")
		}
	})
}

func TestStatusPendingCount(t *testing.T) {
	paths := testPaths(t)
	enable(t, paths, "https://never-echoed.example/v1/traces")
	seed(t, paths, 5)
	if err := writeDeliveryState(deliveryStatePath(paths), deliveryState{Position: 3}); err != nil {
		t.Fatalf("writeDeliveryState() error = %v", err)
	}

	status, err := Describe(paths)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if status.DeliveredThrough != 3 {
		t.Errorf("DeliveredThrough = %d, want 3", status.DeliveredThrough)
	}
	if status.Pending != 2 {
		t.Errorf("Pending = %d, want 2", status.Pending)
	}
}

// TestStatusAfterRebuildReportsZeroDelivered applies the same self-heal view
// Flush does. Without it, status would claim delivery of records the spool no
// longer holds and report a negative pending count as a very large one.
func TestStatusAfterRebuildReportsZeroDelivered(t *testing.T) {
	paths := testPaths(t)
	enable(t, paths, "https://never-echoed.example/v1/traces")
	seed(t, paths, 3)
	if err := writeDeliveryState(deliveryStatePath(paths), deliveryState{Position: 99}); err != nil {
		t.Fatalf("writeDeliveryState() error = %v", err)
	}

	status, err := Describe(paths)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if status.DeliveredThrough != 0 {
		t.Errorf("DeliveredThrough = %d, want 0", status.DeliveredThrough)
	}
	if status.Pending != 3 {
		t.Errorf("Pending = %d, want 3", status.Pending)
	}
}

// TestStatusOnAFreshInstall is the case a fresh clone actually hits: nothing
// configured, nothing spooled, nothing flushed. It must answer rather than fail,
// because `doctor` runs on exactly this machine.
func TestStatusOnAFreshInstall(t *testing.T) {
	paths := testPaths(t)

	status, err := Describe(paths)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if status.EndpointConfigured || status.Enabled {
		t.Errorf("Describe() = %+v, want nothing configured", status)
	}
	if !status.LastFlush.IsZero() {
		t.Errorf("LastFlush = %v, want the zero time", status.LastFlush)
	}
	if status.DeliveredThrough != 0 || status.Pending != 0 {
		t.Errorf("DeliveredThrough = %d, Pending = %d, want 0 and 0", status.DeliveredThrough, status.Pending)
	}
}

// TestStatusReportsLastFlush pins the field `remote status` and `doctor` print,
// and the reason the delivery state carries a time at all.
func TestStatusReportsLastFlush(t *testing.T) {
	paths := testPaths(t)
	flushedAt := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	if err := writeDeliveryState(deliveryStatePath(paths), deliveryState{Position: 0, LastFlush: flushedAt}); err != nil {
		t.Fatalf("writeDeliveryState() error = %v", err)
	}

	status, err := Describe(paths)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if !status.LastFlush.Equal(flushedAt) {
		t.Errorf("LastFlush = %v, want %v", status.LastFlush, flushedAt)
	}
}

// TestStatusIsAStructNotARenderer keeps ADR-0011's boundary: `remote status` and
// `doctor` both render this value, so a formatting decision taken here would be
// a formatting decision taken for both.
func TestStatusIsAStructNotARenderer(t *testing.T) {
	statusType := reflect.PointerTo(reflect.TypeOf(Status{}))
	for i := range statusType.NumMethod() {
		t.Errorf("Status has the method %s: rendering belongs to the caller", statusType.Method(i).Name)
	}
}
