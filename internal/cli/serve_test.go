package cli

import (
	"net"
	"strings"
	"testing"
)

// Acceptance: a port collision never prints a false active-server message. The
// bind is the last thing that can fail, so it is the one most likely to be
// reported after the fact.
func TestServeReportsABoundPortWithoutAnnouncingTheDashboard(t *testing.T) {
	isolate(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })

	original := dashboardPort
	dashboardPort = occupied.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { dashboardPort = original })

	stdout, stderr, err := runSplit(t, "serve")
	if err == nil {
		t.Fatal("serve on an occupied port returned a nil error, want a failure")
	}
	// The unconsented-project notice on stderr is expected; only the dashboard
	// announcement must be absent.
	for stream, out := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if strings.Contains(out, "Serving dashboard") {
			t.Errorf("%s announced the dashboard despite a failed bind: %s", stream, out)
		}
	}
}
