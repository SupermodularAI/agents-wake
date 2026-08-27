package repolabel_test

import (
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
	"github.com/SupermodularAI/agents-wake/internal/repolabel"
)

const (
	first  = record.Hash("0123456789abcdef0123456789abcdef")
	second = record.Hash("fedcba9876543210fedcba9876543210")
)

func TestDisplayShowsTheRecordedLabel(t *testing.T) {
	labels := repolabel.Labels{string(first): "agents-wake"}
	if got := labels.Display(first); got != "agents-wake" {
		t.Fatalf("Display() = %q, want %q", got, "agents-wake")
	}
}

func TestDisplayFallsBackToAReadableIdWithNoLabelRecorded(t *testing.T) {
	got := repolabel.Labels(nil).Display(first)
	if got != "repo-0123456789ab" {
		t.Fatalf("Display() = %q, want %q", got, "repo-0123456789ab")
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("Display() must never render a blank cell")
	}
}

func TestDisplayRefusesALabelThatIsNotABoundedToken(t *testing.T) {
	for _, raw := range []string{"has space", "tab\there", "\x1b[31mred", "  padded  ", ""} {
		t.Run(strings.ReplaceAll(raw, "\x1b", "ESC"), func(t *testing.T) {
			got := repolabel.Labels{string(first): raw}.Display(first)
			if got != "repo-0123456789ab" {
				t.Fatalf("Display() = %q, want the id fallback", got)
			}
			if raw != "" && strings.Contains(got, raw) {
				t.Fatalf("Display() = %q leaked the refused label %q", got, raw)
			}
		})
	}
}

func TestDisplayNamesNoRepositoryWithoutOne(t *testing.T) {
	if got := repolabel.Labels(nil).Display(""); got != "-" {
		t.Fatalf("Display(\"\") = %q, want %q", got, "-")
	}
}

func TestDisplayIsDistinctPerRepository(t *testing.T) {
	labels := repolabel.Labels(nil)
	if labels.Display(first) == labels.Display(second) {
		t.Fatalf("two repositories rendered identically as %q", labels.Display(first))
	}
}
