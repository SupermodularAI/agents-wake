//go:build remote

package remote

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestStatusReportsPresenceNotTheEndpoint is ADR-0028's never-echo rule applied
// to the one struct that exists to be printed. ADR-0018 § Command surface listed
// "endpoint" among what `remote status` reports; ADR-0028 is later and narrower,
// and the narrower constraint governs — presence answers ADR-0012's "doctor
// states whether an endpoint is configured" without echoing the value.
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
// TestExportedTypesCarryNoPathOrLabelField, which is untagged and so cannot see
// this type at all. Equality rather than containment: a field added later has to
// be justified here, which is the only point at which "and here is why, as a
// string" gets stopped.
func TestStatusFieldsAreExactly(t *testing.T) {
	want := []string{"EndpointConfigured", "Enabled", "LastFlush", "DeliveredThrough", "Pending"}
	forbidden := []string{"path", "root", "label", "dir", "cwd", "credential", "url", "host"}

	statusType := reflect.TypeOf(Status{})
	got := make([]string, 0, statusType.NumField())
	for i := range statusType.NumField() {
		field := statusType.Field(i)
		got = append(got, field.Name)

		lowered := strings.ToLower(field.Name)
		for _, token := range forbidden {
			if strings.Contains(lowered, token) {
				t.Errorf("Status.%s names %q: doctor output is what people paste into issues", field.Name, token)
			}
		}
		if strings.Contains(lowered, "endpoint") && field.Name != "EndpointConfigured" {
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
