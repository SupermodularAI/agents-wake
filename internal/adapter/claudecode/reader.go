// Package claudecode derives safe terminal records from Claude Code JSONL
// transcripts. It retains pending call metadata only until a matching result
// arrives and never persists transcript content.
package claudecode

import (
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/SupermodularAI/agents-wake/internal/jsonl"
	"github.com/SupermodularAI/agents-wake/internal/record"
)

const harness = record.Identifier("claude-code")

// maxLineBytes is the largest transcript line this reader accepts. It stays an
// internal constant, not a config key: ADR-0014 keeps the config surface
// deliberately small, and a limit a user can raise is a limit that stops bounding
// anything.
const maxLineBytes = 1024 * 1024

// Result contains safe derived records plus collection health counters.
type Result struct {
	Records   []record.Record
	Malformed int
	Pending   int
}

// Resolver maps a recorded working directory to a consented repository hash.
// It returns false when the directory was never consented. The reader never
// accesses the filesystem while resolving a transcript entry.
type Resolver func(cwd string) (record.Hash, bool)

// Read streams one Claude Code transcript. Only events accepted by resolve can
// become records, so an adapter scan cannot widen project consent.
//
// names carries the key a directory-scoped reference's scope is digested under. A
// scope is a repository path fragment, so the reader cannot derive a persistable
// name for one on its own (ADR-0020); a Namer with no key drops those references
// rather than persisting an unkeyed digest of a path.
func Read(reader io.Reader, resolve Resolver, names record.Namer) (Result, error) {
	if resolve == nil {
		return Result{}, errors.New("missing repository resolver")
	}

	result := Result{}
	pending := map[string]call{}
	skipped, err := jsonl.Lines(reader, maxLineBytes, func(line []byte) {
		var entry transcriptEntry
		if unmarshalErr := json.Unmarshal(line, &entry); unmarshalErr != nil {
			result.Malformed++
			return
		}
		if !entry.valid() {
			result.Malformed++
			return
		}
		if event, ok := entry.attributedRun(resolve, names); ok {
			result.Records = append(result.Records, event)
		}
		for _, block := range entry.Message.Content {
			switch block.Type {
			case "tool_use":
				if pendingCall, ok := entry.call(block, resolve, names); ok {
					pending[pendingCall.id] = pendingCall
				}
			case "tool_result":
				pendingCall, ok := pending[block.ToolUseID]
				if !ok {
					continue
				}
				delete(pending, block.ToolUseID)
				event, ok := pendingCall.complete(entry, block)
				if ok {
					result.Records = append(result.Records, event)
				}
			}
		}
	})
	if err != nil {
		return Result{}, errors.New("reading Claude Code history")
	}
	// A line too long to deliver is unusable in the same way a line that does not
	// parse is: counted as malformed so doctor can report blindness, and nothing is
	// synthesised from it — no call is opened, so no result can terminate one.
	result.Malformed += skipped
	result.Pending = len(pending)
	return result, nil
}

type transcriptEntry struct {
	UUID                 string     `json:"uuid"`
	SessionID            string     `json:"sessionId"`
	CWD                  string     `json:"cwd"`
	Timestamp            time.Time  `json:"timestamp"`
	Version              string     `json:"version"`
	AttributionMCPServer string     `json:"attributionMcpServer"`
	AttributionMCPTool   string     `json:"attributionMcpTool"`
	AttributionAgent     string     `json:"attributionAgent"`
	AttributionSkill     string     `json:"attributionSkill"`
	ToolDenialKind       string     `json:"toolDenialKind"`
	ToolUseResult        toolResult `json:"toolUseResult"`
	Message              message    `json:"message"`
}

type message struct {
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Content    []contentBlock `json:"content"`
}

type contentBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	IsError   *bool  `json:"is_error"`
	Input     input  `json:"input"`
}

// input names only the allowlisted fields a primitive needs. In particular, it
// does not retain a Skill's free-text args field while decoding the transcript.
type input struct {
	Skill        string `json:"skill"`
	SubagentType string `json:"subagent_type"`
}

type toolResult struct {
	Interrupted bool `json:"interrupted"`
}

// valid bounds the entry id to the opaque-token domain rather than only requiring
// it to be non-empty, because that id is half of every tool call's derived
// identity: a value from the token domain cannot contain callSeparator, which is
// what makes the composition unambiguous (ADR-0004). A transcript entry whose id
// is outside that domain is counted malformed like any other unusable line. Real
// Claude Code ids are RFC 4122 uuids, which the domain admits.
func (entry transcriptEntry) valid() bool {
	if entry.SessionID == "" || entry.Timestamp.IsZero() {
		return false
	}
	_, err := record.BoundedToken(entry.UUID)
	return err == nil
}

// callSeparator delimits the two halves of a tool call's source identity. It is a
// unit separator, which neither half can contain — valid bounds the entry id to
// the token domain and call bounds the block id — so the split is unambiguous: no
// two distinct (entry, block) pairs share a composed identity, and no composed
// identity can equal a bare entry id, which is what attributedRun derives a
// terminal run from.
const callSeparator = "\x1f"

// callSourceEvent identifies one tool_use block by the source event carrying it
// and the block's own id. Both halves come from the transcript: no ordinal of the
// block within its entry, no write time, no randomness — so the same transcript
// re-derives the same ids forever and re-ingestion stays a no-op (ADR-0004).
func callSourceEvent(entryUUID string, blockID record.Identifier) record.Identifier {
	return record.Identifier(entryUUID + callSeparator + string(blockID))
}

type call struct {
	id          string
	eventID     record.Hash
	sessionID   record.Identifier
	timestamp   time.Time
	version     record.Version
	kind        record.Kind
	name        record.Identifier
	packageName record.Identifier
	viaSkill    record.Identifier
	viaAgent    record.Identifier
	model       record.Identifier
	invoker     record.Invoker
	repo        record.Hash
}

func (entry transcriptEntry) call(block contentBlock, resolve Resolver, names record.Namer) (call, bool) {
	id, err := record.BoundedToken(block.ID)
	if err != nil {
		return call{}, false
	}
	name, err := primitiveName(block, names)
	if err != nil {
		return call{}, false
	}
	sessionID, err := record.BoundedToken(entry.SessionID)
	if err != nil {
		return call{}, false
	}
	repo, consented := resolve(entry.CWD)
	if !consented {
		return call{}, false
	}

	derived := call{
		id:        string(id),
		eventID:   record.DeriveEventID(harness, callSourceEvent(entry.UUID, id)),
		sessionID: sessionID,
		timestamp: record.NormalizedTimestamp(entry.Timestamp),
		kind:      kindFor(record.Identifier(block.Name)),
		name:      name,
		invoker:   record.InvokerModel,
		repo:      repo,
	}
	if version, err := record.BoundedVersion(entry.Version); err == nil {
		derived.version = version
	}
	if model, err := record.BoundedIdentifier(entry.Message.Model); err == nil {
		derived.model = model
	}
	if skill, err := names.DerivedName(entry.AttributionSkill); err == nil {
		derived.viaSkill = skill
	}
	if agent, err := names.DerivedName(entry.AttributionAgent); err == nil {
		derived.viaAgent = agent
	}
	if packageName, ok := packageFromAttribution(entry.AttributionMCPServer); ok {
		derived.packageName = packageName
	}
	return derived, true
}

// attributedRun records the terminal completion of a skill or subagent. Claude
// Code puts those identities on every transcript entry, including entries in
// subagent files; the final end_turn entry is the safe completion boundary.
func (entry transcriptEntry) attributedRun(resolve Resolver, names record.Namer) (record.Record, bool) {
	if entry.Message.StopReason != "end_turn" {
		return record.Record{}, false
	}

	name := entry.AttributionSkill
	kind := record.KindSkill
	invoker := record.InvokerUser
	if name == "" {
		name = entry.AttributionAgent
		kind = record.KindSubagent
		invoker = record.InvokerModel
	}
	primitive, err := names.DerivedName(name)
	if err != nil {
		return record.Record{}, false
	}
	sessionID, err := record.BoundedToken(entry.SessionID)
	if err != nil {
		return record.Record{}, false
	}
	repo, consented := resolve(entry.CWD)
	if !consented {
		return record.Record{}, false
	}

	event := record.Record{
		SchemaVersion: record.SchemaVersion,
		EventID:       record.DeriveEventID(harness, record.Identifier(entry.UUID)),
		Timestamp:     record.NormalizedTimestamp(entry.Timestamp),
		Harness:       harness,
		SessionID:     sessionID,
		Repo:          repo,
		Kind:          kind,
		Name:          primitive,
		Invoker:       invoker,
	}
	if version, err := record.BoundedVersion(entry.Version); err == nil {
		event.HarnessVersion = version
	}
	if model, err := record.BoundedIdentifier(entry.Message.Model); err == nil {
		event.Model = model
	}
	return event, true
}

func primitiveName(block contentBlock, names record.Namer) (record.Identifier, error) {
	if block.Name == "Skill" && block.Input.Skill != "" {
		return names.DerivedName(block.Input.Skill)
	}
	return record.BoundedIdentifier(block.Name)
}

func (call call) complete(entry transcriptEntry, block contentBlock) (record.Record, bool) {
	outcome := outcomeFor(entry, block)
	return record.Record{
		SchemaVersion:  record.SchemaVersion,
		EventID:        call.eventID,
		Timestamp:      record.NormalizedTimestamp(entry.Timestamp),
		Harness:        harness,
		HarnessVersion: call.version,
		SessionID:      call.sessionID,
		Repo:           call.repo,
		Kind:           call.kind,
		Name:           call.name,
		Package:        call.packageName,
		ViaSkill:       call.viaSkill,
		ViaAgent:       call.viaAgent,
		Model:          call.model,
		Invoker:        call.invoker,
		Outcome:        outcome,
	}, true
}

func kindFor(name record.Identifier) record.Kind {
	switch name {
	case "Skill":
		return record.KindSkill
	case "Task":
		return record.KindSubagent
	}
	if len(name) > 5 && string(name[:5]) == "mcp__" {
		return record.KindMCPTool
	}
	return record.KindBuiltinTool
}

func packageFromAttribution(value string) (record.Identifier, bool) {
	const prefix = "plugin:"
	if len(value) <= len(prefix) || value[:len(prefix)] != prefix {
		return "", false
	}
	for index := len(prefix); index < len(value); index++ {
		if value[index] == ':' {
			// The name domain, not the derived one: an MCP server package is never
			// directory-scoped, so a segment carrying a separator is a hostile value
			// rather than a scope to digest.
			packageName, err := record.BoundedIdentifier(value[len(prefix):index])
			return packageName, err == nil
		}
	}
	return "", false
}

func outcomeFor(entry transcriptEntry, block contentBlock) *record.Outcome {
	switch entry.ToolDenialKind {
	case "permission-rule":
		outcome := record.OutcomeDeniedPolicy
		return &outcome
	case "user-rejected":
		outcome := record.OutcomeDeniedUser
		return &outcome
	}
	if entry.ToolUseResult.Interrupted {
		outcome := record.OutcomeInterrupted
		return &outcome
	}
	if block.IsError == nil {
		return nil
	}
	if *block.IsError {
		outcome := record.OutcomeError
		return &outcome
	}
	outcome := record.OutcomeOK
	return &outcome
}
