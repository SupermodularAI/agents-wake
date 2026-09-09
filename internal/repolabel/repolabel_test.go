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

// DisplayAll's three cases. A row spanning several projects renders how many rather
// than naming one of them: naming one would report it as the only one, which is the
// misreport a per-repository row grain used to make visible.
func TestDisplayAllNamesOneProjectAndCountsSeveral(t *testing.T) {
	labels := repolabel.Labels{string(first): "agents-wake"}
	third := record.Hash("00112233445566778899aabbccddeeff")
	fourth := record.Hash("ffeeddccbbaa99887766554433221100")
	for _, testCase := range []struct {
		name  string
		repos []record.Hash
		want  string
	}{
		{name: "none", repos: nil, want: "-"},
		{name: "one labelled", repos: []record.Hash{first}, want: "agents-wake"},
		{name: "one unlabelled", repos: []record.Hash{second}, want: "repo-fedcba987654"},
		{name: "four", repos: []record.Hash{first, second, third, fourth}, want: "4-projects"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := labels.DisplayAll(testCase.repos); got != testCase.want {
				t.Fatalf("DisplayAll(%v) = %q, want %q", testCase.repos, got, testCase.want)
			}
		})
	}
}

// The multi-project cell holds no whitespace. internal/ingest/claudecode_test.go's
// reportedCalls reads the CALLS column by field index out of a whitespace-split row
// and documents that precondition; a cell of "4 projects" would shift the index.
func TestDisplayAllKeepsTheProjectCellASingleToken(t *testing.T) {
	got := repolabel.Labels{string(first): "agents-wake"}.DisplayAll([]record.Hash{first, second})
	if fields := strings.Fields(got); len(fields) != 1 {
		t.Fatalf("DisplayAll() = %q, want a single whitespace-free token", got)
	}
}
