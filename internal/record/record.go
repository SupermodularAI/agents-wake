// Package record defines the only data Wake is allowed to retain from a harness.
//
// The Record type is an allowlist: it intentionally has no free-text field for
// prompts, tool arguments, paths, or repository labels. Adapters must derive a
// Record before anything can reach the local store.
package record

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SchemaVersion identifies the on-disk record contract.
const SchemaVersion uint = 1

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+/-]{0,127}$`)
	versionPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+:-]{0,127}$`)
	hexPattern        = regexp.MustCompile(`^[a-f0-9]+$`)
)

// Identifier is a bounded, machine-readable name. It is not free text.
type Identifier string

// Version is a bounded harness or package version.
type Version string

// Hash is a lowercase SHA-256 digest, except RepoHash which is a truncated
// keyed digest defined by config.
type Hash string

// Kind classifies a primitive or session event.
type Kind string

const (
	KindSkill        Kind = "skill"
	KindSubagent     Kind = "subagent"
	KindMCPTool      Kind = "mcp_tool"
	KindMCPServer    Kind = "mcp_server"
	KindCommand      Kind = "command"
	KindPlugin       Kind = "plugin"
	KindBuiltinTool  Kind = "builtin_tool"
	KindHook         Kind = "hook"
	KindSessionStart Kind = "session_start"
	KindSessionEnd   Kind = "session_end"
)

// Source describes where an installed primitive came from.
type Source string

const (
	SourceMarketplace Source = "marketplace"
	SourceLocal       Source = "local"
	SourceBuiltin     Source = "builtin"
)

// Invoker identifies how a primitive call began.
type Invoker string

const (
	InvokerUser  Invoker = "user"
	InvokerModel Invoker = "model"
	InvokerAuto  Invoker = "auto"
)

// Outcome is nullable on Record. Nil means the harness did not report one.
type Outcome string

const (
	OutcomeOK           Outcome = "ok"
	OutcomeError        Outcome = "error"
	OutcomeDeniedPolicy Outcome = "denied_policy"
	OutcomeDeniedUser   Outcome = "denied_user"
	OutcomeTimeout      Outcome = "timeout"
	OutcomeInterrupted  Outcome = "interrupted"
	OutcomeNotFound     Outcome = "not_found"
	OutcomeBadArgs      Outcome = "bad_args"
)

// Record is a safe, derived terminal event. Its fields are identifiers, enums,
// hashes, timestamps, or numbers; it deliberately has no unbounded text.
type Record struct {
	SchemaVersion  uint       `json:"schema_version"`
	EventID        Hash       `json:"event_id"`
	Timestamp      time.Time  `json:"ts"`
	Harness        Identifier `json:"harness"`
	HarnessVersion Version    `json:"harness_version,omitempty"`
	SessionID      Identifier `json:"session_id"`
	Repo           Hash       `json:"repo"`
	Kind           Kind       `json:"kind"`
	Name           Identifier `json:"name"`
	Package        Identifier `json:"package,omitempty"`
	PackageVersion Version    `json:"package_version,omitempty"`
	Source         *Source    `json:"source"`
	ViaSkill       Identifier `json:"via_skill,omitempty"`
	ViaAgent       Identifier `json:"via_agent,omitempty"`
	Model          Identifier `json:"model,omitempty"`
	Effort         Identifier `json:"effort,omitempty"`
	Invoker        Invoker    `json:"invoker"`
	Outcome        *Outcome   `json:"outcome"`
	DurationMS     *int64     `json:"duration_ms"`
}

// DeriveEventID derives a stable identifier from the harness and its source
// event identity. The store never creates IDs at write time.
func DeriveEventID(harness, sourceEvent Identifier) Hash {
	sum := sha256.Sum256([]byte(string(harness) + "\x00" + string(sourceEvent)))
	return Hash(hex.EncodeToString(sum[:]))
}

// Marshal serializes a validated record in the stable field order declared by
// Record. Struct encoding avoids map-order variation in the NDJSON spool.
func Marshal(r Record) ([]byte, error) {
	if err := Validate(r); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// Validate checks the persisted privacy and format contract.
func Validate(r Record) error {
	if r.SchemaVersion != SchemaVersion {
		return errors.New("unsupported schema version")
	}
	if !validSHA256(r.EventID) {
		return errors.New("invalid event id")
	}
	if r.Timestamp.IsZero() {
		return errors.New("missing timestamp")
	}
	if !validIdentifier(r.Harness) || !validIdentifier(r.SessionID) || !validRepoHash(r.Repo) || !validKind(r.Kind) || !validIdentifier(r.Name) || !validInvoker(r.Invoker) {
		return errors.New("invalid required record field")
	}
	if !validOptionalIdentifier(r.Package) || !validOptionalIdentifier(r.ViaSkill) || !validOptionalIdentifier(r.ViaAgent) || !validOptionalIdentifier(r.Model) || !validOptionalIdentifier(r.Effort) || !validOptionalVersion(r.HarnessVersion) || !validOptionalVersion(r.PackageVersion) {
		return errors.New("invalid optional record field")
	}
	if r.Source != nil && !validSource(*r.Source) {
		return errors.New("invalid source")
	}
	if r.Outcome != nil && !validOutcome(*r.Outcome) {
		return errors.New("invalid outcome")
	}
	if r.DurationMS != nil && *r.DurationMS < 0 {
		return errors.New("invalid duration")
	}
	return nil
}

func validIdentifier(v Identifier) bool { return identifierPattern.MatchString(string(v)) }

func validOptionalIdentifier(v Identifier) bool { return v == "" || validIdentifier(v) }

func validOptionalVersion(v Version) bool { return v == "" || versionPattern.MatchString(string(v)) }

func validSHA256(v Hash) bool { return len(v) == sha256.Size*2 && hexPattern.MatchString(string(v)) }

func validRepoHash(v Hash) bool { return len(v) == 32 && hexPattern.MatchString(string(v)) }

func validKind(v Kind) bool {
	switch v {
	case KindSkill, KindSubagent, KindMCPTool, KindMCPServer, KindCommand, KindPlugin, KindBuiltinTool, KindHook, KindSessionStart, KindSessionEnd:
		return true
	default:
		return false
	}
}

func validSource(v Source) bool {
	return v == SourceMarketplace || v == SourceLocal || v == SourceBuiltin
}

func validInvoker(v Invoker) bool {
	return v == InvokerUser || v == InvokerModel || v == InvokerAuto
}

func validOutcome(v Outcome) bool {
	switch v {
	case OutcomeOK, OutcomeError, OutcomeDeniedPolicy, OutcomeDeniedUser, OutcomeTimeout, OutcomeInterrupted, OutcomeNotFound, OutcomeBadArgs:
		return true
	default:
		return false
	}
}

// Decode validates one persisted line. It returns only a generic error so raw
// line content cannot leak through diagnostics.
func Decode(line []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(line, &r); err != nil {
		return Record{}, errors.New("invalid record encoding")
	}
	if err := Validate(r); err != nil {
		return Record{}, fmt.Errorf("invalid record: %w", err)
	}
	return r, nil
}

// IsFailure is the shared health classification. Permission denials are known
// outcomes but not tool failures.
func IsFailure(outcome Outcome) bool {
	return outcome == OutcomeError || outcome == OutcomeTimeout || outcome == OutcomeInterrupted || outcome == OutcomeNotFound || outcome == OutcomeBadArgs
}

// IsTerminal reports whether a record is suitable for the invocation store.
func IsTerminal(r Record) bool {
	return r.Kind != KindSessionStart
}

// NormalizedTimestamp strips monotonic clock data before persistence.
func NormalizedTimestamp(t time.Time) time.Time { return t.UTC().Round(0) }

// BoundedIdentifier returns an Identifier only when it fits the record contract.
func BoundedIdentifier(value string) (Identifier, error) {
	value = strings.TrimSpace(value)
	identifier := Identifier(value)
	if !validIdentifier(identifier) {
		return "", errors.New("invalid identifier")
	}
	return identifier, nil
}
