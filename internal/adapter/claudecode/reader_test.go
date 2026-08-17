package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/SupermodularAI/agents-wake/internal/record"
)

const repo = record.Hash("0123456789abcdef0123456789abcdef")

// hostileValues is the Claude Code adapter's hostile-payload corpus. ADR-0007
// requires one per adapter: each adapter is a new input shape, and the privacy
// guarantee has to hold for every one of them.
var hostileValues = []string{
	"/usr/local/bin", "usr/local/bin", "../secrets", "./relative", "~/.ssh/id_rsa",
	`C:\Windows\System32`, "C:temp", `\\server\share`, "contains space", "tab\there",
}

func TestReadDerivesOnlyTerminalRecords(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"1.0.0","attributionMcpServer":"plugin:atlassian:cloud","attributionMcpTool":"search","attributionSkill":"jira-work","message":{"model":"sonnet","content":[{"type":"tool_use","id":"call-1","name":"mcp__atlassian__search"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Pending != 0 || result.Malformed != 0 {
		t.Fatalf("Read() = %+v", result)
	}
	event := result.Records[0]
	if event.Kind != record.KindMCPTool || event.Package != "atlassian" || event.ViaSkill != "jira-work" || event.Outcome == nil || *event.Outcome != record.OutcomeOK {
		t.Fatalf("record = %+v", event)
	}
}

func TestReadDoesNotEmitUnterminatedCall(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill"}]}}`
	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 || result.Pending != 1 {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadUsesSkillNameInsteadOfToolName(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":"pr-review","args":"never retain this prose"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Records[0].Name != "pr-review" || result.Records[0].Kind != record.KindSkill {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadDerivesTerminalAttributedSkill(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionSkill":"run-sdlc","message":{"model":"sonnet","stop_reason":"end_turn"}}`
	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
	event := result.Records[0]
	if event.Kind != record.KindSkill || event.Name != "run-sdlc" || event.Invoker != record.InvokerUser || event.Outcome != nil {
		t.Fatalf("record = %+v", event)
	}
}

func TestReadDerivesTerminalAttributedSubagent(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionAgent":"sdlc-check-architecture","message":{"model":"sonnet","stop_reason":"end_turn"}}`
	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
	event := result.Records[0]
	if event.Kind != record.KindSubagent || event.Name != "sdlc-check-architecture" || event.Invoker != record.InvokerModel || event.Outcome != nil {
		t.Fatalf("record = %+v", event)
	}
}

func TestReadRecordsAttributingAgentForPrimitiveCalls(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionAgent":"sdlc-implement","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":"commit-message"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Records[0].ViaAgent != "sdlc-implement" {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadDoesNotEmitUnfinishedAttributedRun(t *testing.T) {
	input := `{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","attributionAgent":"sdlc-check-architecture","message":{"model":"sonnet","stop_reason":"tool_use"}}`
	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
}

func TestReadSkipsUnconsentedRepository(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/outside","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/outside","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), func(string) (record.Hash, bool) { return "", false }, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 0 {
		t.Fatalf("Read() emitted records for an unconsented repository: %+v", result.Records)
	}
}

func TestReadKeepsUnknownOutcomeNull(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1"}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 || result.Records[0].Outcome != nil {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadSkipsMalformedLine(t *testing.T) {
	input := strings.Join([]string{
		`not json`,
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Task"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":true}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if result.Malformed != 1 || len(result.Records) != 1 || result.Records[0].Kind != record.KindSubagent {
		t.Fatalf("Read() = %+v", result)
	}
}

func TestReadCountsAnOversizedLineWithoutRetainingIt(t *testing.T) {
	oversized := `{"uuid":"entry-x","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-x","name":"Bash"}]},"pad":"swordfish` + strings.Repeat("A", 1024*1024) + `"}`
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
		oversized,
		`{"uuid":"entry-3","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:02Z","message":{"content":[{"type":"tool_use","id":"call-2","name":"Task"}]}}`,
		`{"uuid":"entry-4","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-2","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("Read() records = %d, want a record on each side of the oversized line: %+v", len(result.Records), result)
	}
	if result.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1", result.Malformed)
	}
	if result.Pending != 0 {
		t.Errorf("Pending = %d, want 0", result.Pending)
	}
	for _, event := range result.Records {
		encoded, marshalErr := record.Marshal(event)
		if marshalErr != nil {
			t.Fatalf("Marshal() error = %v", marshalErr)
		}
		for _, fragment := range []string{"swordfish", "entry-x"} {
			if strings.Contains(string(encoded), fragment) {
				t.Fatalf("record retains %q from the oversized line: %s", fragment, encoded)
			}
		}
	}
}

func TestReadNeverUsesToolArguments(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"args":"do not retain this secret"}}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	encoded, err := record.Marshal(result.Records[0])
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), "do not retain") {
		t.Fatalf("record contains source argument: %s", encoded)
	}
}

func resolver(cwd string) (record.Hash, bool) {
	return repo, cwd == "/repo"
}

// names keys the scope digest for this package's tests. Production keys it with a
// subkey of the per-machine salt (config.Repos.NameKey).
var names = record.NewNamer([]byte("test scope key"))

// quoted encodes value as a JSON string so a hostile value can be embedded in a
// transcript line without hand-escaping it.
func quoted(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal(%q) error = %v", value, err)
	}
	return string(encoded)
}

// skillCallTranscript is a terminated Skill call naming skill as its primitive.
func skillCallTranscript(t *testing.T, skill string) string {
	t.Helper()
	return strings.Join([]string{
		fmt.Sprintf(`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","message":{"content":[{"type":"tool_use","id":"call-1","name":"Skill","input":{"skill":%s}}]}}`, quoted(t, skill)),
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")
}

func TestReadDropsCallsWhosePrimitiveNameIsPathShaped(t *testing.T) {
	for _, value := range hostileValues {
		result, err := Read(strings.NewReader(skillCallTranscript(t, value)), resolver, names)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if len(result.Records) != 0 || result.Pending != 0 {
			t.Errorf("Read(skill=%q) = %+v", value, result)
		}
	}
}

func TestReadDropsAttributedRunWithPathShapedAttribution(t *testing.T) {
	for _, field := range []string{"attributionSkill", "attributionAgent"} {
		for _, value := range hostileValues {
			input := fmt.Sprintf(`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z",%q:%s,"message":{"model":"sonnet","stop_reason":"end_turn"}}`, field, quoted(t, value))
			result, err := Read(strings.NewReader(input), resolver, names)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if len(result.Records) != 0 {
				t.Errorf("Read(%s=%q) records = %+v", field, value, result.Records)
			}
		}
	}
}

func TestReadOmitsUnsafeOptionalFieldsAndKeepsTheEvent(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"../1.0.0","attributionSkill":"/etc/passwd","attributionAgent":"../x","attributionMcpServer":"plugin:../evil:tool","message":{"model":"C:\\Windows","content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
	event := result.Records[0]
	if event.ViaSkill != "" || event.ViaAgent != "" || event.Package != "" || event.HarnessVersion != "" || event.Model != "" {
		t.Fatalf("record retained an unsafe optional field: %+v", event)
	}
	if err := record.Validate(event); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestReadDerivesADirectoryScopedSkillReference(t *testing.T) {
	result, err := Read(strings.NewReader(skillCallTranscript(t, "apps/web:deploy")), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
	event := result.Records[0]
	name := string(event.Name)
	if !strings.HasPrefix(name, "scope-") || !strings.HasSuffix(name, ":deploy") {
		t.Fatalf("record name = %q, want scope-<digest>:deploy", name)
	}
	if strings.Contains(name, "apps") || strings.Contains(name, "web") {
		t.Fatalf("record name retains a path fragment: %q", name)
	}
	if err := record.Validate(event); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestReadPreservesRealClaudeCodeIdentityFormats(t *testing.T) {
	input := strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"2.1.3","attributionMcpServer":"plugin:atlassian:cloud","attributionSkill":"pr-review","attributionAgent":"sdlc-implement","message":{"model":"claude-sonnet-4-5-20250929","content":[{"type":"tool_use","id":"call-1","name":"mcp__atlassian__search"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n")

	result, err := Read(strings.NewReader(input), resolver, names)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Read() records = %+v", result.Records)
	}
	event := result.Records[0]
	if event.Package != "atlassian" {
		t.Errorf("Package = %q", event.Package)
	}
	if event.ViaSkill != "pr-review" {
		t.Errorf("ViaSkill = %q", event.ViaSkill)
	}
	if event.ViaAgent != "sdlc-implement" {
		t.Errorf("ViaAgent = %q", event.ViaAgent)
	}
	if event.HarnessVersion != "2.1.3" {
		t.Errorf("HarnessVersion = %q", event.HarnessVersion)
	}
	if event.Model != "claude-sonnet-4-5-20250929" {
		t.Errorf("Model = %q", event.Model)
	}
	if event.Name != "mcp__atlassian__search" {
		t.Errorf("Name = %q", event.Name)
	}
}

func TestMarshalledRecordsCarryNoSeparator(t *testing.T) {
	inputs := []string{skillCallTranscript(t, "apps/web:deploy")}
	for _, value := range hostileValues {
		inputs = append(inputs, skillCallTranscript(t, value))
	}
	inputs = append(inputs, strings.Join([]string{
		`{"uuid":"entry-1","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:00Z","version":"../1.0.0","attributionSkill":"/etc/passwd","attributionAgent":"../secrets","attributionMcpServer":"plugin:../evil:tool","message":{"model":"~/.ssh/id_rsa","content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`,
		`{"uuid":"entry-2","sessionId":"session-1","cwd":"/repo","timestamp":"2026-08-13T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`,
	}, "\n"))

	for _, input := range inputs {
		result, err := Read(strings.NewReader(input), resolver, names)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		for _, event := range result.Records {
			encoded, err := record.Marshal(event)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			for _, fragment := range []string{"/", `\`, "etc", "Windows", "secrets", ".ssh"} {
				if strings.Contains(string(encoded), fragment) {
					t.Fatalf("record contains %q: %s", fragment, encoded)
				}
			}
		}
	}
}
